package socket

import (
	"errors"
	"fmt"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
	"time"

	json "github.com/json-iterator/go"
)

const (
	// PublishKey 与前端(客户端约定的推送关键字)
	PublishKey = "publish"
	// StateKey 是可丢弃的状态事件。默认每个订阅者都收；标成被动的连接不收。
	StateKey = "state"
	// OfflineTopicByUserIdKey 踢除，将用户某个topic踢下线
	OfflineTopicByUserIdKey = "offline_topic_by_userid"
	// OfflineTopicKey 针对某个topic进行踢除
	OfflineTopicKey = "offline_topic"
	// OfflineUserKey 将某个用户踢下线
	OfflineUserKey = "offline_user"
)

// TcpClient tcp客户端结构体
type TcpClient struct {
	Address      string
	isClose      bool
	closeChan    chan struct{}
	Conn         net.Conn
	readIn       chan *SendMessage
	sendOut      chan []byte
	mutex        sync.Mutex
	manualClose  bool
	reconnecting bool
	generation   uint64
	dialID       uint64
	onConnect    func()
	routeKey     string
	handshakeOK  atomic.Bool
	readyKnown   atomic.Bool
	ready        atomic.Bool
}

// HandshakeTopic 是 Gateway 向 Manager 上报路由键的保留 Topic。旧 Manager 没有订阅者，会把它丢掉。
const HandshakeTopic = "_firetower_handshake"

// EpochTopic 只出现在订阅重放的最后一块里，告诉 Manager 这次快照的版本上界。
const EpochTopic = "_firetower_epoch"

// managerReadTimeout 是 Gateway 读 Manager TCP 的上限，单位纳秒。Manager 心跳 10 秒，3 个间隔没收到任何字节就按断线处理。
var managerReadTimeout atomic.Int64

func init() {
	managerReadTimeout.Store(int64(30 * time.Second))
}

func readTimeout() time.Duration {
	return time.Duration(managerReadTimeout.Load())
}

// PushMessage 推送消息结构体
type PushMessage struct {
	MessageId string `json:"message_id"`
	Source    string `json:"source"`
	Topic     string `json:"topic"`
	Data      []byte `json:"data"`
	Type      string `json:"type"`
}

// SendMessage 发送的消息结构体
// 发送不用限制用户消息内容的格式
type SendMessage struct {
	Context     *sendLife
	Type        string `json:"type"`
	MessageType int
	Data        json.RawMessage `json:"data"`
	Topic       string
}

type sendLife struct {
	StartTime time.Time
	Id        string
	Source    string
}

// GetSendMessage 创建一条发送记录。每次都是新对象，不从池里取。
// id 记录消息id
// source 记录这条信息是用户推送还是平台推送 user | platform
func GetSendMessage(id, source string) *SendMessage {
	return &SendMessage{
		Context: &sendLife{
			StartTime: time.Now(),
			Id:        id,
			Source:    source,
		},
	}
}

func init() {
	SendLogger = sendLog
}

// NewClient 实例化一个tcp客户端
func NewClient(address string) *TcpClient {
	return &TcpClient{
		Address: address,
		isClose: false,
		readIn:  make(chan *SendMessage, 1024),
		sendOut: make(chan []byte, 1024),
	}
}

// CurrentAddr 返回当前连接的本地地址。重连替换连接时持锁读取。
func (t *TcpClient) CurrentAddr() string {
	if t == nil {
		return ""
	}
	t.mutex.Lock()
	defer t.mutex.Unlock()
	if t.Conn == nil || t.Conn.LocalAddr() == nil {
		return ""
	}
	return t.Conn.LocalAddr().String()
}

// dialTCP 是连接入口，测试用来插入可暂停的连接以复现旧读协程误关新连接。
type dialFunc func(network, address string) (net.Conn, error)

var dialTCP atomic.Value

func init() {
	dialTCP.Store(dialFunc(net.Dial))
}

func dial(network, address string) (net.Conn, error) {
	fn, _ := dialTCP.Load().(dialFunc)
	if fn == nil {
		return net.Dial(network, address)
	}
	return fn(network, address)
}

// Connect 建立tcp连接。失败时不启动读写协程。
func (t *TcpClient) Connect() error {
	if t == nil {
		return errors.New("nil tcp client")
	}
	if t.Address == "" {
		return errors.New("empty address")
	}
	lis, err := dial("tcp", t.Address)
	if err != nil {
		return err
	}
	t.mutex.Lock()
	if t.manualClose {
		_ = lis.Close()
		t.mutex.Unlock()
		return ErrorClose
	}
	t.swapConnLocked(lis)
	fn := t.onConnect
	t.mutex.Unlock()
	fmt.Println("[topic manager] connected:", t.Address)
	if fn != nil {
		fn()
	}
	return nil
}

// SetOnConnect 在每次 TCP 连接建立后调用。回调里不要再抢这把锁。
func (t *TcpClient) SetOnConnect(fn func()) {
	if t == nil {
		return
	}
	t.mutex.Lock()
	t.onConnect = fn
	t.mutex.Unlock()
}

// swapConnLocked 替换底层连接，并使上一代读写协程失效。
func (t *TcpClient) swapConnLocked(lis net.Conn) {
	t.disarmLocked()
	t.generation++
	t.routeKey = ""
	t.handshakeOK.Store(false)
	t.readyKnown.Store(false)
	t.ready.Store(false)
	gen := t.generation
	t.isClose = false
	stop := make(chan struct{})
	t.closeChan = stop
	t.Conn = lis
	go t.writeLoop(lis, stop, gen)
	go t.readLoopConn(lis, stop, gen)
}

// disarmLocked 关闭当前连接并作废它的协程。调用方必须持有 mutex。
func (t *TcpClient) disarmLocked() {
	t.generation++
	t.isClose = true
	t.routeKey = ""
	t.handshakeOK.Store(false)
	t.readyKnown.Store(false)
	t.ready.Store(false)
	if t.Conn != nil {
		_ = t.Conn.Close()
		t.Conn = nil
	}
	if t.closeChan != nil {
		select {
		case <-t.closeChan:
		default:
			close(t.closeChan)
		}
	}
}

// Close 关闭当前连接并在未 Shutdown 时重连。同一时刻只保留一个重连协程。
func (t *TcpClient) Close() {
	if t == nil {
		return
	}
	fmt.Println("socket close")
	t.mutex.Lock()
	if t.manualClose {
		t.disarmLocked()
		t.mutex.Unlock()
		return
	}
	t.disarmLocked()
	t.startDialLocked()
	t.mutex.Unlock()
}

// Shutdown 永久关闭客户端，不进行重连，并唤醒阻塞中的 Read。
func (t *TcpClient) Shutdown() {
	if t == nil {
		return
	}
	t.mutex.Lock()
	t.manualClose = true
	t.reconnecting = false
	t.disarmLocked()
	t.mutex.Unlock()
}

func (t *TcpClient) startDialLocked() {
	if t.manualClose || t.reconnecting {
		return
	}
	t.reconnecting = true
	t.dialID++
	go t.dialLoop(t.dialID)
}

func (t *TcpClient) dialLoop(id uint64) {
	defer func() {
		panicked := recover()
		if panicked != nil {
			fmt.Printf("PANIC: tcp dial loop recovered: %v\n", panicked)
		}
		t.mutex.Lock()
		same := t.dialID == id
		manual := t.manualClose
		if same {
			t.reconnecting = false
		}
		t.mutex.Unlock()
		if panicked != nil && same && !manual {
			time.Sleep(retryDelay(0))
			t.mutex.Lock()
			t.startDialLocked()
			t.mutex.Unlock()
		}
	}()
	attempt := 0
	for {
		t.mutex.Lock()
		stop := t.manualClose
		t.mutex.Unlock()
		if stop {
			return
		}
		err := t.Connect()
		if errors.Is(err, ErrorClose) {
			return
		}
		if err == nil {
			attempt = 0
			t.mutex.Lock()
			stopCh := t.closeChan
			manual := t.manualClose
			t.mutex.Unlock()
			if manual || stopCh == nil {
				return
			}
			<-stopCh
			if !t.waitRetry(retryDelay(0)) {
				return
			}
			continue
		}
		fmt.Println("[topic manager] wait topic manager online", t.Address)
		delay := retryDelay(attempt)
		if attempt < 20 {
			attempt++
		}
		if !t.waitRetry(delay) {
			return
		}
	}
}

const (
	dialBase = 200 * time.Millisecond
	dialCap  = 30 * time.Second
)

func retryDelay(attempt int) time.Duration {
	d := dialBase
	for i := 0; i < attempt; i++ {
		if d >= dialCap {
			d = dialCap
			break
		}
		d *= 2
		if d > dialCap {
			d = dialCap
		}
	}
	span := int64(d / 2)
	if span <= 0 {
		return d
	}
	return d/2 + time.Duration(rand.Int63n(span))
}

func (t *TcpClient) waitRetry(delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	pulse := time.NewTicker(50 * time.Millisecond)
	defer pulse.Stop()
	for {
		select {
		case <-timer.C:
			t.mutex.Lock()
			stop := t.manualClose
			t.mutex.Unlock()
			return !stop
		case <-pulse.C:
			t.mutex.Lock()
			stop := t.manualClose
			t.mutex.Unlock()
			if stop {
				return false
			}
		}
	}
}

func (t *TcpClient) onConnLost(gen uint64) {
	if t == nil {
		return
	}
	t.mutex.Lock()
	if t.manualClose || gen != t.generation {
		t.mutex.Unlock()
		return
	}
	t.disarmLocked()
	t.startDialLocked()
	t.mutex.Unlock()
}

func (t *TcpClient) writeLoop(conn net.Conn, stop <-chan struct{}, gen uint64) {
	defer func() {
		if err := recover(); err != nil {
			fmt.Printf("PANIC: tcp client send loop recovered: %v\n", err)
			t.onConnLost(gen)
		}
	}()
	for {
		select {
		case <-stop:
			return
		case message, ok := <-t.sendOut:
			if !ok {
				return
			}
			if conn == nil {
				t.onConnLost(gen)
				return
			}
			if _, err := conn.Write(message); err != nil {
				t.onConnLost(gen)
				return
			}
		}
	}
}

func (t *TcpClient) readLoopConn(conn net.Conn, stop <-chan struct{}, gen uint64) {
	defer func() {
		if err := recover(); err != nil {
			fmt.Printf("PANIC: tcp client read loop recovered: %v\n", err)
			t.onConnLost(gen)
		}
	}()
	if conn == nil {
		t.onConnLost(gen)
		return
	}
	var overflow []byte
	for {
		if timeout := readTimeout(); timeout > 0 {
			_ = conn.SetReadDeadline(time.Now().Add(timeout))
		}
		msg := make([]byte, 1024*16)
		n, err := conn.Read(msg)
		if err != nil {
			t.onConnLost(gen)
			return
		}
		if t.readIn == nil {
			t.onConnLost(gen)
			return
		}
		overflow, err = Depack(append(overflow, msg[:n]...), t.readIn)
		if err != nil {
			fmt.Println("[manager client] depack error:", err)
		}
		select {
		case <-stop:
			return
		default:
		}
	}
}

// Read 从tcp通道中读取消息。Shutdown 或连接替换会把它唤醒。
func (t *TcpClient) Read() (*SendMessage, error) {
	if t == nil || t.readIn == nil {
		return nil, ErrorClose
	}
	for {
		t.mutex.Lock()
		stop := t.closeChan
		manual := t.manualClose
		t.mutex.Unlock()
		if manual && stop == nil {
			return nil, ErrorClose
		}
		select {
		case message, ok := <-t.readIn:
			if !ok || message == nil {
				if manual {
					return nil, ErrorClose
				}
				continue
			}
			if t.noteControl(message) {
				continue
			}
			return message, nil
		case <-stop:
			return nil, ErrorClose
		}
	}
}

func (t *TcpClient) noteControl(message *SendMessage) bool {
	if t == nil || message == nil {
		return false
	}
	switch message.Type {
	case "heartbeat":
		body := string(message.Data)
		t.readyKnown.Store(true)
		t.ready.Store(body == "ready" || body == "heartbeat")
		return true
	case "handshake_ack":
		t.handshakeOK.Store(true)
		return true
	default:
		return false
	}
}

// ApplyHeartbeat 记下 Manager 心跳的正文。"ready" 和 "heartbeat" 可以接收发布，"wait" 不行。
func (t *TcpClient) ApplyHeartbeat(body string) {
	if t == nil {
		return
	}
	t.noteControl(&SendMessage{Type: "heartbeat", Data: []byte(body)})
}

// Publishable 还没收到心跳时按可发布处理，避免唯一的 Manager 在第一跳心跳前被跳过。
// 收到 "wait" 之后不再把它当作首选发布目标。一台都不是 ready 时，Gateway 仍会退回任意一台连得上的。
func (t *TcpClient) Publishable() bool {
	if t == nil {
		return false
	}
	if !t.readyKnown.Load() {
		return true
	}
	return t.ready.Load()
}

// Generation 返回当前连接代数。握手键用它区分重连前后。
func (t *TcpClient) Generation() uint64 {
	if t == nil {
		return 0
	}
	t.mutex.Lock()
	defer t.mutex.Unlock()
	return t.generation
}

// RouteKey 返回握手成功后的路由键。没有握手时是空字符串。
func (t *TcpClient) RouteKey() string {
	if t == nil {
		return ""
	}
	t.mutex.Lock()
	defer t.mutex.Unlock()
	return t.routeKey
}

// SetRouteKey 记下 Manager 已确认的路由键。
func (t *TcpClient) SetRouteKey(key string) {
	if t == nil {
		return
	}
	t.mutex.Lock()
	t.routeKey = key
	t.mutex.Unlock()
}

// SendPacked 把已经封好的帧写进发送队列。
func (t *TcpClient) SendPacked(message []byte) error {
	return t.send(message)
}

// WaitHandshake 等待 Manager 的握手确认。超时返回 false，调用方继续用本地地址。
func (t *TcpClient) WaitHandshake(timeout time.Duration) bool {
	if t == nil {
		return false
	}
	if t.handshakeOK.Load() {
		return true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	pulse := time.NewTicker(10 * time.Millisecond)
	defer pulse.Stop()
	for {
		if t.handshakeOK.Load() {
			return true
		}
		select {
		case <-timer.C:
			return t.handshakeOK.Load()
		case <-pulse.C:
		}
	}
}

func (t *TcpClient) send(message []byte) error {
	if t == nil || t.sendOut == nil {
		return ErrorClose
	}
	// isClose 由重连协程在 mutex 下改写，这里也要在锁下读。
	t.mutex.Lock()
	closed := t.isClose
	t.mutex.Unlock()
	if closed {
		return ErrorClose
	}
	// 设置一秒超时
	ticker := time.NewTicker(time.Duration(3) * time.Second)
	for {
		select {
		case t.sendOut <- message:
			ticker.Stop()
			return nil
		case <-ticker.C:
			fmt.Println("[topic manager] send timeout bytes:", len(message))
			ticker.Stop()
			return ErrorBlock
		}
	}
}

// Publish 通过tcp来进行推送的方法
func (t *TcpClient) Publish(messageId, source, topic string, data json.RawMessage) error {
	if t == nil {
		return errors.New("nil tcp client")
	}
	b, err := Enpack(PublishKey, messageId, source, topic, data)
	if err != nil {
		return err
	}
	return t.send(b)
}

// OnPush 当有新的推送消息到达tcp客户端时触发。
// 单次连接断开只表示这一代连接结束。重连后的消息仍要继续交给回调，直到 Shutdown。
func (t *TcpClient) OnPush(fn func(message *SendMessage)) {
	if t == nil || fn == nil {
		return
	}
	go func() {
		for {
			stop := false
			func() {
				defer func() {
					if err := recover(); err != nil {
						fmt.Printf("PANIC: OnPush callback runner recovered: %v\n", err)
					}
				}()
				for {
					message, err := t.Read()
					if err != nil {
						if message != nil {
							message.Panic(err.Error())
						}
						t.mutex.Lock()
						manual := t.manualClose
						t.mutex.Unlock()
						if manual {
							stop = true
							return
						}
						time.Sleep(50 * time.Millisecond)
						continue
					}
					fn(message)
				}
			}()
			if stop {
				return
			}
			t.mutex.Lock()
			manual := t.manualClose
			t.mutex.Unlock()
			if manual {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
}
