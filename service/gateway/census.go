package gateway

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/OSMeteor/firetower/grpc/manager"
	"github.com/OSMeteor/firetower/socket"
)

// 本机订阅人数是重放的真相。版本号只增不减，Manager 用它丢掉旧请求。
type topicCensus struct {
	n   int64
	ver uint64
}

var (
	censusMu       sync.Mutex
	census         = map[string]topicCensus{}
	censusEpoch    uint64
	replayRound    atomic.Uint64
	censusInterval = 3 * time.Minute
	periodicOnce   sync.Once
	startupOnce    sync.Once
	startupRandom  string
	replaySlots    sync.Map
)

type replaySlot struct {
	mu    sync.Mutex
	dirty atomic.Bool
	// 连续失败的次数。重放失败后按它退避重试，成功后清零。
	fails atomic.Int32
	timer atomic.Bool
}

// replayRPCTimeout 后台重放每一块的超时。
var replayRPCTimeout = 5 * time.Second

// 重放失败后的重试间隔。Manager 刚拉起时 gRPC 通道可能还没连上，TCP 却已经通了，
// 这时第一次重放会立刻失败。不重试的话，这台 Manager 要等到下一轮 3 分钟对账才有这台 Gateway 的路由。
var defaultReplayRetryDelays = []time.Duration{
	200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond,
	1500 * time.Millisecond, 3 * time.Second, 3 * time.Second, 5 * time.Second, 5 * time.Second,
	10 * time.Second, 10 * time.Second,
}

var replayDelays atomic.Pointer[[]time.Duration]

func retryDelays() []time.Duration {
	if d := replayDelays.Load(); d != nil && len(*d) > 0 {
		return *d
	}
	return defaultReplayRetryDelays
}

// setRetryDelays 只给测试用，返回原来的值。
func setRetryDelays(d []time.Duration) []time.Duration {
	prev := retryDelays()
	replayDelays.Store(&d)
	return prev
}

func attachReplay() {
	manage := loadTopicManage()
	if manage == nil {
		return
	}
	manage.SetOnConnect(onManagerConnected)
}

func nextCensusVersion() uint64 {
	censusEpoch++
	return censusEpoch
}

func noteSubscribe(topics []string) (counts []int64, versions []uint64) {
	censusMu.Lock()
	defer censusMu.Unlock()
	for _, topic := range topics {
		st := census[topic]
		st.n++
		st.ver = nextCensusVersion()
		census[topic] = st
		counts = append(counts, st.n)
		versions = append(versions, st.ver)
	}
	return counts, versions
}

// revertSubscribe 撤掉一次没有落地的订阅，返回撤销之后的人数和版本。
// 调用方要把它们再发给 Manager：登记可能已经写到一部分 Manager 上了，不盖回去的话人数会多到下一轮对账。
func revertSubscribe(topics []string) (counts []int64, versions []uint64) {
	censusMu.Lock()
	defer censusMu.Unlock()
	for _, topic := range topics {
		st := census[topic]
		if st.n > 0 {
			st.n--
		}
		ver := nextCensusVersion()
		if st.n <= 0 {
			delete(census, topic)
		} else {
			st.ver = ver
			census[topic] = st
		}
		counts = append(counts, st.n)
		versions = append(versions, ver)
	}
	return counts, versions
}

func noteUnsubscribe(topics []string) (counts []int64, versions []uint64) {
	censusMu.Lock()
	defer censusMu.Unlock()
	for _, topic := range topics {
		st := census[topic]
		if st.n > 0 {
			st.n--
		}
		ver := nextCensusVersion()
		if st.n <= 0 {
			delete(census, topic)
		} else {
			st.ver = ver
			census[topic] = st
		}
		counts = append(counts, st.n)
		versions = append(versions, ver)
	}
	return counts, versions
}

func startPeriodicReplay() {
	periodicOnce.Do(func() {
		go func() {
			timer := time.NewTimer(censusInterval)
			defer timer.Stop()
			for range timer.C {
				func() {
					defer func() {
						if err := recover(); err != nil {
							fmt.Printf("PANIC: periodic replay recovered: %v\n", err)
						}
					}()
					replayLocalTopics()
				}()
				timer.Reset(censusInterval)
			}
		}()
	})
}

func onManagerConnected() {
	announceRoute()
	replayLocalTopics()
}

func announceRoute() {
	announceRouteClient(loadTopicManage())
}

func announceRouteClient(client *socket.TcpClient) {
	if client == nil {
		return
	}
	gen := client.Generation()
	key := routeIdentity(gen)
	frame, err := socket.Enpack("handshake", "0", "gateway", socket.HandshakeTopic, []byte(key))
	if err != nil || client.SendPacked(frame) != nil {
		return
	}
	// 等待期间可能已经换了一代连接。确认到的是新连接时，不能把上一代的键写回去。
	if client.WaitHandshake(500 * time.Millisecond) {
		if client.Generation() == gen {
			client.SetRouteKey(key)
		}
		return
	}
	// 500ms 内没等到确认，调用方先按本地地址登记（老版本 Manager 不会确认）。
	// 但确认也可能只是来晚了：Manager 那边已经把这条连接换成了路由键，按地址登记的条目它找不到连接，
	// 中间有地址转换时这台 Gateway 会一条消息都收不到。所以继续等，等到了就改用路由键并整表重放。
	go adoptLateHandshake(client, gen, key)
}

func adoptLateHandshake(client *socket.TcpClient, gen uint64, key string) {
	defer func() {
		if err := recover(); err != nil {
			fmt.Printf("PANIC: late handshake recovered: %v\n", err)
		}
	}()
	for i := 0; i < 60; i++ {
		if client.Generation() != gen {
			return
		}
		if !client.WaitHandshake(time.Second) {
			continue
		}
		if client.Generation() != gen {
			return
		}
		old := client.CurrentAddr()
		client.SetRouteKey(key)
		callTowerLogger(nil, "WARN", "manager handshake was acknowledged late, switching to the route key and replaying")
		for _, link := range activeLinks() {
			if link == nil || link.tcp != client {
				continue
			}
			// 先把按地址登记的那一份清掉：发一轮空的对账，Manager 会删掉这个键下的全部条目。
			if old != "" && link.rpc != nil {
				_, _, _, epoch := censusSnapshot()
				_ = sendReplay(link.rpc, old, replayRound.Add(1), true, true, []string{socket.EpochTopic}, []int64{0}, []uint64{epoch})
			}
			replayLink(link)
		}
		return
	}
}

func routeIdentity(generation uint64) string {
	startupOnce.Do(func() {
		var buf [8]byte
		if _, err := rand.Read(buf[:]); err != nil {
			binary.BigEndian.PutUint64(buf[:], uint64(time.Now().UnixNano()))
		}
		startupRandom = hex.EncodeToString(buf[:])
	})
	return fmt.Sprintf("%d-%s-%d", ClusterId, startupRandom, generation)
}

func replayLocalTopics() {
	for _, link := range reachableRegisters() {
		replayLink(link)
	}
}

func replaySlotFor(client *socket.TcpClient) *replaySlot {
	if client == nil {
		return &replaySlot{}
	}
	if value, ok := replaySlots.Load(client); ok {
		return value.(*replaySlot)
	}
	slot := &replaySlot{}
	actual, _ := replaySlots.LoadOrStore(client, slot)
	return actual.(*replaySlot)
}

func replayLink(link *managerLink) {
	if link == nil || link.rpc == nil || link.tcp == nil {
		return
	}
	slot := replaySlotFor(link.tcp)
	// 先登记「需要重放」，再抢锁。抢不到说明有人正在重放，它放锁之后会再看一眼这个标记，
	// 所以不会出现「标记刚写上、持锁的人已经决定退出」而漏掉一轮的情况。
	slot.dirty.Store(true)
	for slot.dirty.Load() {
		if !slot.mu.TryLock() {
			return
		}
		failed := replayWhileDirty(link, slot)
		if failed {
			retryReplayLater(link, slot)
			return
		}
		slot.fails.Store(0)
	}
}

// replayWhileDirty 持有 slot.mu 期间重放，直到没有新的请求或者失败。
// 解锁放在 defer 里：重放中途 panic 时外层的 recover 会接住，锁不能一直留着，否则这条链路再也不会重放。
func replayWhileDirty(link *managerLink, slot *replaySlot) (failed bool) {
	defer slot.mu.Unlock()
	for slot.dirty.Swap(false) {
		if err := replayOnce(link); err != nil {
			return true
		}
	}
	return false
}

// linkInPool 这条链路还在连接池里（按 TCP 客户端比对，只有一台 Manager 时池里的项是临时拼出来的）。
func linkInPool(link *managerLink) bool {
	if link == nil || link.tcp == nil {
		return false
	}
	for _, l := range activeLinks() {
		if l != nil && l.tcp == link.tcp {
			return true
		}
	}
	return false
}

// replayLinkSoon 过一会儿在后台把整表重放给这台 Manager，用来纠正一次没写进去的登记。不阻塞调用方。
func replayLinkSoon(link *managerLink) {
	if link == nil || link.tcp == nil {
		return
	}
	slot := replaySlotFor(link.tcp)
	slot.dirty.Store(true)
	retryReplayLater(link, slot)
}

// retryReplayLater 在这条链路还连着的前提下，过一会儿再重放。同一时刻只挂一个定时器。
func retryReplayLater(link *managerLink, slot *replaySlot) {
	if link == nil || slot == nil || !link.connected() || !linkInPool(link) {
		// 链路断了就不用重试，重连时会再重放。
		slot.fails.Store(0)
		return
	}
	n := int(slot.fails.Add(1))
	// 一直重试到成功为止，间隔用完就停在最后一档。链路断了、或者已经不在连接池里时上面就退出了，
	// 重连时本来就会整表重放。
	delays := retryDelays()
	delay := delays[len(delays)-1]
	if n <= len(delays) {
		delay = delays[n-1]
	}
	if n == len(delays) {
		callTowerLogger(nil, "WARN", fmt.Sprintf("replay to manager %s keeps failing, retrying every %s", link.name, delay))
	}
	if !slot.timer.CompareAndSwap(false, true) {
		return
	}
	time.AfterFunc(delay, func() {
		slot.timer.Store(false)
		defer func() {
			if err := recover(); err != nil {
				fmt.Printf("PANIC: replay retry recovered: %v\n", err)
			}
		}()
		replayLink(link)
	})
}

func censusSnapshot() (topics []string, counts []int64, versions []uint64, epoch uint64) {
	censusMu.Lock()
	defer censusMu.Unlock()
	epoch = censusEpoch
	for topic, st := range census {
		if topic == "" || st.n <= 0 || st.ver == 0 {
			continue
		}
		topics = append(topics, topic)
		counts = append(counts, st.n)
		versions = append(versions, st.ver)
	}
	return topics, counts, versions, epoch
}

// replayOnce 把本机整张表发给一台 Manager。链路没连上时返回 nil：重连时会再来。
// 发到一半失败返回错误，由调用方安排重试。
func replayOnce(link *managerLink) error {
	ip, err := link.endpoint()
	if err != nil {
		return nil
	}
	topics, counts, versions, epoch := censusSnapshot()
	round := replayRound.Add(1)
	const chunk = 256
	if len(topics) == 0 {
		return sendReplay(link.rpc, ip, round, true, true, []string{socket.EpochTopic}, []int64{0}, []uint64{epoch})
	}
	for i := 0; i < len(topics); i += chunk {
		j := i + chunk
		if j > len(topics) {
			j = len(topics)
		}
		if err := sendReplay(link.rpc, ip, round, i == 0, false, topics[i:j], counts[i:j], versions[i:j]); err != nil {
			return err
		}
	}
	return sendReplay(link.rpc, ip, round, false, true, []string{socket.EpochTopic}, []int64{0}, []uint64{epoch})
}

func sendReplay(client pb.TopicServiceClient, ip string, round uint64, begin, end bool, topics []string, counts []int64, versions []uint64) error {
	if client == nil {
		return fmt.Errorf("nil replay client")
	}
	// 重放在后台跑，不卡任何连接，超时放宽。Manager 刚重启、几十台 Gateway 同时重放时登记要排队。
	ctx, cancel := context.WithTimeout(context.Background(), replayRPCTimeout)
	defer cancel()
	_, err := client.SubscribeTopic(ctx, &pb.SubscribeTopicRequest{
		Ip:         ip,
		Topic:      append([]string(nil), topics...),
		Count:      append([]int64(nil), counts...),
		Version:    append([]uint64(nil), versions...),
		Round:      round,
		RoundBegin: begin,
		RoundEnd:   end,
	})
	return err
}
