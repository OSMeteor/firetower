package manager

import (
	"container/list"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/OSMeteor/firetower/grpc/manager"
	"github.com/OSMeteor/firetower/socket"
)

// 阶段 0：坏连接、坏路由值和并发登记不能把 Manager 进程或 Accept 循环打崩。

func TestStage0_NilAndCorruptRoutesDoNotPanic(t *testing.T) {
	prev := currentLogger()
	setLogger(func(string, string) {})
	t.Cleanup(func() { setLogger(prev) })
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil route panicked: %v", r)
		}
	}()

	var bucket *connectBucket
	bucket.relation()
	(&connectBucket{}).relation()
	(&connectBucket{conn: nilAddrConn{fakeConn{addr: fakeAddr("stage0-nil")}}}).relation()

	topic := fmt.Sprintf("stage0-bad-%d", time.Now().UnixNano())
	topicRelevance.Store(topic, "not-a-list")
	t.Cleanup(func() { topicRelevance.Delete(topic) })

	svc := &topicGrpcService{}
	if _, err := svc.Publish(context.Background(), &pb.PublishRequest{Topic: topic, Data: []byte("x")}); err == nil {
		t.Fatal("corrupt route was treated as a live topic")
	}
	if _, err := svc.GetConnectNum(context.Background(), &pb.GetConnectNumRequest{Topic: topic}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CheckTopicExist(context.Background(), &pb.CheckTopicExistRequest{Topic: topic}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SubscribeTopic(context.Background(), &pb.SubscribeTopicRequest{Topic: []string{topic, ""}, Ip: ""}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UnSubscribeTopic(context.Background(), &pb.UnSubscribeTopicRequest{Topic: []string{topic}, Ip: "missing"}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/topic", nil)
	topicWebHandler(httptest.NewRecorder(), req)
	topicWebHandler(nil, req)
}

func TestStage0_FailedWriteDoesNotPanic(t *testing.T) {
	prev := currentLogger()
	setLogger(func(string, string) {})
	t.Cleanup(func() { setLogger(prev) })

	topic := fmt.Sprintf("stage0-write-%d", time.Now().UnixNano())
	const ip = "10.8.8.8:8"
	store := list.New()
	store.PushBack(&topicRelevanceItem{ip: ip, num: 1})
	topicRelevance.Store(topic, store)
	t.Cleanup(func() {
		topicRelevance.Delete(topic)
		ConnIndexTable.Delete(ip)
	})

	conn := &connectBucket{
		conn:       errConn{fakeConn{addr: fakeAddr(ip)}},
		packetChan: make(chan *socket.SendMessage, 2),
		closeChan:  make(chan struct{}),
	}
	ConnIndexTable.Store(ip, conn)
	go conn.sendLoop()

	empty := socket.GetSendMessage("0", "user")
	empty.Context = nil
	conn.packetChan <- nil
	conn.packetChan <- empty
	msg := socket.GetSendMessage("1", "user")
	msg.Type = socket.PublishKey
	msg.Topic = topic
	msg.Data = []byte("x")
	conn.packetChan <- msg

	select {
	case <-conn.closeChan:
	case <-time.After(time.Second):
		t.Fatal("failed gateway write did not close that connection")
	}
}

func TestStage0_SubscribeRacesDoNotPanic(t *testing.T) {
	prev := currentLogger()
	setLogger(func(string, string) {})
	t.Cleanup(func() { setLogger(prev) })

	prefix := fmt.Sprintf("stage0-race-%d-", time.Now().UnixNano())
	t.Cleanup(func() {
		topicRelevance.Range(func(key, _ any) bool {
			name, ok := key.(string)
			if ok && strings.HasPrefix(name, prefix) {
				topicRelevance.Delete(key)
			}
			return true
		})
	})

	svc := &topicGrpcService{}
	var failed atomic.Bool
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() {
				if recover() != nil {
					failed.Store(true)
				}
			}()
			topic := fmt.Sprintf("%s%d", prefix, i%4)
			ip := fmt.Sprintf("10.7.0.%d:7", i)
			for n := 0; n < 30; n++ {
				_, _ = svc.SubscribeTopic(context.Background(), &pb.SubscribeTopicRequest{Topic: []string{topic}, Ip: ip})
				_, _ = svc.GetConnectNum(context.Background(), &pb.GetConnectNumRequest{Topic: topic})
				_, _ = svc.Publish(context.Background(), &pb.PublishRequest{Topic: topic, Data: []byte("x"), MessageId: "1"})
				_, _ = svc.UnSubscribeTopic(context.Background(), &pb.UnSubscribeTopicRequest{Topic: []string{topic}, Ip: ip})
				_, _ = svc.CheckTopicExist(context.Background(), &pb.CheckTopicExistRequest{Topic: topic})
			}
		}(i)
	}
	wg.Wait()
	if failed.Load() {
		t.Fatal("concurrent subscribe and publish panicked")
	}
}

func TestStage0_AcceptLoopSurvivesBadConnection(t *testing.T) {
	prev := currentLogger()
	setLogger(func(string, string) {})
	t.Cleanup(func() { setLogger(prev) })

	bound := make(chan net.Addr, 1)
	stop := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-stop:
		default:
			close(stop)
		}
	})
	go serveSocket("127.0.0.1:0", stop, bound)
	var addr net.Addr
	select {
	case addr = <-bound:
	case <-time.After(2 * time.Second):
		t.Fatal("manager did not start listening")
	}
	first, err := net.DialTimeout("tcp", addr.String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	junk := append([]byte("junk"), append([]byte(socket.ConstHeader), socket.IntToBytes(-1)...)...)
	good, err := socket.Enpack("publish", "1", "src", "stage0-room", []byte("hi"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = first.Write(append(junk, good...)); err != nil {
		t.Fatal(err)
	}
	_ = first.Close()

	second, err := net.DialTimeout("tcp", addr.String(), time.Second)
	if err != nil {
		t.Fatal("accept loop died after a bad connection")
	}
	_ = second.Close()
}

func TestStage0_GrpcPanicDoesNotEscape(t *testing.T) {
	prev := currentLogger()
	setLogger(func(string, string) {})
	t.Cleanup(func() { setLogger(prev) })
	_, err := grpcRecoverUnary(context.Background(), nil, nil, func(context.Context, interface{}) (interface{}, error) {
		panic("handler")
	})
	if err == nil {
		t.Fatal("handler panic was returned as success")
	}
}

type nilAddrConn struct{ fakeConn }

func (nilAddrConn) RemoteAddr() net.Addr { return nil }

type errConn struct{ fakeConn }

func (errConn) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
