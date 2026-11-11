package manager

import (
	"container/list"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"net/http"

	"sort"

	"encoding/json"

	pb "github.com/OSMeteor/firetower/grpc/manager"
	"github.com/OSMeteor/firetower/socket"

	"github.com/pkg/errors"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Manager topic管理中心结构体
// 为了绑定一些专属的操作方法 所以创建了一个空的结构体
type Manager struct{}

var (
	// HttpAddress http服务监听端口配置
	HttpAddress    = ":8000"
	topicRelevance sync.Map
	// ConnIndexTable 连接关系索引表
	ConnIndexTable sync.Map

	// Logger 接管系统log t log类型 info log信息
	Logger func(types, info string) = log
	// LogLevel 日志打印级别
	LogLevel = "INFO"
	// DefaultWriter 正常日志的默认写入方式
	DefaultWriter io.Writer = os.Stdout
	// DefaultErrorWriter 错误日志的默认写入方式
	DefaultErrorWriter io.Writer = os.Stderr
)

var loggerMu sync.RWMutex

func logManager(level, info string) {
	loggerMu.RLock()
	fn := Logger
	loggerMu.RUnlock()
	if fn != nil {
		// Logger 可以被业务替换，recover 里也会调用它。它自己 panic 时不能再往外抛。
		defer func() {
			if err := recover(); err != nil {
				fmt.Printf("PANIC: manager logger recovered: %v\n", err)
			}
		}()
		fn(level, info)
	}
}

func setLogger(fn func(string, string)) {
	loggerMu.Lock()
	Logger = fn
	loggerMu.Unlock()
}

func currentLogger() func(string, string) {
	loggerMu.RLock()
	defer loggerMu.RUnlock()
	return Logger
}

type topicRelevanceItem struct {
	ip   string
	num  int64
	conn net.Conn
}

type topicGrpcService struct{}

// topicMu 保护 topicRelevance 里的链表。sync.Map 只保护表本身，链表节点的 next/list 必须同一把锁。
var (
	topicMu    sync.RWMutex
	subVersion = map[string]map[string]uint64{}
)

func topicVersion(ip, topic string) uint64 {
	return subVersion[ip][topic]
}

func storeVersion(ip, topic string, ver uint64) {
	byIP := subVersion[ip]
	if byIP == nil {
		byIP = map[string]uint64{}
		subVersion[ip] = byIP
	}
	byIP[topic] = ver
}

func recoverTopic(name string) {
	if err := recover(); err != nil {
		logManager("PANIC", fmt.Sprintf("%s panic: %v", name, err))
	}
}

func asTopicList(v any) *list.List {
	if v == nil {
		return nil
	}
	l, ok := v.(*list.List)
	if !ok {
		return nil
	}
	return l
}

func asItem(e *list.Element) *topicRelevanceItem {
	if e == nil || e.Value == nil {
		return nil
	}
	item, ok := e.Value.(*topicRelevanceItem)
	if !ok {
		return nil
	}
	return item
}

// targetsLocked 在持有 topicMu 时把当前链表上的连接抄出来。调用方放开锁之后再写网络。
func targetsLocked(l *list.List) []*connectBucket {
	if l == nil {
		return nil
	}
	var out []*connectBucket
	for e := l.Front(); e != nil; e = e.Next() {
		item := asItem(e)
		if item == nil || item.ip == "" {
			continue
		}
		raw, ok := ConnIndexTable.Load(item.ip)
		if !ok || raw == nil {
			continue
		}
		bucket, ok := raw.(*connectBucket)
		if !ok || bucket == nil || bucket.conn == nil {
			continue
		}
		out = append(out, bucket)
	}
	return out
}

// Publish 推送的grpc接口
// request *pb.PublishRequest
// 接收 Topic 话题
//
//	Data  传输内容
//	MessageId gateway 来源的消息id
func (t *topicGrpcService) Publish(ctx context.Context, request *pb.PublishRequest) (*pb.PublishResponse, error) {
	defer recoverTopic("Publish")
	if t == nil || request == nil || request.Topic == "" {
		return &pb.PublishResponse{Ok: false}, errors.New("empty request")
	}
	if socket.NoteHot() {
		logManager("INFO", fmt.Sprintf("new message: %s", string(request.Data)))
	}

	targets, ok := snapshotTargets(request.Topic)
	if !ok {
		return &pb.PublishResponse{Ok: false}, errors.New("topic not exist")
	}

	b, err := socket.Enpack(socket.PublishKey, request.MessageId, request.Source, request.Topic, request.Data)
	if err != nil {
		return &pb.PublishResponse{Ok: false}, err
	}
	for _, bucket := range targets {
		if bucket == nil || bucket.conn == nil {
			continue
		}
		bucket.deliver(b)
	}
	return &pb.PublishResponse{Ok: true}, nil
}

// CheckTopicExist 检测topic是否已经存在订阅关系
func (t *topicGrpcService) CheckTopicExist(ctx context.Context, request *pb.CheckTopicExistRequest) (*pb.CheckTopicExistResponse, error) {
	defer recoverTopic("CheckTopicExist")
	if request == nil || request.Topic == "" {
		return &pb.CheckTopicExistResponse{Ok: false}, nil
	}
	_, ok := topicRelevance.Load(request.Topic)
	if !ok {
		topicMu.RLock()
		grouped := len(groupListsLocked(request.Topic)) > 0
		topicMu.RUnlock()
		if grouped {
			return &pb.CheckTopicExistResponse{Ok: true}, nil
		}
		// topic 没有存在订阅列表中直接过滤
		return &pb.CheckTopicExistResponse{Ok: false}, nil
	}
	return &pb.CheckTopicExistResponse{Ok: true}, nil
}

// GetConnectNum 获取topic订阅数的grpc接口
func (t *topicGrpcService) GetConnectNum(ctx context.Context, request *pb.GetConnectNumRequest) (*pb.GetConnectNumResponse, error) {
	defer recoverTopic("GetConnectNum")
	if request == nil || request.Topic == "" {
		return &pb.GetConnectNumResponse{Number: 0}, nil
	}
	topicMu.RLock()
	defer topicMu.RUnlock()
	value, ok := topicRelevance.Load(request.Topic)
	var num int64
	if ok {
		num = getConnectNum(asTopicList(value))
	} else {
		for _, l := range groupListsLocked(request.Topic) {
			num += getConnectNum(l)
		}
	}
	return &pb.GetConnectNumResponse{Number: num}, nil
}

// 房间按用户分片后（Gateway 的 SetTopicShards / [[group]]），Manager 上只有片键「房间名 + 分隔符 + 片号」。
// 业务后端和管理命令仍然只知道房间名，所以按房间名找不到时在这里展开成各片。
const (
	groupSep       = "\x1f"
	maxGroupShards = 256
)

// groupListsLocked 返回这个房间名下各片的订阅表。没有分片或本身就是片键时返回空。调用方持有 topicMu。
func groupListsLocked(topic string) []*list.List {
	if topic == "" || strings.Contains(topic, groupSep) {
		return nil
	}
	var out []*list.List
	for i := 0; i < maxGroupShards; i++ {
		value, ok := topicRelevance.Load(topic + groupSep + strconv.Itoa(i))
		if !ok {
			continue
		}
		if l := asTopicList(value); l != nil {
			out = append(out, l)
		}
	}
	return out
}

var lastMixedWarn atomic.Int64

// noteMixedGroup 同一个房间既有整间登记又有分片登记，说明各 Gateway 的 [[group]] 配置不一致。
// 这时按房间名的发布只会到整间那一部分。不改行为，只告警，每 10 秒最多一条。调用方持有 topicMu。
func noteMixedGroup(topic string) {
	parent := topic
	if i := strings.Index(topic, groupSep); i >= 0 {
		parent = topic[:i]
		if _, whole := topicRelevance.Load(parent); !whole {
			return
		}
	} else if len(groupListsLocked(topic)) == 0 {
		return
	}
	now := time.Now().UnixNano()
	prev := lastMixedWarn.Load()
	if now-prev < int64(10*time.Second) || !lastMixedWarn.CompareAndSwap(prev, now) {
		return
	}
	logManager("WARN", fmt.Sprintf("topic %q is registered both whole and in shards; check that every gateway has the same [[group]] config", parent))
}

func snapshotTargets(topic string) ([]*connectBucket, bool) {
	topicMu.RLock()
	defer topicMu.RUnlock()
	value, ok := topicRelevance.Load(topic)
	if !ok {
		// 一台 Gateway 只收一帧，它自己再分给本机的各片。
		lists := groupListsLocked(topic)
		if len(lists) == 0 {
			return nil, false
		}
		seen := map[*connectBucket]struct{}{}
		var out []*connectBucket
		for _, l := range lists {
			for _, bucket := range targetsLocked(l) {
				if _, dup := seen[bucket]; dup {
					continue
				}
				seen[bucket] = struct{}{}
				out = append(out, bucket)
			}
		}
		return out, true
	}
	table := asTopicList(value)
	if table == nil {
		return nil, false
	}
	return targetsLocked(table), true
}

func getConnectNum(l *list.List) int64 {
	if l == nil {
		return 0
	}
	var num int64
	for e := l.Front(); e != nil; e = e.Next() {
		item := asItem(e)
		if item == nil {
			continue
		}
		num += item.num
	}
	return num
}

// SubscribeTopic 订阅topic的grpc接口
func (t *topicGrpcService) SubscribeTopic(ctx context.Context, request *pb.SubscribeTopicRequest) (*pb.SubscribeTopicResponse, error) {
	defer recoverTopic("SubscribeTopic")
	if t == nil || request == nil {
		return &pb.SubscribeTopicResponse{}, errors.New("empty request")
	}
	if len(request.Topic) > maxTopicsPerRequest {
		return &pb.SubscribeTopicResponse{}, errors.New("too many topics")
	}
	if len(request.Version) > 0 && (len(request.Version) != len(request.Topic) || len(request.Count) != len(request.Topic)) {
		return &pb.SubscribeTopicResponse{}, errors.New("count and version must match topics")
	}
	if err := enterRegister(ctx); err != nil {
		return &pb.SubscribeTopicResponse{}, err
	}
	defer leaveRegister()
	topicMu.Lock()
	defer topicMu.Unlock()
	ip, ok := resolveKeyLocked(request.Ip)
	if !ok {
		// 这条连接已经断开。Gateway 重连后会按新的键整表重放。
		return &pb.SubscribeTopicResponse{}, nil
	}
	if request.Round > 0 && request.RoundBegin {
		openReconcile(ip, request.Round)
	}
	for i, topic := range request.Topic {
		if topic == "" || ip == "" || topic == socket.EpochTopic {
			if topic == socket.EpochTopic && len(request.Version) > i {
				noteEpoch(ip, request.Version[i])
			}
			continue
		}
		markReconcile(ip, topic)
		noteMixedGroup(topic)
		if len(request.Version) > 0 && request.Version[i] > 0 {
			applyAbsolute(topic, ip, request.Count[i], request.Version[i])
			continue
		}
		bumpTopic(topic, ip)
	}
	if request.Round > 0 && request.RoundEnd {
		finishReconcile(ip, request.Round)
	}
	return &pb.SubscribeTopicResponse{}, nil
}

// UnSubscribeTopic 取消订阅topic的grpc接口
func (t *topicGrpcService) UnSubscribeTopic(ctx context.Context, request *pb.UnSubscribeTopicRequest) (*pb.UnSubscribeTopicResponse, error) {
	defer recoverTopic("UnSubscribeTopic")
	if t == nil || request == nil {
		return &pb.UnSubscribeTopicResponse{}, errors.New("empty request")
	}
	if len(request.Topic) > maxTopicsPerRequest {
		return &pb.UnSubscribeTopicResponse{}, errors.New("too many topics")
	}
	if len(request.Version) > 0 && (len(request.Version) != len(request.Topic) || len(request.Count) != len(request.Topic)) {
		return &pb.UnSubscribeTopicResponse{}, errors.New("count and version must match topics")
	}
	if err := enterRegister(ctx); err != nil {
		return &pb.UnSubscribeTopicResponse{}, err
	}
	defer leaveRegister()
	topicMu.Lock()
	defer topicMu.Unlock()
	ip, ok := resolveKeyLocked(request.Ip)
	if !ok {
		return &pb.UnSubscribeTopicResponse{}, nil
	}
	for i, topic := range request.Topic {
		if topic == "" || ip == "" {
			continue
		}
		if len(request.Version) > 0 && request.Version[i] > 0 {
			applyAbsolute(topic, ip, request.Count[i], request.Version[i])
			continue
		}
		dropOne(topic, ip)
	}
	return &pb.UnSubscribeTopicResponse{}, nil
}

// 登记是 gRPC，连接是 TCP，两条路到达 Manager 的先后没有保证。下面两张表处理两种晚到的登记：
//   - keyAlias：握手确认前 Gateway 用本地地址登记。这样的请求可能在 rebind 之后才到，
//     这时地址键已经没有连接。把它记到这条连接现在的路由键上，否则这些人数会一直留着。
//   - deadKeys：连接已经断开，它的键下面的登记都清掉了。之后才到的登记不能再写回去，
//     路由键每次重连都会变，没有连接会再来认领它。
//
// 两张表都由 topicMu 保护。
var (
	keyAlias = map[string]string{}
	deadKeys = map[string]int64{}
)

const (
	maxDeadKeys = 8192
	deadKeyTTL  = 10 * time.Minute
)

// resolveKeyLocked 返回这次登记应当写到哪个键下。第二个返回值为假表示这个键属于已断开的连接，登记要丢掉。
func resolveKeyLocked(ip string) (string, bool) {
	if ip == "" {
		return ip, true
	}
	if to, ok := keyAlias[ip]; ok && to != "" {
		ip = to
	}
	if _, dead := deadKeys[ip]; dead {
		return "", false
	}
	return ip, true
}

func buryKeyLocked(key string) {
	if key == "" {
		return
	}
	now := time.Now().UnixNano()
	if len(deadKeys) >= maxDeadKeys {
		for k, at := range deadKeys {
			if now-at > int64(deadKeyTTL) {
				delete(deadKeys, k)
			}
		}
		if len(deadKeys) >= maxDeadKeys {
			deadKeys = map[string]int64{}
		}
	}
	deadKeys[key] = now
	delete(keyAlias, key)
	for from, to := range keyAlias {
		if to == key {
			delete(keyAlias, from)
		}
	}
}

// claimKeyLocked 在一条新连接开始使用某个键时调用。
func claimKeyLocked(key string) {
	if key == "" {
		return
	}
	delete(deadKeys, key)
	delete(keyAlias, key)
}

func bumpTopic(topic, ip string) {
	store := topicList(topic)
	for e := store.Front(); e != nil; e = e.Next() {
		item := asItem(e)
		if item != nil && item.ip == ip {
			item.num++
			return
		}
	}
	store.PushBack(&topicRelevanceItem{ip: ip, num: 1})
}

func dropOne(topic, ip string) {
	value, ok := topicRelevance.Load(topic)
	if !ok {
		return
	}
	store := asTopicList(value)
	if store == nil {
		return
	}
	for e := store.Front(); e != nil; e = e.Next() {
		item := asItem(e)
		if item == nil || item.ip != ip {
			continue
		}
		if item.num-1 <= 0 {
			store.Remove(e)
			if store.Len() == 0 {
				topicRelevance.Delete(topic)
			}
		} else {
			item.num--
		}
		return
	}
}

func applyAbsolute(topic, ip string, count int64, ver uint64) {
	if ver == 0 {
		return
	}
	if prev := topicVersion(ip, topic); prev > ver {
		return
	}
	storeVersion(ip, topic, ver)
	store := topicList(topic)
	var found *list.Element
	for e := store.Front(); e != nil; e = e.Next() {
		item := asItem(e)
		if item != nil && item.ip == ip {
			found = e
			break
		}
	}
	if count <= 0 {
		if found != nil {
			store.Remove(found)
		}
		if store.Len() == 0 {
			topicRelevance.Delete(topic)
		}
		return
	}
	if found == nil {
		store.PushBack(&topicRelevanceItem{ip: ip, num: count})
		return
	}
	if item := asItem(found); item != nil {
		item.num = count
	}
}

type reconcileRound struct {
	round     uint64
	watermark uint64
	seen      map[string]struct{}
}

var openRounds = map[string]*reconcileRound{}

func openReconcile(ip string, round uint64) {
	if ip == "" || round == 0 {
		return
	}
	openRounds[ip] = &reconcileRound{round: round, seen: map[string]struct{}{}}
}

func markReconcile(ip, topic string) {
	round := openRounds[ip]
	if round == nil || topic == "" {
		return
	}
	round.seen[topic] = struct{}{}
}

func noteEpoch(ip string, ver uint64) {
	round := openRounds[ip]
	if round == nil || ver <= round.watermark {
		return
	}
	round.watermark = ver
}

func finishReconcile(ip string, round uint64) {
	current := openRounds[ip]
	if current == nil || current.round != round {
		return
	}
	topicRelevance.Range(func(key, value any) bool {
		topic, _ := key.(string)
		if _, ok := current.seen[topic]; ok {
			return true
		}
		if current.watermark > 0 && topicVersion(ip, topic) > current.watermark {
			return true
		}
		store := asTopicList(value)
		if store == nil {
			return true
		}
		for e := store.Front(); e != nil; {
			next := e.Next()
			item := asItem(e)
			if item != nil && item.ip == ip {
				store.Remove(e)
			}
			e = next
		}
		if store.Len() == 0 {
			topicRelevance.Delete(topic)
		}
		return true
	})
	if byIP := subVersion[ip]; byIP != nil {
		for topic, ver := range byIP {
			if _, ok := current.seen[topic]; ok {
				continue
			}
			if current.watermark > 0 && ver > current.watermark {
				continue
			}
			delete(byIP, topic)
		}
		if len(byIP) == 0 {
			delete(subVersion, ip)
		}
	}
	delete(openRounds, ip)
}

func topicList(topic string) *list.List {
	value, ok := topicRelevance.Load(topic)
	if ok {
		if store := asTopicList(value); store != nil {
			return store
		}
	}
	store := list.New()
	topicRelevance.Store(topic, store)
	return store
}

const (
	maxTopicsPerRequest = 1024
	registerParallel    = 8
)

var registerSlots = make(chan struct{}, registerParallel)

func enterRegister(ctx context.Context) error {
	select {
	case registerSlots <- struct{}{}:
		return nil
	default:
	}
	noteRegisterWait()
	select {
	case registerSlots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func leaveRegister() {
	select {
	case <-registerSlots:
	default:
	}
}

var lastRegisterWarn atomic.Int64

func noteRegisterWait() {
	now := time.Now().UnixNano()
	prev := lastRegisterWarn.Load()
	if now-prev < int64(time.Second) {
		return
	}
	if lastRegisterWarn.CompareAndSwap(prev, now) {
		logManager("WARN", "register requests are waiting")
	}
}

// StartGrpcService 启动grpc服务
// 包含 话题订阅 与 取消订阅 推送等
func (m *Manager) StartGrpcService(port string) error {
	if m == nil {
		return errors.New("manager is nil")
	}
	lis, err := net.Listen("tcp", port)
	if err != nil {
		logManager("ERROR", fmt.Sprintf("grpc service listen error: %v", err))
		return err
	}
	s := grpc.NewServer(grpc.UnaryInterceptor(grpcRecoverUnary))
	pb.RegisterTopicServiceServer(s, &topicGrpcService{})
	return s.Serve(lis)
}

// grpcRecoverUnary 接住处理函数抛出的 panic。grpc 默认不恢复，一次 panic 会让进程退出。
func grpcRecoverUnary(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp interface{}, err error) {
	defer func() {
		if r := recover(); r != nil {
			logPanic("grpc", r)
			err = status.Error(codes.Internal, "internal error")
			resp = nil
		}
	}()
	return handler(ctx, req)
}

type connectBucket struct {
	overflow   []byte
	packetChan chan *socket.SendMessage
	outbox     chan []byte
	conn       net.Conn
	isClose    bool
	closeChan  chan struct{}
	routeKey   string
	mu         sync.Mutex
}

const gatewayWriteTimeout = 5 * time.Second

// StartSocketService 启动tcp服务
// 主要用来接收gateway的推送消息
func (m *Manager) StartSocketService(addr string) {
	autoMemoryGuard()
	serveSocket(addr, nil, nil)
}

// serveSocket 监听并接入 Gateway。stop 关闭后退出；bound 在开始 Accept 前收到实际地址。
// 一条连接上的解包或写出失败只关闭这一条，Accept 循环继续。
func serveSocket(addr string, stop <-chan struct{}, bound chan<- net.Addr) {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		logManager("ERROR", fmt.Sprintf("tcp service listen error: %v", err))
		return
	}
	logManager("INFO", fmt.Sprintf("tcp service listening: %s", addr))
	if bound != nil {
		bound <- lis.Addr()
	}
	if stop != nil {
		go func() {
			<-stop
			_ = lis.Close()
		}()
	}
	for {
		conn, err := lis.Accept()
		if err != nil {
			logManager("ERROR", fmt.Sprintf("tcp service accept error: %v", err))
			if stop != nil {
				select {
				case <-stop:
					return
				default:
				}
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			timer := time.NewTimer(time.Second)
			if stop == nil {
				<-timer.C
				continue
			}
			select {
			case <-stop:
				timer.Stop()
				return
			case <-timer.C:
			}
			continue
		}
		serveConn(conn)
	}
}

func serveConn(conn net.Conn) {
	defer func() {
		if err := recover(); err != nil {
			logPanic("accept", err)
			if conn != nil {
				_ = conn.Close()
			}
		}
	}()
	if conn == nil {
		return
	}
	bucket := &connectBucket{
		overflow:   make([]byte, 0),
		packetChan: make(chan *socket.SendMessage, 1024),
		outbox:     make(chan []byte, gatewayOutboxLen),
		conn:       conn,
		isClose:    false,
		closeChan:  make(chan struct{}),
	}
	bucket.relation()
	go bucket.writeLoop()
	go bucket.sendLoop()
	go bucket.handler()
	go bucket.heartbeat()
}

func log(types, info string) {
	if types == "INFO" {
		if LogLevel != "INFO" {
			return
		}
		writer := DefaultWriter
		if writer == nil {
			writer = os.Stdout
		}
		fmt.Fprintf(
			writer,
			"[Firetower Manager] %s %s %s | LOGTIME %s | LOG %s\n",
			socket.Green, types, socket.Reset,
			time.Now().Format("2006-01-02 15:04:05"),
			info)
	} else {
		writer := DefaultErrorWriter
		if writer == nil {
			writer = os.Stderr
		}
		fmt.Fprintf(
			writer,
			"[Firetower Manager] %s %s %s | LOGTIME %s | LOG %s\n",
			socket.Red, types, socket.Reset,
			time.Now().Format("2006-01-02 15:04:05"),
			info)
	}
}

func (c *connectBucket) acceptHandshake(message *socket.SendMessage) {
	if c == nil || message == nil {
		return
	}
	key := string(message.Data)
	if key == "" || len(key) > 128 {
		return
	}
	c.rebind(key)
	ack, err := socket.Enpack("handshake_ack", "0", "system", socket.HandshakeTopic, []byte("ok"))
	if err != nil {
		return
	}
	c.deliver(ack)
}

func (c *connectBucket) rebind(key string) {
	if c == nil || key == "" {
		return
	}
	c.mu.Lock()
	if c.isClose {
		c.mu.Unlock()
		return
	}
	prevKey := c.routeKey
	if prevKey == "" && c.conn != nil && c.conn.RemoteAddr() != nil {
		prevKey = c.conn.RemoteAddr().String()
	}
	c.routeKey = key
	c.mu.Unlock()
	func() {
		topicMu.Lock()
		defer topicMu.Unlock()
		claimKeyLocked(key)
		if prevKey != "" && prevKey != key {
			ConnIndexTable.CompareAndDelete(prevKey, c)
			// 握手前按地址登记的条目留在旧键上。重放会按新键再登记，这里先把旧键清掉，避免人数一直偏大。
			forgetIPLocked(prevKey)
			// 之后才到的、仍然写着旧键的登记，记到新键上。
			keyAlias[prevKey] = key
			for from, to := range keyAlias {
				if to == prevKey {
					keyAlias[from] = key
				}
			}
		}
	}()
	swapped, loaded := ConnIndexTable.Swap(key, c)
	noteIdentity(key)
	if !loaded || swapped == nil || swapped == c {
		return
	}
	if old, ok := swapped.(*connectBucket); ok && old != nil && old != c {
		old.close()
	}
}

var (
	identityMu sync.Mutex
	identities = map[string]string{}
)

func noteIdentity(key string) {
	parts := strings.SplitN(key, "-", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
		return
	}
	identityMu.Lock()
	prev, ok := identities[parts[0]]
	if !ok {
		identities[parts[0]] = parts[1]
		identityMu.Unlock()
		return
	}
	identities[parts[0]] = parts[1]
	identityMu.Unlock()
	if prev != parts[1] {
		logManager("WARN", fmt.Sprintf("cluster %s reported startup %s and %s", parts[0], prev, parts[1]))
	}
}

func forgetIdentity(key string) {
	parts := strings.SplitN(key, "-", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
		return
	}
	identityMu.Lock()
	if identities[parts[0]] == parts[1] {
		delete(identities, parts[0])
	}
	identityMu.Unlock()
}

func (c *connectBucket) indexKey() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	key := c.routeKey
	c.mu.Unlock()
	if key != "" {
		return key
	}
	if c.conn != nil && c.conn.RemoteAddr() != nil {
		return c.conn.RemoteAddr().String()
	}
	return ""
}

func (c *connectBucket) relation() {
	if c == nil || c.conn == nil {
		return
	}
	addr := c.conn.RemoteAddr()
	if addr == nil {
		return
	}
	ip := addr.String()
	if ip == "" {
		return
	}
	func() {
		topicMu.Lock()
		defer topicMu.Unlock()
		claimKeyLocked(ip)
	}()
	prev, loaded := ConnIndexTable.Swap(ip, c)
	logManager("INFO", fmt.Sprintf("new connection: %s", ip))
	if !loaded || prev == nil || prev == c {
		return
	}
	if old, ok := prev.(*connectBucket); ok && old != nil && old != c {
		old.close()
	}
}

func (c *connectBucket) delRelation() {
	if c == nil {
		return
	}
	keys := c.indexKeys()
	if len(keys) == 0 {
		return
	}
	topicMu.Lock()
	defer topicMu.Unlock()
	for _, ip := range keys {
		forgetIPLocked(ip)
		// 同一个键已经被另一条连接接手时（地址被复用），不能把它标成已断开。
		if cur, ok := ConnIndexTable.Load(ip); ok && cur != nil && cur != any(c) {
			continue
		}
		buryKeyLocked(ip)
	}
}

// forgetIPLocked 删掉某个地址或路由键下的订阅、版本和对账进度。调用方必须持有 topicMu。
func forgetIPLocked(ip string) {
	if ip == "" {
		return
	}
	delete(subVersion, ip)
	delete(openRounds, ip)
	topicRelevance.Range(func(key, value any) bool {
		store := asTopicList(value)
		if store == nil {
			return true
		}
		for e := store.Front(); e != nil; {
			next := e.Next()
			item := asItem(e)
			if item != nil && item.ip == ip {
				store.Remove(e)
			}
			e = next
		}
		if store.Len() == 0 {
			topicRelevance.Delete(key)
		}
		return true
	})
}

func (c *connectBucket) indexKeys() []string {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	route := c.routeKey
	c.mu.Unlock()
	var keys []string
	if route != "" {
		keys = append(keys, route)
	}
	if c.conn != nil && c.conn.RemoteAddr() != nil {
		remote := c.conn.RemoteAddr().String()
		if remote != "" && remote != route {
			keys = append(keys, remote)
		}
	}
	return keys
}

func (c *connectBucket) close() {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.isClose {
		c.mu.Unlock()
		return
	}
	c.isClose = true
	ip := c.routeKey
	if ip == "" && c.conn != nil && c.conn.RemoteAddr() != nil {
		ip = c.conn.RemoteAddr().String()
	}
	if c.closeChan != nil {
		close(c.closeChan)
	}
	if c.conn != nil {
		_ = c.conn.Close()
	}
	c.mu.Unlock()
	if ip != "" {
		ConnIndexTable.CompareAndDelete(ip, c)
		forgetIdentity(ip)
	}
	c.delRelation()
}

func logPanic(name string, err any) {
	logManager("PANIC", fmt.Sprintf("%s panic: %v", name, err))
}

func (c *connectBucket) handler() {
	defer func() {
		if err := recover(); err != nil {
			logPanic("handler", err)
			c.close()
		}
	}()
	for {
		var buffer = make([]byte, 1024*16)
		l, err := c.conn.Read(buffer)
		if err != nil {
			c.close()
			return
		}
		c.overflow, err = socket.Depack(append(c.overflow, buffer[:l]...), c.packetChan)
		if err != nil {
			logManager("ERROR", err.Error())
		}
	}
}

func (c *connectBucket) deliver(b []byte) {
	if c == nil || len(b) == 0 {
		return
	}
	c.mu.Lock()
	if c.isClose {
		c.mu.Unlock()
		return
	}
	ch := c.outbox
	conn := c.conn
	c.mu.Unlock()
	if ch != nil {
		// 内存紧张时积压超过 1/4 的队列不再接新帧。
		if memPressure.Load() && len(ch) >= cap(ch)/4 {
			gatewayDrops.Add(1)
			memDropped.Add(1)
			noteOutbox(ch)
			return
		}
		payload := append([]byte(nil), b...)
		select {
		case ch <- payload:
			noteOutbox(ch)
		default:
			gatewayDrops.Add(1)
			noteOutbox(ch)
		}
		return
	}
	if conn == nil {
		c.close()
		return
	}
	if _, err := conn.Write(b); err != nil {
		c.close()
	}
}

func (c *connectBucket) writeLoop() {
	defer func() {
		if err := recover(); err != nil {
			logPanic("writeLoop", err)
			c.close()
		}
	}()
	for {
		select {
		case <-c.closeChan:
			return
		case payload, ok := <-c.outbox:
			if !ok || c == nil {
				return
			}
			c.mu.Lock()
			conn := c.conn
			c.mu.Unlock()
			if conn == nil {
				c.close()
				return
			}
			_ = conn.SetWriteDeadline(time.Now().Add(gatewayWriteTimeout))
			if _, err := conn.Write(payload); err != nil {
				warnMinute("gateway-write", "gateway write failed action=check that gateway")
				c.close()
				return
			}
		}
	}
}

var (
	warnMu    sync.Mutex
	warnSince = map[string]time.Time{}
	warnLast  = map[string]time.Time{}
)

func noteOutbox(ch chan []byte) {
	if ch == nil || cap(ch) == 0 {
		return
	}
	line := cap(ch) * 70 / 100
	depth := len(ch)
	if line == 0 || depth < line {
		warnMu.Lock()
		delete(warnSince, "outbox")
		warnMu.Unlock()
		return
	}
	if !held("outbox", 10*time.Second) {
		return
	}
	logManager("WARN", fmt.Sprintf("gateway outbox value=%d line=%d action=check that gateway", depth, line))
}

func warnMinute(name, msg string) {
	if !held(name, 0) {
		return
	}
	logManager("WARN", msg)
}

func held(name string, wait time.Duration) bool {
	now := time.Now()
	warnMu.Lock()
	defer warnMu.Unlock()
	if warnSince[name].IsZero() {
		warnSince[name] = now
	}
	if now.Sub(warnSince[name]) < wait {
		return false
	}
	if last := warnLast[name]; !last.IsZero() && now.Sub(last) < time.Minute {
		return false
	}
	warnLast[name] = now
	return true
}

func (c *connectBucket) sendLoop() {
	defer func() {
		if err := recover(); err != nil {
			logPanic("sendLoop", err)
			c.close()
		}
	}()
	for {
		select {
		case message := <-c.packetChan:
			if message == nil || message.Context == nil {
				continue
			}
			if message.Type == "handshake" && message.Topic == socket.HandshakeTopic {
				c.acceptHandshake(message)
				continue
			}
			// 保留 Topic 只给握手和对账用。客户端经 Gateway 发布到这两个名字的帧不能当成握手，
			// 否则一条 WebSocket 连接就能改掉整台 Gateway 的路由键。
			if message.Type == "handshake" || message.Topic == socket.HandshakeTopic || message.Topic == socket.EpochTopic {
				message.Recycling()
				continue
			}
			targets, ok := snapshotTargets(message.Topic)
			if ok && len(targets) > 0 {
				bytes, err := socket.Enpack(message.Type, message.Context.Id, message.Context.Source, message.Topic, message.Data)
				if err != nil {
					logManager("ERROR", fmt.Sprintf("protocol 封包时错误，%v", err))
					continue
				}
				for _, bucket := range targets {
					bucket.deliver(bytes)
				}
			}
			if socket.NoteHot() {
				message.Info("topic manager sended")
			}
			message.Recycling()
		case <-c.closeChan:
			return
		}
	}
}

func (c *connectBucket) heartbeat() {
	defer func() {
		if err := recover(); err != nil {
			logPanic("heartbeat", err)
			if c != nil {
				c.close()
			}
		}
	}()
	if c == nil || c.conn == nil || c.closeChan == nil {
		return
	}
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	started := time.Now()
	for {
		select {
		case <-c.closeChan:
			return
		case <-ticker.C:
			b, err := socket.Enpack("heartbeat", "0", "system", "*", []byte(managerStatus(started)))
			if err != nil {
				continue
			}
			c.deliver(b)
		}
	}
}

// managerWarmup 之内心跳告诉 Gateway 先不要把发布打到这台。登记进行中仍然是 ready，否则只有一台 Manager 时，一次订阅就会让接下来的发布全部失败。
var managerWarmup time.Duration

const gatewayOutboxLen = 4096

var gatewayDrops atomic.Int64

func managerStatus(started time.Time) string {
	if managerWarmup > 0 && time.Since(started) < managerWarmup {
		return "wait"
	}
	return "ready"
}

// web service
// Response webservice 返回数据结构体
type Response struct {
	Meta *Meta  `json:"meta"`
	Data string `json:"data"`
}

// Meta http response 状态标识
type Meta struct {
	Code  int    `json:"code"`
	Error string `json:"error"`
}

// TopicWebRes topic信息面板结构体
type TopicWebRes struct {
	Title      string `json:"title"`
	ConnectNum int64  `json:"connect_num"`
}

type topics []*TopicWebRes

func (c topics) Len() int {
	return len(c)
}
func (c topics) Swap(i, j int) {
	c[i], c[j] = c[j], c[i]
}
func (c topics) Less(i, j int) bool {
	return c[i].ConnectNum > c[j].ConnectNum
}

// HttpDashboard  dashboard http 服务
func HttpDashboard() {
	// http.HandleFunc("/", Dashboard)
	http.HandleFunc("/topic", topicWebHandler)
	http.ListenAndServe(HttpAddress, nil)
}

// func Dashboard(w http.ResponseWriter, r *http.Request) {
//	 w.Header().Set("Content-Type", "text/html; charset=utf-8")
//	 w.WriteHeader(200)
//	 template.ParseFiles("")
// }

func collectTopicStats() topics {
	topicMu.RLock()
	defer topicMu.RUnlock()
	var topicSlice topics
	topicRelevance.Range(func(key, value interface{}) bool {
		title, ok := key.(string)
		if !ok {
			return true
		}
		t := &TopicWebRes{Title: title, ConnectNum: getConnectNum(asTopicList(value))}
		topicSlice = append(topicSlice, t)
		return true
	})
	return topicSlice
}

// TopicWebHandler 获取topic相关统计信息
func topicWebHandler(w http.ResponseWriter, r *http.Request) {
	defer recoverTopic("topicWebHandler")
	topicSlice := collectTopicStats()

	sort.Sort(topicSlice)
	res, err := json.Marshal(topicSlice)
	response := new(Response)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err != nil {
		w.WriteHeader(500)
		meta := &Meta{
			Code:  5001,
			Error: err.Error(),
		}
		response.Meta = meta
	} else {
		meta := &Meta{
			Code:  0,
			Error: "",
		}
		response.Meta = meta
		response.Data = string(res)
	}
	bytes, _ := json.Marshal(response)
	w.Write(bytes)
}
