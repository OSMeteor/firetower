package manager

import (
	"container/list"
	"context"
	"fmt"
	"io"
	"net"
	"os"
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
	}
	return &pb.GetConnectNumResponse{Number: num}, nil
}

func snapshotTargets(topic string) ([]*connectBucket, bool) {
	topicMu.RLock()
	defer topicMu.RUnlock()
	value, ok := topicRelevance.Load(topic)
	if !ok {
		return nil, false
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
	if request.Round > 0 && request.RoundBegin {
		openReconcile(request.Ip, request.Round)
	}
	for i, topic := range request.Topic {
		if topic == "" || request.Ip == "" || topic == socket.EpochTopic {
			if topic == socket.EpochTopic && len(request.Version) > i {
				noteEpoch(request.Ip, request.Version[i])
			}
			continue
		}
		markReconcile(request.Ip, topic)
		if len(request.Version) > 0 && request.Version[i] > 0 {
			applyAbsolute(topic, request.Ip, request.Count[i], request.Version[i])
			continue
		}
		bumpTopic(topic, request.Ip)
	}
	if request.Round > 0 && request.RoundEnd {
		finishReconcile(request.Ip, request.Round)
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
	for i, topic := range request.Topic {
		if topic == "" || request.Ip == "" {
			continue
		}
		if len(request.Version) > 0 && request.Version[i] > 0 {
			applyAbsolute(topic, request.Ip, request.Count[i], request.Version[i])
			continue
		}
		dropOne(topic, request.Ip)
	}
	return &pb.UnSubscribeTopicResponse{}, nil
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
	if prevKey != "" && prevKey != key {
		ConnIndexTable.CompareAndDelete(prevKey, c)
		// 握手前按地址登记的条目留在旧键上。重放会按新键再登记，这里先把旧键清掉，避免人数一直偏大。
		topicMu.Lock()
		forgetIPLocked(prevKey)
		topicMu.Unlock()
	}
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
			if message.Type == "handshake" || message.Topic == socket.HandshakeTopic {
				c.acceptHandshake(message)
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
