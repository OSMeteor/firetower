package gateway

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
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

func GetTopicManage() *socket.TcpClient {
	return topicManage
}
func GetTopicManageGrpc() pb.TopicServiceClient {
	return topicManageGrpc
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
	if FireLogger != nil {
		FireLogger(f, "Panic", info)
	}
	f.Recycling()
}

// Info 记录一个INFO级别的日志
func (f *FireInfo) Info(info string) {
	if f == nil || FireLogger == nil {
		return
	}
	FireLogger(f, "INFO", info)
}

// Error 记录一个ERROR级别的日志
func (f *FireInfo) Error(info string) {
	if f == nil || FireLogger == nil {
		return
	}
	FireLogger(f, "ERROR", info)
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
	if IdWorker != nil {
		f.id = strconv.FormatInt(IdWorker.GetId(), 10)
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

	readIn    chan *FireInfo           // 读取队列
	sendOut   chan *socket.SendMessage // 发送队列
	ws        *websocket.Conn          // 保存底层websocket连接
	topic     map[string]bool          // 订阅topic列表
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
}

// Init 初始化firetower
// 在调用firetower前请一定要先调用Init方法
// Init 初始化firetower
// 在调用firetower前请一定要先调用Init方法
func Init() {
	IdWorker, _ = snowFlakeByGo.NewWorker(ClusterId)

	TowerLogger = towerLog
	FireLogger = fireLog

	loadConfig(DefaultConfigPath) // 加载配置
	buildBuckets()                // 构建服务架构
	buildManagerClient()          // 构建连接manager(topic管理服务)的客户端
}

// BuildTower 实例化一个websocket客户端
func BuildTower(ws *websocket.Conn, clientId string) (tower *FireTower) {
	if topicManageGrpc == nil {
		towerLog(nil, "ERROR", "please confirm gateway was inited")
		return nil
	}

	if ws == nil {
		towerLog(nil, "ERROR", "websocket.Conn is nil")
		return nil
	}
	tower = buildNewTower(ws, clientId)
	return
}

func buildNewTower(ws *websocket.Conn, clientId string) *FireTower {
	t := &FireTower{}
	t.connId = getConnId()
	t.ClientId = clientId
	t.startTime = time.Now()
	t.readIn = make(chan *FireInfo, configInt("chanLens", 8))
	t.sendOut = make(chan *socket.SendMessage, configInt("chanLens", 8))
	t.topic = make(map[string]bool)
	t.ws = ws
	t.isClose = false
	t.closeChan = make(chan struct{})

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
		ok := t.onConnectHandler()
		if !ok {
			t.Close()
		}
	}
	// 向websocket发送信息
	t.sendLoop()
}

func managerIP() (string, error) {
	if topicManageGrpc == nil {
		return "", errors.New("topicManageGrpc is nil")
	}
	if topicManage == nil {
		return "", errors.New("topicManage is nil")
	}
	if topicManage.Conn == nil {
		return "", errors.New("topicManage.Conn is nil")
	}
	addr := topicManage.Conn.LocalAddr()
	if addr == nil || addr.Network() == "" || addr.String() == "" {
		return "", errors.New("topicManage.Conn is nil")
	}
	return addr.String(), nil
}

// 订阅topic的绑定过程
func (t *FireTower) bindTopic(topic []string) ([]string, error) {
	var addTopic []string
	if t == nil {
		return addTopic, errors.New("t is nil")
	}
	if TM == nil {
		return addTopic, errors.New("TM is nil")
	}
	ip, err := managerIP()
	if err != nil {
		return addTopic, err
	}
	bucket := TM.GetBucket(t)
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
	for _, v := range topic {
		if v == "" {
			continue
		}
		if _, ok := t.topic[v]; !ok {
			addTopic = append(addTopic, v)
			t.topic[v] = true
		}
	}
	t.mutex.Unlock()
	for _, v := range addTopic {
		bucket.AddSubscribe(v, t)
	}
	if len(addTopic) == 0 {
		return addTopic, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), managerRPCTimeout)
	defer cancel()
	_, err = topicManageGrpc.SubscribeTopic(ctx, &pb.SubscribeTopicRequest{Topic: addTopic, Ip: ip})
	if err != nil {
		t.mutex.Lock()
		for _, v := range addTopic {
			delete(t.topic, v)
		}
		t.mutex.Unlock()
		for _, v := range addTopic {
			bucket.DelSubscribe(v, t)
		}
		return addTopic, err
	}
	return addTopic, nil
}

func (t *FireTower) unbindTopic(topic []string) ([]string, error) {
	var delTopic []string
	if t == nil {
		return delTopic, errors.New("t is nil")
	}
	if TM == nil {
		return delTopic, errors.New("TM is nil")
	}
	bucket := TM.GetBucket(t)
	t.mutex.Lock()
	for _, v := range topic {
		if t.topic != nil && t.topic[v] {
			delTopic = append(delTopic, v)
			delete(t.topic, v)
		}
	}
	t.mutex.Unlock()
	if bucket != nil {
		for _, v := range delTopic {
			bucket.DelSubscribe(v, t)
		}
	}
	if len(delTopic) == 0 {
		return delTopic, nil
	}
	ip, err := managerIP()
	if err != nil {
		return delTopic, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), managerRPCTimeout)
	defer cancel()
	_, err = topicManageGrpc.UnSubscribeTopic(ctx, &pb.UnSubscribeTopicRequest{Topic: delTopic, Ip: ip})
	return delTopic, err
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
	// 非阻塞发送，防止慢消费者阻塞整个 Bucket 的分发
	select {
	case ch <- message:
		return nil
	default:
		// 缓冲区满，丢弃消息，防止阻塞上游
		// 在生产环境中，这里应该增加一个 Metric 指标，记录丢弃的消息数量
		if TowerLogger != nil {
			TowerLogger(t, "WARN", "send buffer full, message dropped")
		}
		// 返回错误让上层知道发送失败
		return errors.New("send buffer full")
	}
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
	var topics []string
	for k := range t.topic {
		topics = append(topics, k)
	}
	ws := t.ws
	stop := t.closeChan
	offline := t.onOfflineHandler
	unsub := t.unSubscribeHandler
	t.mutex.Unlock()

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
				unsub(fire.Context, delTopic)
			}
		}
	}
	if offline != nil {
		offline()
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
			if message.MessageType == 0 {
				message.MessageType = 1 // 文本格式
			}
			if err := t.ws.WriteMessage(message.MessageType, []byte(message.Data)); err != nil {
				if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					message.Info(fmt.Sprintf("websocket closed while sending: %v", err))
				} else {
					message.Panic(err.Error())
				}
				goto collapse
			}
		case <-heartTicker.C:
			if t.ws == nil {
				goto collapse
			}
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
		return t.ws.SetReadDeadline(time.Now().Add(wait))
	})
	for {
		_ = t.ws.SetReadDeadline(time.Now().Add(wait))
		messageType, data, err := t.ws.ReadMessage()
		if err != nil { // 断开连接
			goto collapse // 出现问题烽火台直接坍塌
		}
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
				if TowerLogger != nil {
					TowerLogger(t, "ERROR", fmt.Sprintf("read message failed:%v", err))
				}
			}
			t.Close()
			return
		} else if t.isClose {
			return
		} else {
			if fire == nil || fire.Message == nil {
				continue
			}
			if fire.Message.Topic == "" {
				fire.Panic(fmt.Sprintf("%s:topic is empty, ClintId:%s, UserId:%s", fire.Message.Type, t.ClientId, t.UserId))
				continue
			}
			switch fire.Message.Type {
			case "subscribe": // 客户端订阅topic
				addTopic := strings.Split(fire.Message.Topic, ",")
				// 如果设置了订阅前触发事件则调用
				// 如果设置了订阅前触发事件则调用
				if t.beforeSubscribeHandler != nil {
					var ok bool
					addTopic, ok = t.beforeSubscribeHandler(fire.Context, addTopic)
					if !ok {
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
				delTopic := strings.Split(fire.Message.Topic, ",")
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
			fire.Info("Extinguished")
			fire.Recycling()
		}
	}
}

// Publish 推送接口
// 通过BuildTower生成的实例都可以调用该方法来达到推送的目的
func (t *FireTower) Publish(fire *FireInfo) error {
	err := topicManage.Publish(fire.Context.id, "user", fire.Message.Topic, fire.Message.Data)
	if err != nil {
		fire.Panic(fmt.Sprintf("publish err: %v", err))
		return err
	}
	return nil
}
func (f *FireInfo) GetContextId() string {
	if f == nil || f.Context == nil {
		return ""
	}
	if strings.TrimSpace(f.Context.id) == "" {
		if IdWorker != nil {
			f.Context.id = strconv.FormatInt(IdWorker.GetId(), 10)
		} else {
			f.Context.id = strconv.FormatInt(time.Now().UnixNano(), 10)
		}
	}
	return f.Context.id
}

func configInt(key string, fallback int) int {
	if ConfigTree == nil {
		return fallback
	}
	v, ok := ConfigTree.Get(key).(int64)
	if !ok || v <= 0 {
		return fallback
	}
	return int(v)
}

func heartbeatInterval() time.Duration {
	return time.Duration(configInt("heartbeat", 30)) * time.Second
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
	if t == nil || t.ws == nil || t.isClose {
		return ErrorClose
	}
	return t.ws.WriteMessage(websocket.TextMessage, b)
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
	res, err := topicManageGrpc.CheckTopicExist(context.Background(), &pb.CheckTopicExistRequest{Topic: topic})
	if err != nil {
		return false
	}
	return res.Ok
}

// SetOnConnectHandler 建立连接事件
func (t *FireTower) SetOnConnectHandler(fn func() bool) {
	t.onConnectHandler = fn
}

// SetOnOfflineHandler 用户连接关闭时触发
func (t *FireTower) SetOnOfflineHandler(fn func()) {
	t.onOfflineHandler = fn
}

// SetReadHandler 客户端推送事件
// 接收到用户publish的消息时触发
func (t *FireTower) SetReadHandler(fn func(*FireInfo) bool) {
	t.readHandler = fn
}

// SetSubscribeHandler 订阅事件
// 用户订阅topic后触发
func (t *FireTower) SetSubscribeHandler(fn func(context *FireLife, topic []string) bool) {
	t.subscribeHandler = fn
}

// SetUnSubscribeHandler 取消订阅事件
// 用户取消订阅topic后触发
func (t *FireTower) SetUnSubscribeHandler(fn func(context *FireLife, topic []string) bool) {
	t.unSubscribeHandler = fn
}

// SetBeforeSubscribeHandler 订阅前回调事件
// 用户订阅topic前触发
func (t *FireTower) SetBeforeSubscribeHandler(fn func(context *FireLife, topic []string) ([]string, bool)) {
	t.beforeSubscribeHandler = fn
}

// SetReadTimeoutHandler 超时回调
// readIn channal写满了  生产 > 消费的情况下触发超时机制
func (t *FireTower) SetReadTimeoutHandler(fn func(*FireInfo)) {
	t.readTimeoutHandler = fn
}

// SetOnSystemRemove 系统移除某个用户的topic订阅
func (t *FireTower) SetOnSystemRemove(fn func(topic string)) {
	t.onSystemRemove = fn
}

// GetConnectNum 获取话题订阅数的grpc方法封装
func (t *FireTower) GetConnectNum(topic string) int64 {
	res, err := topicManageGrpc.GetConnectNum(context.Background(), &pb.GetConnectNumRequest{Topic: topic})
	if err != nil {
		return 0
	}
	return res.Number
}
