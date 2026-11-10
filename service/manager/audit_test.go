package manager

import (
	"container/list"
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
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

func TestAudit_DeadGatewayDoesNotStopForward(t *testing.T) {
	topic := fmt.Sprintf("dead-%d", time.Now().UnixNano())
	const deadIP = "10.4.4.1:1"
	const liveIP = "10.4.4.2:2"
	store := list.New()
	store.PushBack(&topicRelevanceItem{ip: deadIP, num: 1})
	store.PushBack(&topicRelevanceItem{ip: liveIP, num: 1})
	topicRelevance.Store(topic, store)
	t.Cleanup(func() { topicRelevance.Delete(topic) })

	dead := &connectBucket{conn: failConn{fakeConn{addr: fakeAddr(deadIP)}}, closeChan: make(chan struct{})}
	liveConn := &recordConn{fakeConn: fakeConn{addr: fakeAddr(liveIP)}}
	live := &connectBucket{conn: liveConn, closeChan: make(chan struct{})}
	ConnIndexTable.Store(deadIP, dead)
	ConnIndexTable.Store(liveIP, live)
	t.Cleanup(func() {
		ConnIndexTable.Delete(deadIP)
		ConnIndexTable.Delete(liveIP)
	})

	pub := &connectBucket{
		packetChan: make(chan *socket.SendMessage, 4),
		closeChan:  make(chan struct{}),
	}
	go pub.sendLoop()
	t.Cleanup(func() { pub.close() })

	for i := 0; i < 2; i++ {
		msg := socket.GetSendMessage("m", "user")
		msg.Type = socket.PublishKey
		msg.Topic = topic
		msg.Data = []byte("x")
		select {
		case pub.packetChan <- msg:
		case <-time.After(time.Second):
			t.Fatal("publisher stopped accepting messages")
		}
	}
	deadline := time.Now().Add(time.Second)
	for liveConn.n.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if liveConn.n.Load() != 2 {
		t.Fatalf("forwarder stopped after a dead gateway, live writes=%d", liveConn.n.Load())
	}
}

func TestAudit_CloseDropsConnIndex(t *testing.T) {
	const ip = "10.4.4.3:3"
	first := &connectBucket{conn: fakeConn{addr: fakeAddr(ip)}, closeChan: make(chan struct{})}
	first.relation()
	second := &connectBucket{conn: fakeConn{addr: fakeAddr(ip)}, closeChan: make(chan struct{})}
	second.relation()
	got, ok := ConnIndexTable.Load(ip)
	if !ok || got != second {
		t.Fatal("new connection did not replace the index")
	}
	if !first.isClose {
		t.Fatal("replaced connection was left open")
	}
	second.close()
	if _, ok := ConnIndexTable.Load(ip); ok {
		t.Fatal("closed connection stayed in the index")
	}
}

func TestAudit_NilLoggerRecoverDoesNotEscape(t *testing.T) {
	prev := currentLogger()
	setLogger(nil)
	t.Cleanup(func() { setLogger(prev) })
	c := &connectBucket{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.handler()
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler did not return when Logger was nil")
	}
}

func TestAudit_AcceptErrorDoesNotSpin(t *testing.T) {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		serveSocket("127.0.0.1:0", stop, nil)
	}()
	time.Sleep(50 * time.Millisecond)
	close(stop)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("accept loop did not leave after the listener closed")
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

type failConn struct{ fakeConn }

func (c failConn) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

type recordConn struct {
	fakeConn
	n atomic.Int32
}

func (c *recordConn) Write(p []byte) (int, error) {
	c.n.Add(1)
	return len(p), nil
}

func TestAudit_NW07_FailedWriteDoesNotStopPublisher(t *testing.T) {
	prev := currentLogger()
	setLogger(func(string, string) {})
	t.Cleanup(func() { setLogger(prev) })

	topic := fmt.Sprintf("nw07-%d", time.Now().UnixNano())
	const badIP = "10.4.4.1:1"
	const goodIP = "10.4.4.2:2"
	store := list.New()
	store.PushBack(&topicRelevanceItem{ip: badIP, num: 1})
	store.PushBack(&topicRelevanceItem{ip: goodIP, num: 1})
	topicRelevance.Store(topic, store)
	t.Cleanup(func() {
		topicRelevance.Delete(topic)
		ConnIndexTable.Delete(badIP)
		ConnIndexTable.Delete(goodIP)
	})

	good := &recordConn{fakeConn: fakeConn{addr: fakeAddr(goodIP)}}
	bad := &connectBucket{conn: failConn{fakeConn{addr: fakeAddr(badIP)}}, closeChan: make(chan struct{})}
	goodBucket := &connectBucket{conn: good, closeChan: make(chan struct{})}
	ConnIndexTable.Store(badIP, bad)
	ConnIndexTable.Store(goodIP, goodBucket)

	publisher := &connectBucket{
		conn:       fakeConn{addr: fakeAddr("10.4.4.9:9")},
		packetChan: make(chan *socket.SendMessage, 4),
		closeChan:  make(chan struct{}),
	}
	go publisher.sendLoop()
	t.Cleanup(func() { publisher.close() })

	send := func(body string) {
		t.Helper()
		msg := socket.GetSendMessage("1", "user")
		msg.Type = socket.PublishKey
		msg.Topic = topic
		msg.Data = []byte(body)
		select {
		case publisher.packetChan <- msg:
		case <-publisher.closeChan:
			t.Fatal("publisher send loop exited")
		case <-time.After(time.Second):
			t.Fatal("enqueue timed out")
		}
	}
	send("one")
	send("two")
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if good.n.Load() >= 2 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("NW07: good gateway received %d writes, want 2", good.n.Load())
}

func TestAudit_CO05_ReplacedConnKeepsIndex(t *testing.T) {
	prev := currentLogger()
	setLogger(func(string, string) {})
	t.Cleanup(func() { setLogger(prev) })

	const ip = "10.4.5.1:1"
	t.Cleanup(func() { ConnIndexTable.Delete(ip) })
	old := &connectBucket{conn: fakeConn{addr: fakeAddr(ip)}, closeChan: make(chan struct{})}
	neu := &connectBucket{conn: fakeConn{addr: fakeAddr(ip)}, closeChan: make(chan struct{})}
	old.relation()
	neu.relation()

	if got, ok := ConnIndexTable.Load(ip); !ok || got != neu {
		t.Fatalf("CO05: index = %v, want the replacement", got)
	}
	select {
	case <-old.closeChan:
	default:
		t.Fatal("CO05: replaced connection was left open")
	}
	neu.close()
	if _, ok := ConnIndexTable.Load(ip); ok {
		t.Fatal("CO05: close left its own index in place")
	}
}

func TestAudit_AbsoluteCountOverwritesAndIgnoresStale(t *testing.T) {
	topic := fmt.Sprintf("abs-%d", time.Now().UnixNano())
	const ip = "10.6.6.1:1"
	t.Cleanup(func() { topicRelevance.Delete(topic) })
	svc := &topicGrpcService{}
	req := &pb.SubscribeTopicRequest{Topic: []string{topic}, Ip: ip}
	if _, err := svc.SubscribeTopic(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SubscribeTopic(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got := countTopic(t, svc, topic); got != 2 {
		t.Fatalf("legacy num++ = %d, want 2", got)
	}
	if _, err := svc.SubscribeTopic(context.Background(), &pb.SubscribeTopicRequest{
		Topic: []string{topic}, Ip: ip, Count: []int64{5}, Version: []uint64{3},
	}); err != nil {
		t.Fatal(err)
	}
	if got := countTopic(t, svc, topic); got != 5 {
		t.Fatalf("absolute count = %d, want 5", got)
	}
	if _, err := svc.SubscribeTopic(context.Background(), &pb.SubscribeTopicRequest{
		Topic: []string{topic}, Ip: ip, Count: []int64{9}, Version: []uint64{2},
	}); err != nil {
		t.Fatal(err)
	}
	if got := countTopic(t, svc, topic); got != 5 {
		t.Fatalf("stale version changed count to %d", got)
	}
	if _, err := svc.UnSubscribeTopic(context.Background(), &pb.UnSubscribeTopicRequest{
		Topic: []string{topic}, Ip: ip, Count: []int64{0}, Version: []uint64{4},
	}); err != nil {
		t.Fatal(err)
	}
	if got := countTopic(t, svc, topic); got != 0 {
		t.Fatalf("count after delete = %d", got)
	}
}

func countTopic(t *testing.T, svc *topicGrpcService, topic string) int64 {
	t.Helper()
	res, err := svc.GetConnectNum(context.Background(), &pb.GetConnectNumRequest{Topic: topic})
	if err != nil || res == nil {
		t.Fatalf("count: %v", err)
	}
	return res.Number
}

func TestAudit_FullOutboxDoesNotBlockPublisher(t *testing.T) {
	prev := currentLogger()
	setLogger(func(string, string) {})
	t.Cleanup(func() { setLogger(prev) })

	topic := fmt.Sprintf("box-%d", time.Now().UnixNano())
	const slowIP = "10.6.7.1:1"
	const liveIP = "10.6.7.2:2"
	store := list.New()
	store.PushBack(&topicRelevanceItem{ip: slowIP, num: 1})
	store.PushBack(&topicRelevanceItem{ip: liveIP, num: 1})
	topicRelevance.Store(topic, store)
	t.Cleanup(func() {
		topicRelevance.Delete(topic)
		ConnIndexTable.Delete(slowIP)
		ConnIndexTable.Delete(liveIP)
	})

	slow := &connectBucket{
		conn:      fakeConn{addr: fakeAddr(slowIP)},
		outbox:    make(chan []byte, 1),
		closeChan: make(chan struct{}),
	}
	liveConn := &recordConn{fakeConn: fakeConn{addr: fakeAddr(liveIP)}}
	live := &connectBucket{conn: liveConn, closeChan: make(chan struct{})}
	ConnIndexTable.Store(slowIP, slow)
	ConnIndexTable.Store(liveIP, live)
	pub := &connectBucket{
		conn:       fakeConn{addr: fakeAddr("10.6.7.9:9")},
		packetChan: make(chan *socket.SendMessage, 4),
		closeChan:  make(chan struct{}),
	}
	go pub.sendLoop()
	t.Cleanup(func() { pub.close() })
	for i := 0; i < 3; i++ {
		msg := socket.GetSendMessage("1", "user")
		msg.Type = socket.PublishKey
		msg.Topic = topic
		msg.Data = []byte("x")
		select {
		case pub.packetChan <- msg:
		case <-time.After(200 * time.Millisecond):
			t.Fatal("publisher blocked on a full gateway outbox")
		}
	}
	deadline := time.Now().Add(time.Second)
	for liveConn.n.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if liveConn.n.Load() != 3 {
		t.Fatalf("live gateway writes = %d, want 3", liveConn.n.Load())
	}
}

func TestAudit_ReconcileDropsTopicsMissingFromTheRound(t *testing.T) {
	keep := fmt.Sprintf("keep-%d", time.Now().UnixNano())
	drop := fmt.Sprintf("drop-%d", time.Now().UnixNano())
	extra := fmt.Sprintf("extra-%d", time.Now().UnixNano())
	const ip = "10.8.8.1:1"
	t.Cleanup(func() {
		topicRelevance.Delete(keep)
		topicRelevance.Delete(drop)
		topicRelevance.Delete(extra)
	})
	svc := &topicGrpcService{}
	if _, err := svc.SubscribeTopic(context.Background(), &pb.SubscribeTopicRequest{
		Topic: []string{keep, drop}, Ip: ip, Count: []int64{1, 1}, Version: []uint64{1, 1},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SubscribeTopic(context.Background(), &pb.SubscribeTopicRequest{
		Topic: []string{keep}, Ip: ip, Count: []int64{1}, Version: []uint64{2},
		Round: 7, RoundBegin: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SubscribeTopic(context.Background(), &pb.SubscribeTopicRequest{
		Topic: []string{extra}, Ip: ip, Count: []int64{2}, Version: []uint64{1},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SubscribeTopic(context.Background(), &pb.SubscribeTopicRequest{
		Ip: ip, Round: 7, RoundEnd: true,
	}); err != nil {
		t.Fatal(err)
	}
	if got := countTopic(t, svc, keep); got != 1 {
		t.Fatalf("kept topic count = %d", got)
	}
	if got := countTopic(t, svc, extra); got != 2 {
		t.Fatalf("subscription during reconcile count = %d", got)
	}
	if got := countTopic(t, svc, drop); got != 0 {
		t.Fatalf("topic missing from the round stayed at %d", got)
	}
}

func TestAudit_HandshakeReplacesRemoteAddr(t *testing.T) {
	prev := currentLogger()
	var warns []string
	setLogger(func(level, msg string) {
		if level == "WARN" {
			warns = append(warns, msg)
		}
	})
	t.Cleanup(func() {
		setLogger(prev)
		identityMu.Lock()
		delete(identities, "7")
		identityMu.Unlock()
	})

	const remote = "10.9.1.1:1"
	const key = "7-aabb-1"
	const other = "7-ccdd-2"
	conn := &recordConn{fakeConn: fakeConn{addr: fakeAddr(remote)}}
	bucket := &connectBucket{conn: conn, closeChan: make(chan struct{})}
	bucket.relation()
	t.Cleanup(func() {
		ConnIndexTable.Delete(remote)
		ConnIndexTable.Delete(key)
		ConnIndexTable.Delete(other)
		bucket.close()
	})
	if _, ok := ConnIndexTable.Load(remote); !ok {
		t.Fatal("connection was not indexed by the remote address")
	}
	msg := socket.GetSendMessage("1", "gateway")
	msg.Type = "handshake"
	msg.Topic = socket.HandshakeTopic
	msg.Data = []byte(key)
	bucket.acceptHandshake(msg)
	if _, ok := ConnIndexTable.Load(remote); ok {
		t.Fatal("remote address stayed in the index after handshake")
	}
	if got, ok := ConnIndexTable.Load(key); !ok || got != bucket {
		t.Fatal("handshake key was not indexed")
	}
	if conn.n.Load() < 1 {
		t.Fatal("handshake was not acknowledged")
	}
	bucket.acceptHandshake(&socket.SendMessage{Type: "handshake", Topic: socket.HandshakeTopic, Data: []byte(other)})
	if len(warns) == 0 {
		t.Fatal("same cluster with a different startup was not warned")
	}
	warned := len(warns)
	bucket.acceptHandshake(&socket.SendMessage{Type: "handshake", Topic: socket.HandshakeTopic, Data: []byte(other)})
	if len(warns) != warned {
		t.Fatal("the same startup warned on every handshake")
	}
	bucket.close()
	if _, ok := ConnIndexTable.Load(other); ok {
		t.Fatal("closed connection kept its handshake key")
	}
}

func TestAudit_RebindDropsRoutesUnderTheOldKey(t *testing.T) {
	prev := currentLogger()
	setLogger(func(string, string) {})
	t.Cleanup(func() { setLogger(prev) })

	const remote = "10.4.4.4:4"
	const key = "4-rrrr-1"
	const next = "4-ssss-2"
	const other = "10.4.4.5:5"
	conn := &connectBucket{
		conn:      &recordConn{fakeConn: fakeConn{addr: fakeAddr(remote)}},
		closeChan: make(chan struct{}),
	}
	conn.relation()
	t.Cleanup(func() {
		conn.close()
		ConnIndexTable.Delete(remote)
		ConnIndexTable.Delete(key)
		ConnIndexTable.Delete(next)
	})

	svc := &topicGrpcService{}
	if _, err := svc.SubscribeTopic(context.Background(), &pb.SubscribeTopicRequest{
		Topic: []string{"rebind-room"}, Ip: remote, Count: []int64{1}, Version: []uint64{1},
		Round: 1, RoundBegin: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SubscribeTopic(context.Background(), &pb.SubscribeTopicRequest{
		Topic: []string{"other-room"}, Ip: other, Count: []int64{1}, Version: []uint64{1},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = svc.SubscribeTopic(context.Background(), &pb.SubscribeTopicRequest{
			Topic: []string{"rebind-room", "other-room"}, Ip: other, Count: []int64{0, 0}, Version: []uint64{9, 9},
		})
		topicMu.Lock()
		delete(subVersion, remote)
		delete(subVersion, key)
		delete(subVersion, next)
		delete(subVersion, other)
		delete(openRounds, remote)
		topicMu.Unlock()
	})
	if got := countTopic(t, svc, "rebind-room"); got != 1 {
		t.Fatalf("before rebind count=%d", got)
	}

	conn.rebind(key)
	select {
	case <-conn.closeChan:
		t.Fatal("rebind closed the connection")
	default:
	}
	if got := countTopic(t, svc, "rebind-room"); got != 0 {
		t.Fatalf("address route stayed at %d after handshake", got)
	}
	topicMu.Lock()
	_, oldVer := subVersion[remote]
	_, oldRound := openRounds[remote]
	topicMu.Unlock()
	if oldVer || oldRound {
		t.Fatal("address versions or reconcile state stayed after handshake")
	}
	if got := countTopic(t, svc, "other-room"); got != 1 {
		t.Fatalf("another gateway was cleared, count=%d", got)
	}

	if _, err := svc.SubscribeTopic(context.Background(), &pb.SubscribeTopicRequest{
		Topic: []string{"rebind-room"}, Ip: key, Count: []int64{1}, Version: []uint64{2},
	}); err != nil {
		t.Fatal(err)
	}
	if got := countTopic(t, svc, "rebind-room"); got != 1 {
		t.Fatalf("replay under the new key count=%d", got)
	}
	conn.rebind(next)
	if got := countTopic(t, svc, "rebind-room"); got != 0 {
		t.Fatalf("previous handshake key stayed at %d", got)
	}
	if got := countTopic(t, svc, "other-room"); got != 1 {
		t.Fatalf("second rebind cleared another gateway, count=%d", got)
	}
}

func TestAudit_HeartbeatWaitsWhileWarmingOrBusy(t *testing.T) {
	if managerStatus(time.Now().Add(-time.Minute)) != "ready" {
		t.Fatal("idle manager was not ready")
	}
	prev := managerWarmup
	managerWarmup = time.Hour
	t.Cleanup(func() { managerWarmup = prev })
	if managerStatus(time.Now()) != "wait" {
		t.Fatal("manager inside warmup was ready")
	}
	managerWarmup = 0
	for i := 0; i < registerParallel; i++ {
		if err := enterRegister(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for i := 0; i < registerParallel; i++ {
			leaveRegister()
		}
	})
	if managerStatus(time.Now().Add(-time.Minute)) != "ready" {
		t.Fatal("a busy register queue made the only manager unpublishable")
	}
}

func TestAudit_RegisterWaitsWhenFull(t *testing.T) {
	for i := 0; i < registerParallel; i++ {
		if err := enterRegister(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for i := 0; i < registerParallel; i++ {
			leaveRegister()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := enterRegister(ctx); err == nil {
		leaveRegister()
		t.Fatal("register proceeded with no free slot")
	}
}
