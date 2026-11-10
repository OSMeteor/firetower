package manager

import (
	"container/list"
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	pb "github.com/OSMeteor/firetower/grpc/manager"
	"github.com/OSMeteor/firetower/socket"
)

func TestAudit_NilInputsDoNotPanic(t *testing.T) {
	defer func() {
		if recover() != nil {
			t.Fatal("nil input panicked")
		}
	}()
	var svc *topicGrpcService
	if _, err := svc.Publish(context.Background(), nil); err == nil {
		t.Fatal("nil service Publish returned success")
	}
	if _, err := (&topicGrpcService{}).Publish(context.Background(), nil); err == nil {
		t.Fatal("nil publish request returned success")
	}
	var bucket *connectBucket
	bucket.close()
	bucket.delRelation()
	bucket.heartbeat()
}

func TestAudit_FT04_DelRelationRemovesEveryMatch(t *testing.T) {
	topic := fmt.Sprintf("ft04-%d", time.Now().UnixNano())
	store := list.New()
	const gone = "10.9.9.9:9"
	const keep = "10.9.9.9:8"
	store.PushBack(&topicRelevanceItem{ip: gone, num: 1})
	store.PushBack(&topicRelevanceItem{ip: keep, num: 1})
	store.PushBack(&topicRelevanceItem{ip: gone, num: 2})
	topicRelevance.Store(topic, store)
	t.Cleanup(func() { topicRelevance.Delete(topic) })

	conn := &connectBucket{conn: fakeConn{addr: fakeAddr(gone)}}
	conn.delRelation()

	var left []string
	for e := store.Front(); e != nil; e = e.Next() {
		left = append(left, e.Value.(*topicRelevanceItem).ip)
	}
	if len(left) != 1 || left[0] != keep {
		t.Fatalf("FT-04: remaining subscriptions = %v, want [%s]", left, keep)
	}
}

func TestAudit_FT10_PublishWriteDoesNotBlockSubscribe(t *testing.T) {
	topic := fmt.Sprintf("ft10-%d", time.Now().UnixNano())
	const ip = "10.0.0.1:1"
	store := list.New()
	store.PushBack(&topicRelevanceItem{ip: ip, num: 1})
	topicRelevance.Store(topic, store)
	t.Cleanup(func() { topicRelevance.Delete(topic) })

	release := make(chan struct{})
	blocked := make(chan struct{})
	conn := &connectBucket{conn: &blockingConn{blocked: blocked, release: release}}
	ConnIndexTable.Store(ip, conn)
	t.Cleanup(func() {
		close(release)
		ConnIndexTable.Delete(ip)
	})

	svc := &topicGrpcService{}
	go func() {
		_, _ = svc.Publish(context.Background(), &pb.PublishRequest{
			Topic:     topic,
			Data:      []byte("x"),
			MessageId: "1",
			Source:    "audit",
		})
	}()
	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("FT-10: Publish did not reach the gateway write")
	}

	subDone := make(chan struct{})
	go func() {
		_, _ = svc.SubscribeTopic(context.Background(), &pb.SubscribeTopicRequest{
			Topic: []string{"ft10-other"},
			Ip:    "10.0.0.2:2",
		})
		close(subDone)
	}()
	select {
	case <-subDone:
		topicRelevance.Delete("ft10-other")
	case <-time.After(300 * time.Millisecond):
		t.Fatal("FT-10: SubscribeTopic stayed behind Publish's blocking Write")
	}
}

func TestAudit_FT10_HeartbeatExitsWithConnection(t *testing.T) {
	conn := &connectBucket{
		conn:      fakeConn{addr: fakeAddr("10.2.2.2:2")},
		closeChan: make(chan struct{}),
	}
	done := make(chan struct{})
	go func() {
		conn.heartbeat()
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	conn.close()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("FT-10: heartbeat was still running after the connection closed")
	}
}

func TestAudit_NW06_SlowGatewayDoesNotBlockOthers(t *testing.T) {
	slowTopic := fmt.Sprintf("nw06-slow-%d", time.Now().UnixNano())
	fastTopic := fmt.Sprintf("nw06-fast-%d", time.Now().UnixNano())
	const slowIP = "10.8.8.1:1"
	const fastIP = "10.8.8.2:2"
	slowStore := list.New()
	slowStore.PushBack(&topicRelevanceItem{ip: slowIP, num: 1})
	fastStore := list.New()
	fastStore.PushBack(&topicRelevanceItem{ip: fastIP, num: 1})
	topicRelevance.Store(slowTopic, slowStore)
	topicRelevance.Store(fastTopic, fastStore)
	t.Cleanup(func() {
		topicRelevance.Delete(slowTopic)
		topicRelevance.Delete(fastTopic)
	})

	release := make(chan struct{})
	blocked := make(chan struct{})
	slow := &connectBucket{conn: &blockingConn{blocked: blocked, release: release}}
	fast := &connectBucket{conn: fakeConn{addr: fakeAddr(fastIP)}}
	ConnIndexTable.Store(slowIP, slow)
	ConnIndexTable.Store(fastIP, fast)
	t.Cleanup(func() {
		close(release)
		ConnIndexTable.Delete(slowIP)
		ConnIndexTable.Delete(fastIP)
	})

	svc := &topicGrpcService{}
	go func() {
		_, _ = svc.Publish(context.Background(), &pb.PublishRequest{
			Topic: slowTopic, Data: []byte("slow"), MessageId: "1", Source: "audit",
		})
	}()
	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("NW06: slow gateway write did not start")
	}

	done := make(chan struct{})
	go func() {
		_, _ = svc.Publish(context.Background(), &pb.PublishRequest{
			Topic: fastTopic, Data: []byte("fast"), MessageId: "2", Source: "audit",
		})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("NW06: publish to a healthy gateway waited behind a slow gateway")
	}
}

func TestAudit_CO04_SubscribeRacesForward(t *testing.T) {
	topic := fmt.Sprintf("co04-%d", time.Now().UnixNano())
	left, right := net.Pipe()
	t.Cleanup(func() {
		_ = left.Close()
		_ = right.Close()
		topicRelevance.Delete(topic)
	})
	go func() { _, _ = io.Copy(io.Discard, right) }()

	const ip = "10.1.1.1:1000"
	conn := &connectBucket{
		conn:       left,
		packetChan: make(chan *socket.SendMessage, 64),
		closeChan:  make(chan struct{}),
	}
	ConnIndexTable.Store(ip, conn)
	t.Cleanup(func() {
		conn.close()
		ConnIndexTable.Delete(ip)
	})
	go conn.sendLoop()

	svc := &topicGrpcService{}
	errCh := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				select {
				case errCh <- fmt.Errorf("CO04 panic: %v", r):
				default:
				}
			}
		}()
		for i := 0; i < 100; i++ {
			_, _ = svc.SubscribeTopic(context.Background(), &pb.SubscribeTopicRequest{Topic: []string{topic}, Ip: ip})
			_, _ = svc.UnSubscribeTopic(context.Background(), &pb.UnSubscribeTopicRequest{Topic: []string{topic}, Ip: ip})
		}
	}()
	for i := 0; i < 100; i++ {
		msg := socket.GetSendMessage("1", "user")
		msg.Type = socket.PublishKey
		msg.Topic = topic
		msg.Data = []byte("z")
		select {
		case conn.packetChan <- msg:
		case <-conn.closeChan:
			t.Fatal("CO04: forwarder closed during the race")
		}
	}
	wg.Wait()
	select {
	case err := <-errCh:
		t.Fatal(err)
	default:
	}
}

type fakeAddr string

func (a fakeAddr) Network() string { return "tcp" }
func (a fakeAddr) String() string  { return string(a) }

type fakeConn struct{ addr net.Addr }

func (c fakeConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (c fakeConn) Write([]byte) (int, error)        { return 0, nil }
func (c fakeConn) Close() error                     { return nil }
func (c fakeConn) LocalAddr() net.Addr              { return c.addr }
func (c fakeConn) RemoteAddr() net.Addr             { return c.addr }
func (c fakeConn) SetDeadline(time.Time) error      { return nil }
func (c fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (c fakeConn) SetWriteDeadline(time.Time) error { return nil }

type blockingConn struct {
	blocked chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *blockingConn) Read([]byte) (int, error) { return 0, io.EOF }
func (c *blockingConn) Write([]byte) (int, error) {
	c.once.Do(func() { close(c.blocked) })
	<-c.release
	return 1, nil
}
func (c *blockingConn) Close() error                     { return nil }
func (c *blockingConn) LocalAddr() net.Addr              { return fakeAddr("local") }
func (c *blockingConn) RemoteAddr() net.Addr             { return fakeAddr("remote") }
func (c *blockingConn) SetDeadline(time.Time) error      { return nil }
func (c *blockingConn) SetReadDeadline(time.Time) error  { return nil }
func (c *blockingConn) SetWriteDeadline(time.Time) error { return nil }
