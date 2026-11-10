package socket

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	json "github.com/json-iterator/go"
)

const (
	// PublishKey 与前端(客户端约定的推送关键字)
	PublishKey = "publish"
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

var (
	sendPool sync.Pool
)

// GetSendMessage 创建一条发送记录
// id 记录消息id
// source 记录这条信息是用户推送还是平台推送 user | platform
func GetSendMessage(id, source string) *SendMessage {
	sendMessage := sendPool.Get().(*SendMessage)
	sendMessage.Context.StartTime = time.Now()
	sendMessage.Context.Id = id
	sendMessage.Context.Source = source
	return sendMessage
}

func init() {
	sendPool.New = func() interface{} {
		return &SendMessage{
			Context: new(sendLife),
		}
	}
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
	t.mutex.Lock()
	defer t.mutex.Unlock()
	if t.Conn == nil || t.Conn.LocalAddr() == nil {
		return ""
	}
	return t.Conn.LocalAddr().String()
}

// dialTCP 是连接入口，测试用来插入可暂停的连接以复现旧读协程误关新连接。
var dialTCP = func(network, address string) (net.Conn, error) {
	return net.Dial(network, address)
}

// Connect 建立tcp连接。失败时不启动读写协程。
func (t *TcpClient) Connect() error {
	if t == nil {
		return errors.New("nil tcp client")
	}
	if t.Address == "" {
		return errors.New("empty address")
	}
	lis, err := dialTCP("tcp", t.Address)
	if err != nil {
		return err
	}
	t.mutex.Lock()
	defer t.mutex.Unlock()
	if t.manualClose {
		_ = lis.Close()
		return ErrorClose
	}
	t.swapConnLocked(lis)
	fmt.Println("[topic manager] connected:", t.Address)
	return nil
}

// swapConnLocked 替换底层连接，并使上一代读写协程失效。
func (t *TcpClient) swapConnLocked(lis net.Conn) {
	t.disarmLocked()
	t.generation++
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
		if err := recover(); err != nil {
			fmt.Printf("PANIC: tcp dial loop recovered: %v\n", err)
		}
		t.mutex.Lock()
		if t.dialID == id {
			t.reconnecting = false
		}
		t.mutex.Unlock()
	}()
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
			t.mutex.Lock()
			stopCh := t.closeChan
			manual := t.manualClose
			t.mutex.Unlock()
			if manual || stopCh == nil {
				return
			}
			<-stopCh
			if !t.waitRetry() {
				return
			}
			continue
		}
		fmt.Println("[topic manager] wait topic manager online", t.Address)
		timer := time.NewTimer(time.Second)
		t.mutex.Lock()
		stop = t.manualClose
		t.mutex.Unlock()
		if stop {
			timer.Stop()
			return
		}
		<-timer.C
	}
}

func (t *TcpClient) waitRetry() bool {
	timer := time.NewTimer(time.Second)
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
			if message.Type == "heartbeat" {
				continue
			}
			return message, nil
		case <-stop:
			return nil, ErrorClose
		}
	}
}

func (t *TcpClient) send(message []byte) error {
	if t.isClose {
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
			fmt.Println("[topic manager] send timeout:", message)
			ticker.Stop()
			return ErrorBlock
		}
	}
}

// Publish 通过tcp来进行推送的方法
func (t *TcpClient) Publish(messageId, source, topic string, data json.RawMessage) error {
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
					return
				}
				time.Sleep(50 * time.Millisecond)
				continue
			}
			fn(message)
		}
	}()
}
