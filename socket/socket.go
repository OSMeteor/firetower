package socket

import (
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
	Address   string
	isClose   bool
	closeChan chan struct{}
	Conn      net.Conn
	readIn      chan *SendMessage
	sendOut     chan []byte
	mutex       sync.Mutex
	manualClose bool
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

// Connect 建立tcp连接
func (t *TcpClient) Connect() error {
	lis, err := net.Dial("tcp", t.Address)
	if err != nil {
		return err
	}
	t.isClose = false
	t.closeChan = make(chan struct{})
	t.Conn = lis

	go func() {
		defer func() {
			if err := recover(); err != nil {
				fmt.Printf("PANIC: tcp client send loop recovered: %v\n", err)
			}
		}()
		for {
			select {
			case message := <-t.sendOut:
				if _, err := lis.Write(message); err != nil {
					goto close
				}
			case <-t.closeChan:
				return
			}
		}
	close:
		t.Close()
	}()

	// read channal
	go func() {
		defer func() {
			if err := recover(); err != nil {
				fmt.Printf("PANIC: tcp client read loop recovered: %v\n", err)
			}
		}()
		var overflow []byte
		for {
			var msg = make([]byte, 1024*16)

			l, err := lis.Read(msg)
			if err != nil {
				if !t.isClose {
					t.Close()
				}
				return
			}
			overflow, err = Depack(append(overflow, msg[:l]...), t.readIn)
			if err != nil {
				fmt.Println("[manager client] depack error:", err)
			}
			select {
			case <-t.closeChan:
				if !t.isClose {
					t.Close()
					return
				}
			default:
			}
		}
	}()

	return nil
}

// Close 关闭tcp连接
func (t *TcpClient) Close() {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	if !t.isClose {
		fmt.Println("socket close")
		t.isClose = true
		t.Conn.Close()
		close(t.closeChan)
	Retry:
		// Since Connect calls a goroutine that might access t.isClose, and Close calls Connect...
		// Connect itself does not block significantly except for Dial.
		// NOTE: Calling Connect() inside Lock() might be dangerous if Connect() takes a long time (Dial).
		// But Connect() launches goroutines.
		// Ideally we should unlock before Connect, but we need to ensure state is consistent.
		if t.manualClose {
			return
		}
		err := t.Connect()
		if err != nil {
			fmt.Println("[topic manager] wait topic manager online", t.Address)
			t.mutex.Unlock() // Unlock while sleeping
			time.Sleep(time.Duration(1) * time.Second)
			t.mutex.Lock() // Re-lock
			goto Retry
		} else {
			fmt.Println("[topic manager] connected:", t.Address)
		}
	}
}

// Shutdown 永久关闭客户端，不进行重连
func (t *TcpClient) Shutdown() {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	t.manualClose = true
	t.isClose = true
	if t.Conn != nil {
		t.Conn.Close()
	}
	// closeChan might already be closed if Close() was called concurrently?
	// If Close() was called, isClose is true, manualClose is true.
	// We want to ensure any blocking Read/Send returns.
}

// Read 从tcp通道中读取消息
func (t *TcpClient) Read() (*SendMessage, error) {
	if t.isClose {
		return nil, ErrorClose
	}
	for {
		message := <-t.readIn
		if string(message.Type) == "heartbeat" {
			continue
		}
		return message, nil
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

// OnPush 当有新的推送消息到达tcp客户端时触发
func (t *TcpClient) OnPush(fn func(message *SendMessage)) {
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
				// 只可能是连接断开了
				return
			}
			fn(message)
		}
	}()
}
