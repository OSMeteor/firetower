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

func revertSubscribe(topics []string) {
	censusMu.Lock()
	defer censusMu.Unlock()
	for _, topic := range topics {
		st, ok := census[topic]
		if !ok {
			continue
		}
		if st.n > 0 {
			st.n--
		}
		ver := nextCensusVersion()
		if st.n <= 0 {
			delete(census, topic)
			continue
		}
		st.ver = ver
		census[topic] = st
	}
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
	if client.WaitHandshake(500*time.Millisecond) && client.Generation() == gen {
		client.SetRouteKey(key)
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
	if !slot.mu.TryLock() {
		slot.dirty.Store(true)
		return
	}
	defer slot.mu.Unlock()
	for {
		slot.dirty.Store(false)
		replayOnce(link)
		if !slot.dirty.Load() {
			return
		}
	}
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

func replayOnce(link *managerLink) {
	ip, err := link.endpoint()
	if err != nil {
		return
	}
	topics, counts, versions, epoch := censusSnapshot()
	round := replayRound.Add(1)
	const chunk = 256
	if len(topics) == 0 {
		_ = sendReplay(link.rpc, ip, round, true, true, []string{socket.EpochTopic}, []int64{0}, []uint64{epoch})
		return
	}
	for i := 0; i < len(topics); i += chunk {
		j := i + chunk
		if j > len(topics) {
			j = len(topics)
		}
		if err := sendReplay(link.rpc, ip, round, i == 0, false, topics[i:j], counts[i:j], versions[i:j]); err != nil {
			return
		}
	}
	_ = sendReplay(link.rpc, ip, round, false, true, []string{socket.EpochTopic}, []int64{0}, []uint64{epoch})
}

func sendReplay(client pb.TopicServiceClient, ip string, round uint64, begin, end bool, topics []string, counts []int64, versions []uint64) error {
	if client == nil {
		return fmt.Errorf("nil replay client")
	}
	ctx, cancel := context.WithTimeout(context.Background(), managerRPCTimeout)
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
