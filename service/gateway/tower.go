package gateway

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/OSMeteor/firetower/grpc/manager"
	"github.com/OSMeteor/firetower/socket"

	"github.com/gorilla/websocket"
	"github.com/holdno/snowFlakeByGo"
	json "github.com/json-iterator/go"
)

var (
	topicManage *socket.TcpClient
	// topicManageGrpc 话题管理服务的grpc客户端
	topicManageGrpc pb.TopicServiceClient
	// ClusterId 当前实例在集群中的唯一id
	ClusterId int64 = 1

	// DefaultConfigPath 默认配置文件读取路径
	DefaultConfigPath = "./fireTower.toml"
	// TowerLogger 接管系统log t log类型 info log信息
	TowerLogger func(t *FireTower, types, info string)
	// FireLogger 接管链接log t log类型 info log信息
	FireLogger func(f *FireInfo, types, info string)

	// IdWorker 全局唯一id生成器实例
	IdWorker *snowFlakeByGo.Worker
)

// GetTopicManage 返回连接池里的第一台，供旧调用方使用。新代码走连接池。
func GetTopicManage() *socket.TcpClient {
	poolMu.RLock()
	var tcp *socket.TcpClient
	if len(pool) > 0 && pool[0] != nil {
		tcp = pool[0].tcp
	}
	poolMu.RUnlock()
	if tcp != nil {
		return tcp
	}
	return loadTopicManage()
}

// GetTopicManageGrpc 返回连接池里的第一台，供旧调用方使用。新代码走连接池。
func GetTopicManageGrpc() pb.TopicServiceClient {
	poolMu.RLock()
	var rpc pb.TopicServiceClient
	if len(pool) > 0 && pool[0] != nil {
		rpc = pool[0].rpc
	}
	poolMu.RUnlock()
	if rpc != nil {
		return rpc
	}
	return loadTopicGrpc()
}

// FireInfo 接收的消息结构体
type FireInfo struct {
	Context     *FireLife
	MessageType int
	Message     *TopicMessage
}

// TopicMessage 话题信息结构体
type TopicMessage struct {
	Topic string          `json:"topic"`
	Data  json.RawMessage `json:"data"` // 可能是个json
	Type  string          `json:"type"`
}

// NewFireInfo 第二个参数的作用是继承
// 继承上一个消息体的上下文，方便日志追踪或逻辑统一
// NewFireInfo 第二个参数的作用是继承
// 继承上一个消息体的上下文，方便日志追踪或逻辑统一
func NewFireInfo(t *FireTower, context *FireLife) *FireInfo {
	// Replaced sync.Pool with direct allocation for better safety and simplicity.
	// Go's GC handles short-lived small objects very efficiently.
	fireInfo := &FireInfo{
		Context: new(FireLife),
		Message: new(TopicMessage),
	}

	if context != nil {
		fireInfo.Context = context
	} else {
		fireInfo.Context.reset(t)
	}
	return fireInfo
}

// Recycling 变量回收
// Recycling 变量回收
// Deprecated: No-op. We now rely on Go GC.
func (f *FireInfo) Recycling() {
}

// Panic 消息的panic日志 并回收变量
func (f *FireInfo) Panic(info string) {
	if f == nil {
		return
	}
	callFireLogger(f, "Panic", info)
	f.Recycling()
}

// Info 记录一个INFO级别的日志
func (f *FireInfo) Info(info string) {
	if f == nil {
		return
	}
	callFireLogger(f, "INFO", info)
}

// Error 记录一个ERROR级别的日志
func (f *FireInfo) Error(info string) {
	if f == nil {
		return
	}
	callFireLogger(f, "ERROR", info)
}

// FireLife 客户端推送消息的结构体
type FireLife struct {
	id        string
	startTime time.Time
	message   *TopicMessage
	clientId  string
	userId    string
}

func (f *FireLife) reset(t *FireTower) {
	if f == nil {
		return
	}
	f.startTime = time.Now()
	if worker := loadIDWorker(); worker != nil {
		f.id = strconv.FormatInt(worker.GetId(), 10)
	} else {
		f.id = strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	if t != nil {
		f.clientId = t.ClientId
		f.userId = t.UserId
	}
}

// FireTower 客户端连接结构体
// 包含了客户端一个连接的所有信息
type FireTower struct {
	connId    uint64 // 连接id 每台服务器上该id从1开始自增
	ClientId  string // 客户端id 用来做业务逻辑
	UserId    string // 一般业务中每个连接都是一个用户 用来给业务提供用户识别
	Cookie    []byte // 这里提供给业务放一个存放跟当前连接相关的数据信息
	startTime time.Time
	// 最后一次收到 Pong 或任何客户端消息的时间（UnixNano）。内存保护靠它判断连接是否还活着。
	lastActive atomic.Int64

	readIn    chan *FireInfo           // 读取队列
	sendOut   chan *socket.SendMessage // 发送队列
	ws        *websocket.Conn          // 保存底层websocket连接
	topic     map[string]bool          // 客户端看到的 topic
	route     map[string]string        // 客户端 topic → 登记到 Manager 的键。未分组时两者相同
	isClose   bool                     // 判断当前websocket是否被关闭
	closeChan chan struct{}            // 用来作为关闭websocket的触发点
	mutex     sync.Mutex               // 避免并发close chan

	onConnectHandler       func() bool
	onOfflineHandler       func()
	readHandler            func(*FireInfo) bool
	readTimeoutHandler     func(*FireInfo)
	subscribeHandler       func(context *FireLife, topic []string) bool
	unSubscribeHandler     func(context *FireLife, topic []string) bool
	beforeSubscribeHandler func(context *FireLife, topic []string) ([]string, bool)
	onSystemRemove         func(topic string)
	subscribeFailHandler   func(topics []string, err error)
	counted                bool
	gapHints               atomic.Bool
	gapWake                chan struct{}
	gapMu                  sync.Mutex
	gaps                   map[string]struct{}
	passive                map[string]bool
}

var liveConns atomic.Int64

// Init 初始化firetower
// 在调用firetower前请一定要先调用Init方法
// Init 初始化firetower
// 在调用firetower前请一定要先调用Init方法
func Init() {
	worker, _ := snowFlakeByGo.NewWorker(ClusterId)
	storeIDWorker(worker)

	storeTowerLogger(towerLog)
	storeFireLogger(fireLog)

	loadConfig(DefaultConfigPath) // 加载配置
	loadGroups(loadConfigTree())
	buildBuckets()       // 构建服务架构
	buildManagerClient() // 构建连接manager(topic管理服务)的客户端
	startMemoryGuard()   // 配置了 memory.limitMB 才会启动
}

// BuildTower 实例化一个websocket客户端
func BuildTower(ws *websocket.Conn, clientId string) (tower *FireTower) {
	if loadTopicGrpc() == nil {
		towerLog(nil, "ERROR", "please confirm gateway was inited")
		return nil
	}
	if draining.Load() {
		towerLog(nil, "ERROR", "gateway is draining")
		return nil
	}

	if ws == nil {
		towerLog(nil, "ERROR", "websocket.Conn is nil")
		return nil
	}
	if memRejecting.Load() {
		noteMemReject()
		return nil
	}
	if !reserveConn() {
		towerLog(nil, "ERROR", "connection limit reached")
		return nil
	}
	tower = buildNewTower(ws, clientId)
	tower.counted = true
	liveTowers.Store(tower.connId, tower)
	return
}

var (
	draining   atomic.Bool
	liveTowers sync.Map
)

// Drain 停止接入，再按批关闭已经在线上的连接。客户端会断开并自行重连。
func Drain(batch int, pause time.Duration) {
	draining.Store(true)
	if batch < 1 {
		batch = 32
	}
	var group []*FireTower
	liveTowers.Range(func(_, value any) bool {
		ft, _ := value.(*FireTower)
		if ft != nil {
			group = append(group, ft)
		}
		return true
	})
	for i := 0; i < len(group); {
		j := i + batch
		if j > len(group) {
			j = len(group)
		}
		for _, ft := range group[i:j] {
			ft.Close()
		}
		i = j
		if pause > 0 && i < len(group) {
			time.Sleep(pause)
		}
	}
}

func reserveConn() bool {
	limit := int64(maxConnections())
	for {
		cur := liveConns.Load()
		if cur >= limit {
			return false
		}
		if liveConns.CompareAndSwap(cur, cur+1) {
			pct := int64(configInt("warn.connectionPercent", 70))
			warnCapacity("connections", cur+1, limit*pct/100, "add gateway")
			return true
		}
	}
}

func releaseConn() {
	liveConns.Add(-1)
}

func buildNewTower(ws *websocket.Conn, clientId string) *FireTower {
	t := &FireTower{}
	t.connId = getConnId()
	t.ClientId = clientId
	t.startTime = time.Now()
	t.touch()
	t.readIn = make(chan *FireInfo, configInt("chanLens", 8))
	t.sendOut = make(chan *socket.SendMessage, configInt("chanLens", 8))
	t.topic = make(map[string]bool)
	t.ws = ws
	t.isClose = false
	t.closeChan = make(chan struct{})
	t.gapWake = make(chan struct{}, 1)

	t.readHandler = nil
	t.readTimeoutHandler = nil
	t.subscribeHandler = nil
	t.unSubscribeHandler = nil
	t.beforeSubscribeHandler = nil
	return t
}

// Run 启动websocket客户端
func (t *FireTower) Run() {
	if t == nil {
		return
	}
	logInfo(t, "new websocket running")
	// 读取websocket信息
	go t.readLoop()
	// 处理读取事件
	// 这两个协程都依赖 closeChan，在连接断开或被 Close 触发时会自动退出，避免 goroutine 泄漏
	go t.readDispose()

	if t.onConnectHandler != nil {
		// 回调跑在调用方的协程上。它 panic 时按拒绝连接处理，不能带走进程。
		ok := false
		safeCall("on connect handler", func() { ok = t.onConnectHandler() })
		if !ok {
			t.Close()
		}
	}
	// 向websocket发送信息
	t.sendLoop()
}

func managerIP() (string, error) {
	manage, grpcClient := loadTopicClients()
	if grpcClient == nil {
		return "", errors.New("topicManageGrpc is nil")
	}
	if manage == nil {
		return "", errors.New("topicManage is nil")
	}
	if key := manage.RouteKey(); key != "" {
		return key, nil
	}
	addr := manage.CurrentAddr()
	if addr == "" {
		return "", errors.New("topicManage.Conn is nil")
	}
	return addr, nil
}

// 订阅topic的绑定过程
func (t *FireTower) bindTopic(topic []string) ([]string, error) {
	var addTopic []string
	if t == nil {
		return addTopic, errors.New("t is nil")
	}
	attachReplay()
	tm := loadTM()
	if tm == nil {
		return addTopic, errors.New("TM is nil")
	}
	bucket := tm.GetBucket(t)
	if bucket == nil {
		return addTopic, errors.New("bucket is nil")
	}
	t.mutex.Lock()
	if t.isClose {
		t.mutex.Unlock()
		return addTopic, ErrorClose
	}
	if t.topic == nil {
		t.topic = make(map[string]bool)
	}
	room := maxTopicsPerConn()
	seen := make(map[string]struct{}, len(topic))
	var pending []string
	for _, v := range topic {
		if v == "" || reservedTopic(v) {
			continue
		}
		if _, ok := t.topic[v]; ok {
			continue
		}
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		pending = append(pending, v)
	}
	if len(t.topic)+len(pending) > room {
		t.mutex.Unlock()
		return addTopic, errors.New("too many topics on this connection")
	}
	for _, v := range pending {
		addTopic = append(addTopic, v)
	}
	t.mutex.Unlock()
	if len(addTopic) == 0 {
		return addTopic, nil
	}
	routes := make([]string, len(addTopic))
	for i, name := range addTopic {
		routes[i] = registerKey(name, shardUser(t))
	}
	counts, versions := noteSubscribe(routes)
	if err := subscribeAll(routes, counts, versions); err != nil {
		// 多台 Manager 时可能前几台已经写成功。把撤销后的人数盖回去，写不到的那台等重放。
		backCounts, backVersions := revertSubscribe(routes)
		_ = unsubscribeAll(routes, backCounts, backVersions)
		if t.subscribeFailHandler != nil {
			t.subscribeFailHandler(addTopic, err)
		}
		return addTopic, err
	}
	t.mutex.Lock()
	if t.isClose {
		t.mutex.Unlock()
		// 登记期间连接关掉了。Close 当时还看不到这次订阅，这里要自己把 Manager 上的人数改回去。
		backCounts, backVersions := revertSubscribe(routes)
		_ = unsubscribeAll(routes, backCounts, backVersions)
		return addTopic, ErrorClose
	}
	if t.route == nil {
		t.route = make(map[string]string, len(addTopic))
	}
	for i, name := range addTopic {
		t.topic[name] = true
		t.route[name] = routes[i]
	}
	t.mutex.Unlock()
	for _, key := range routes {
		bucket.AddSubscribe(key, t)
	}
	return addTopic, nil
}

// reservedTopic 这两个名字是 Gateway 和 Manager 之间握手、对账用的，客户端不能订阅也不能发布。
// 片键的分隔符也只在框架内部出现，客户端自己拼片键会绕过分组、把人数带歪。
func reservedTopic(name string) bool {
	return name == socket.HandshakeTopic || name == socket.EpochTopic || strings.Contains(name, groupSep)
}

func (t *FireTower) unbindTopic(topic []string) ([]string, error) {
	var delTopic []string
	if t == nil {
		return delTopic, errors.New("t is nil")
	}
	attachReplay()
	tm := loadTM()
	if tm == nil {
		return delTopic, errors.New("TM is nil")
	}
	bucket := tm.GetBucket(t)
	var delRoute []string
	t.mutex.Lock()
	for _, v := range topic {
		client, route, ok := t.takeTopicLocked(v)
		if !ok {
			continue
		}
		delTopic = append(delTopic, client)
		delRoute = append(delRoute, route)
	}
	t.mutex.Unlock()
	if bucket != nil {
		for _, key := range delRoute {
			bucket.DelSubscribe(key, t)
		}
	}
	if len(delTopic) == 0 {
		return delTopic, nil
	}
	counts, versions := noteUnsubscribe(delRoute)
	return delTopic, unsubscribeAll(delRoute, counts, versions)
}

func (t *FireTower) takeTopicLocked(name string) (client, route string, ok bool) {
	if t == nil || name == "" || t.topic == nil {
		return "", "", false
	}
	if t.topic[name] {
		route = name
		if t.route != nil && t.route[name] != "" {
			route = t.route[name]
		}
		delete(t.topic, name)
		delete(t.route, name)
		return name, route, true
	}
	for client, route := range t.route {
		if route != name {
			continue
		}
		delete(t.topic, client)
		delete(t.route, client)
		return client, route, true
	}
	return "", "", false
}

func (t *FireTower) read() (*FireInfo, error) {
	if t == nil || t.readIn == nil {
		return nil, ErrorClose
	}
	t.mutex.Lock()
	stop := t.closeChan
	closed := t.isClose
	t.mutex.Unlock()
	if closed && stop == nil {
		return nil, ErrorClose
	}
	select {
	case message, ok := <-t.readIn:
		if !ok || message == nil {
			return nil, ErrorClose
		}
		return message, nil
	case <-stop:
		return nil, ErrorClose
	}
}

// Send 发送消息方法
// 向某个topic发送某段信息
// Send 发送消息方法
// 向某个topic发送某段信息
func (t *FireTower) Send(message *socket.SendMessage) error {
	if t == nil || message == nil {
		return errors.New("send message is nil")
	}
	t.mutex.Lock()
	if t.isClose || t.sendOut == nil {
		t.mutex.Unlock()
		return ErrorClose
	}
	ch := t.sendOut
	t.mutex.Unlock()
	// 正文在进入 Bucket 时已经复制过。这里只复制结构体，一万人的房间不用再分配一万份正文。
	cloned := *message
	if message.Context != nil {
		life := *message.Context
		cloned.Context = &life
	}
	message = &cloned
	// 内存紧张时，已经积压的队列不再接新消息，免得在两轮检查之间继续涨。正常连接的队列很短，不受影响。
	if memPressure.Load() && len(ch) >= memBacklogMin(cap(ch)) {
		noteSendDrop(t)
		memTrimmed.Add(1)
		if message.Type != socket.StateKey {
			t.noteGap(parentTopic(message.Topic))
		}
		return errors.New("memory guard: backlog dropped")
	}
	// 非阻塞发送，防止慢消费者阻塞整个 Bucket 的分发。
	// 向已关闭的队列发送会 panic，这里接住，只丢掉这一条。
	if err := enqueueSend(ch, message); err != nil {
		noteSendDrop(t)
		if message.Type != socket.StateKey {
			t.noteGap(parentTopic(message.Topic))
		}
		return err
	}
	return nil
}

func enqueueSend(ch chan *socket.SendMessage, message *socket.SendMessage) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("send channel closed")
		}
	}()
	select {
	case ch <- message:
		return nil
	default:
		return errors.New("send buffer full")
	}
}

// EnableGapHints 打开缺失提示。默认关闭，现有客户端不会收到新的帧。
func (t *FireTower) EnableGapHints() {
	if t == nil {
		return
	}
	t.gapHints.Store(true)
	if t.gapWake == nil {
		t.gapWake = make(chan struct{}, 1)
	}
}

// SetPassive 把这条连接在该 Topic 上标成被动。被动连接不收状态事件，业务消息照旧。默认是主动。
func (t *FireTower) SetPassive(topic string, on bool) {
	if t == nil || topic == "" {
		return
	}
	t.mutex.Lock()
	defer t.mutex.Unlock()
	if t.passive == nil {
		t.passive = map[string]bool{}
	}
	if on {
		t.passive[topic] = true
	} else {
		delete(t.passive, topic)
	}
}

func (t *FireTower) passiveTopic(topic string) bool {
	if t == nil {
		return false
	}
	t.mutex.Lock()
	defer t.mutex.Unlock()
	if t.passive[topic] {
		return true
	}
	parent := parentTopic(topic)
	return parent != topic && t.passive[parent]
}

func (t *FireTower) noteGap(topic string) {
	if t == nil || topic == "" || !t.gapHints.Load() {
		return
	}
	t.gapMu.Lock()
	if t.gaps == nil {
		t.gaps = map[string]struct{}{}
	}
	_, existed := t.gaps[topic]
	t.gaps[topic] = struct{}{}
	wake := t.gapWake
	t.gapMu.Unlock()
	if existed || wake == nil {
		return
	}
	select {
	case wake <- struct{}{}:
	default:
	}
}

func (t *FireTower) flushGap() {
	if t == nil || !t.gapHints.Load() || t.ws == nil {
		return
	}
	t.gapMu.Lock()
	var topic string
	for k := range t.gaps {
		topic = k
		delete(t.gaps, k)
		break
	}
	more := len(t.gaps) > 0
	wake := t.gapWake
	t.gapMu.Unlock()
	if topic == "" {
		return
	}
	body, err := json.Marshal(map[string]string{"type": "gap", "topic": topic})
	if err != nil {
		return
	}
	_ = t.ws.SetWriteDeadline(time.Now().Add(3 * time.Second))
	_ = t.ws.WriteMessage(websocket.TextMessage, body)
	if more && wake != nil {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}

func cloneSendMessage(message *socket.SendMessage) *socket.SendMessage {
	if message == nil {
		return nil
	}
	cloned := *message
	if message.Context != nil {
		life := *message.Context
		cloned.Context = &life
	}
	if message.Data != nil {
		cloned.Data = append([]byte(nil), message.Data...)
	}
	return &cloned
}

var lastDropLog atomic.Int64

var sendDrops atomic.Int64

func noteSendDrop(t *FireTower) {
	sendDrops.Add(1)
	now := time.Now().UnixNano()
	prev := lastDropLog.Load()
	if now-prev < int64(time.Second) || !lastDropLog.CompareAndSwap(prev, now) {
		return
	}
	callTowerLogger(t, "WARN", "send buffer full, message dropped")
}

// Close 关闭客户端连接并注销
// 调用该方法会完全注销掉由BuildTower生成的一切内容
func (t *FireTower) Close() {
	if t == nil {
		return
	}
	t.mutex.Lock()
	if t.isClose {
		t.mutex.Unlock()
		return
	}
	t.isClose = true
	liveTowers.Delete(t.connId)
	counted := t.counted
	t.counted = false
	var topics []string
	for k := range t.topic {
		topics = append(topics, k)
	}
	ws := t.ws
	stop := t.closeChan
	offline := t.onOfflineHandler
	unsub := t.unSubscribeHandler
	t.mutex.Unlock()
	if counted {
		releaseConn()
	}

	// 先断开连接并唤醒读写协程，再做 RPC。RPC 失败或回调重入都不能把协程留住。
	if ws != nil {
		_ = ws.Close()
	}
	if stop != nil {
		select {
		case <-stop:
		default:
			close(stop)
		}
	}
	logInfo(t, "websocket connect is closed")

	if len(topics) > 0 {
		delTopic, err := t.unbindTopic(topics)
		if err != nil {
			towerLog(t, "ERROR", err.Error())
		} else if unsub != nil {
			fire := NewFireInfo(t, nil)
			if fire != nil && fire.Context != nil {
				// Close 可能由业务协程直接调用。两个回调各自接住 panic，一个出错不跳过另一个。
				safeCall("unsubscribe handler", func() { unsub(fire.Context, delTopic) })
			}
		}
	}
	if offline != nil {
		safeCall("offline handler", offline)
	}
}

func (t *FireTower) sendLoop() {
	defer func() {
		if err := recover(); err != nil {
			towerLog(t, "PANIC", fmt.Sprintf("sendLoop panic: %v", err))
			safeClose(t)
		}
	}()
	heartTicker := time.NewTicker(heartbeatInterval())
	defer heartTicker.Stop()
	for {
		select {
		case message := <-t.sendOut:
			if message == nil {
				towerLog(t, "ERROR", "sendLoop received nil message")
				continue
			}
			if t.ws == nil {
				goto collapse
			}
			if err := t.ws.SetWriteDeadline(time.Now().Add(3 * time.Second)); err != nil {
				message.Panic(fmt.Sprintf("set write deadline failed: %v", err))
				goto collapse
			}
			kind := message.MessageType
			if kind == 0 {
				kind = websocket.TextMessage
			}
			if err := t.ws.WriteMessage(kind, message.Data); err != nil {
				if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					message.Info(fmt.Sprintf("websocket closed while sending: %v", err))
				} else {
					message.Panic(err.Error())
				}
				goto collapse
			}
			t.flushGap()
		case <-t.gapWake:
			t.flushGap()
		case <-heartTicker.C:
			if t.ws == nil {
				goto collapse
			}
			// 协议层 Ping，不进入业务消息。队列满也不会因为心跳关掉连接。
			if err := t.ws.WriteControl(websocket.PingMessage, []byte("ping"), time.Now().Add(time.Second)); err != nil {
				towerLog(t, "ERROR", fmt.Sprintf("heartbeat ping failed: %v", err))
				goto collapse
			}
		case <-t.closeChan:
			return
		}
	}
collapse:
	t.Close()
}

func (t *FireTower) readLoop() {
	defer func() {
		if err := recover(); err != nil {
			towerLog(t, "PANIC", fmt.Sprintf("%s", err))
			safeClose(t)
		}
	}()
	if t == nil || t.ws == nil {
		safeClose(t)
		return
	}
	t.ws.SetReadLimit(1 << 20)
	wait := readWait()
	_ = t.ws.SetReadDeadline(time.Now().Add(wait))
	t.ws.SetPongHandler(func(string) error {
		if t.ws == nil {
			return nil
		}
		t.touch()
		return t.ws.SetReadDeadline(time.Now().Add(wait))
	})
	for {
		_ = t.ws.SetReadDeadline(time.Now().Add(wait))
		messageType, data, err := t.ws.ReadMessage()
		if err != nil { // 断开连接
			goto collapse // 出现问题烽火台直接坍塌
		}
		t.touch()
		fire := NewFireInfo(t, nil) // 从对象池中获取消息对象 降低GC压力
		fire.MessageType = messageType

		if err := json.Unmarshal(data, &fire.Message); err != nil {
			fire.Panic(fmt.Sprintf("client sended data was unmarshal error:%v", err))
			continue
		}

		timeout := time.After(time.Duration(3) * time.Second)
		select {
		case t.readIn <- fire:
		case <-timeout:
			if t.readTimeoutHandler != nil {
				t.readTimeoutHandler(fire)
			}
			fire.Panic(fmt.Sprintf("readLoop timeout: %q", data))
		case <-t.closeChan:
			return
		}
	}
collapse:
	t.Close()
}

// 处理前端发来的数据
// 这里要做逻辑拆分，判断用户是要进行通信还是topic订阅
func (t *FireTower) readDispose() {
	defer func() {
		if err := recover(); err != nil {
			towerLog(t, "PANIC", fmt.Sprintf("readDispose panic: %v", err))
			safeClose(t)
		}
	}()
	for {
		fire, err := t.read()
		if err != nil {
			if fire != nil {
				fire.Panic(fmt.Sprintf("read message failed:%v", err))
			} else {
				// Manually log if fire is nil
				callTowerLogger(t, "ERROR", fmt.Sprintf("read message failed:%v", err))
			}
			t.Close()
			return
		}
		t.mutex.Lock()
		closed := t.isClose
		t.mutex.Unlock()
		if closed {
			return
		}
		{
			if fire == nil || fire.Message == nil {
				continue
			}
			if fire.Message.Topic == "" {
				fire.Panic(fmt.Sprintf("%s:topic is empty, ClintId:%s, UserId:%s", fire.Message.Type, t.ClientId, t.UserId))
				continue
			}
			switch fire.Message.Type {
			case "subscribe": // 客户端订阅topic
				addTopic, ok := splitTopics(fire.Message.Topic, maxTopicsPerFrame())
				if !ok {
					fire.Error("too many topics in one frame")
					fire.Recycling()
					continue
				}
				// 如果设置了订阅前触发事件则调用
				if t.beforeSubscribeHandler != nil {
					var allow bool
					addTopic, allow = t.beforeSubscribeHandler(fire.Context, addTopic)
					if !allow {
						continue
					}
					if len(addTopic) > maxTopicsPerFrame() {
						fire.Error("too many topics in one frame")
						fire.Recycling()
						continue
					}
				}
				// 增加messageId 方便追踪
				addTopic, err = t.bindTopic(addTopic)
				if err != nil {
					fire.Error(err.Error())
				} else if t.subscribeHandler != nil {
					t.subscribeHandler(fire.Context, addTopic)
				}
			case "unSubscribe": // 客户端取消订阅topic
				delTopic, ok := splitTopics(fire.Message.Topic, maxTopicsPerFrame())
				if !ok {
					fire.Error("too many topics in one frame")
					fire.Recycling()
					continue
				}
				delTopic, err = t.unbindTopic(delTopic)
				if err != nil {
					fire.Error(err.Error())
				} else if t.unSubscribeHandler != nil {
					t.unSubscribeHandler(fire.Context, delTopic)
				}
			default:
				if t.readHandler != nil {
					ok := t.readHandler(fire)
					if !ok {
						fire.Panic("readHandler return false")
						t.Close()
						return
					}
				}
			}
			if socket.NoteHot() {
				fire.Info("Extinguished")
			}
			fire.Recycling()
		}
	}
}

// Publish 推送接口
// 通过BuildTower生成的实例都可以调用该方法来达到推送的目的
func (t *FireTower) Publish(fire *FireInfo) error {
	if fire == nil || fire.Context == nil || fire.Message == nil {
		return errors.New("empty publish")
	}
	if reservedTopic(fire.Message.Topic) {
		return errors.New("reserved topic")
	}
	var first error
	for _, key := range publishKeys(fire.Message.Topic) {
		link := pickPublisher(key)
		if link == nil || link.tcp == nil {
			if first == nil {
				first = errors.New("topicManage is nil")
			}
			continue
		}
		if err := link.tcp.Publish(fire.Context.id, "user", key, fire.Message.Data); err != nil {
			fire.Panic(fmt.Sprintf("publish err: %v", err))
			if first == nil {
				first = err
			}
		}
	}
	return first
}
func (f *FireInfo) GetContextId() string {
	if f == nil || f.Context == nil {
		return ""
	}
	if strings.TrimSpace(f.Context.id) == "" {
		if worker := loadIDWorker(); worker != nil {
			f.Context.id = strconv.FormatInt(worker.GetId(), 10)
		} else {
			f.Context.id = strconv.FormatInt(time.Now().UnixNano(), 10)
		}
	}
	return f.Context.id
}

func configInt(key string, fallback int) int {
	tree := loadConfigTree()
	if tree == nil {
		return fallback
	}
	v, ok := tree.Get(key).(int64)
	if !ok || v <= 0 {
		return fallback
	}
	return int(v)
}

func heartbeatInterval() time.Duration {
	return time.Duration(configInt("heartbeat", 30)) * time.Second
}

func maxTopicsPerFrame() int { return configInt("maxTopicsPerFrame", 1024) }

func maxTopicsPerConn() int { return configInt("maxTopicsPerConn", 4096) }

func maxConnections() int { return configInt("maxConnections", 100000) }

// splitTopics 按逗号拆 Topic。超过 limit 时停止，不把整帧拆成几十万个字符串。
func splitTopics(raw string, limit int) ([]string, bool) {
	if limit <= 0 {
		limit = 1024
	}
	out := make([]string, 0, 8)
	start := 0
	for i := 0; i <= len(raw); i++ {
		if i != len(raw) && raw[i] != ',' {
			continue
		}
		part := raw[start:i]
		start = i + 1
		if part == "" {
			continue
		}
		if len(out) >= limit {
			return nil, false
		}
		out = append(out, part)
	}
	return out, true
}

const managerRPCTimeout = 500 * time.Millisecond

func readWait() time.Duration {
	wait := heartbeatInterval() * 2
	if wait <= 0 {
		return 60 * time.Second
	}
	return wait
}

// ToSelf 向自己推送消息
// 这里描述一下使用场景
// 只针对当前客户端进行的推送请调用该方法
func (t *FireTower) ToSelf(b []byte) error {
	if t == nil {
		return ErrorClose
	}
	// 只能由 sendLoop 写 WebSocket。这里入队，避免和发送循环并发写导致进程 panic。
	message := socket.GetSendMessage("0", "self")
	message.MessageType = websocket.TextMessage
	if b != nil {
		message.Data = append([]byte(nil), b...)
	}
	return t.Send(message)
}

func safeClose(t *FireTower) {
	if t == nil {
		return
	}
	defer func() { _ = recover() }()
	t.Close()
}

// CheckTopicExist 检测topic是否已经有人订阅
func (t *FireTower) CheckTopicExist(topic string) bool {
	if topic == "" {
		return false
	}
	for _, key := range publishKeys(topic) {
		client := rpcFor(key)
		if client == nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), managerRPCTimeout)
		res, err := client.CheckTopicExist(ctx, &pb.CheckTopicExistRequest{Topic: key})
		cancel()
		if err == nil && res != nil && res.Ok {
			return true
		}
	}
	return false
}

func rpcFor(topic string) pb.TopicServiceClient {
	link := pickPublisher(topic)
	if link != nil && link.rpc != nil {
		return link.rpc
	}
	return loadTopicGrpc()
}

// SetOnConnectHandler 建立连接事件
func (t *FireTower) SetOnConnectHandler(fn func() bool) {
	if t == nil {
		return
	}
	t.onConnectHandler = fn
}

// SetOnOfflineHandler 用户连接关闭时触发
func (t *FireTower) SetOnOfflineHandler(fn func()) {
	if t == nil {
		return
	}
	t.onOfflineHandler = fn
}

// SetReadHandler 客户端推送事件
// 接收到用户publish的消息时触发
func (t *FireTower) SetReadHandler(fn func(*FireInfo) bool) {
	if t == nil {
		return
	}
	t.readHandler = fn
}

// SetSubscribeHandler 订阅事件
// 用户订阅topic后触发
func (t *FireTower) SetSubscribeHandler(fn func(context *FireLife, topic []string) bool) {
	if t == nil {
		return
	}
	t.subscribeHandler = fn
}

// SetUnSubscribeHandler 取消订阅事件
// 用户取消订阅topic后触发
func (t *FireTower) SetUnSubscribeHandler(fn func(context *FireLife, topic []string) bool) {
	if t == nil {
		return
	}
	t.unSubscribeHandler = fn
}

// SetBeforeSubscribeHandler 订阅前回调事件
// 用户订阅topic前触发
func (t *FireTower) SetBeforeSubscribeHandler(fn func(context *FireLife, topic []string) ([]string, bool)) {
	if t == nil {
		return
	}
	t.beforeSubscribeHandler = fn
}

// SetReadTimeoutHandler 超时回调
// readIn channal写满了  生产 > 消费的情况下触发超时机制
func (t *FireTower) SetReadTimeoutHandler(fn func(*FireInfo)) {
	if t == nil {
		return
	}
	t.readTimeoutHandler = fn
}

// SetOnSystemRemove 系统移除某个用户的topic订阅
func (t *FireTower) SetOnSystemRemove(fn func(topic string)) {
	if t == nil {
		return
	}
	t.onSystemRemove = fn
}

// SetSubscribeFailHandler 订阅登记失败时通知接入进程。不设置时只记日志，连接和帧格式不变。
func (t *FireTower) SetSubscribeFailHandler(fn func(topics []string, err error)) {
	if t == nil {
		return
	}
	t.subscribeFailHandler = fn
}

// GetConnectNum 获取话题订阅数的grpc方法封装
func (t *FireTower) GetConnectNum(topic string) int64 {
	if topic == "" {
		return 0
	}
	var sum int64
	for _, key := range publishKeys(topic) {
		client := rpcFor(key)
		if client == nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), managerRPCTimeout)
		res, err := client.GetConnectNum(ctx, &pb.GetConnectNumRequest{Topic: key})
		cancel()
		if err != nil || res == nil {
			continue
		}
		sum += res.Number
	}
	return sum
}
