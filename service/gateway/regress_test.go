package gateway

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/OSMeteor/firetower/grpc/manager"
	"github.com/OSMeteor/firetower/socket"
)

// 第 2 层回归。编号对应 docs/release-test-plan.md。

func TestAudit_R01_SubscribeDoesNotBlockPublish(t *testing.T) {
	auditEnv(t)
	t.Cleanup(func() { setPool(nil) })
	tcp := linkedPipe(t)
	// linkedPipe 只有连接，没有发送队列。发布要走 NewClient 的队列。
	client := socket.NewClient("r1")
	client.Conn = tcp.Conn
	setPool([]*managerLink{{
		name: "only", tcp: client, rpc: &scriptedTopic{
			sub: func(context.Context, *pb.SubscribeTopicRequest) (*pb.SubscribeTopicResponse, error) {
				time.Sleep(20 * time.Millisecond)
				return &pb.SubscribeTopicResponse{}, nil
			},
		},
		register: true, publish: true,
	}})
	ft := &FireTower{ClientId: "r1", UserId: "r1"}
	fire := NewFireInfo(ft, nil)
	fire.Message.Type = "publish"
	fire.Message.Topic = "r1-pub"
	fire.Message.Data = []byte("x")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go func() {
		i := 0
		for ctx.Err() == nil {
			_, _ = ft.bindTopic([]string{fmt.Sprintf("r1-sub-%d", i)})
			i++
		}
	}()

	beats := 0
	next := time.Now().Add(8 * time.Second)
	pubs := 0
	for ctx.Err() == nil {
		if pickPublisher("r1-pub") == nil {
			t.Fatalf("publish %d: the only manager was not usable", pubs)
		}
		if err := ft.Publish(fire); err != nil {
			t.Fatalf("publish %d: %v", pubs, err)
		}
		pubs++
		if time.Now().After(next) {
			client.ApplyHeartbeat("wait")
			beats++
			next = time.Now().Add(8 * time.Second)
		}
		time.Sleep(40 * time.Millisecond)
	}
	if beats < 3 {
		t.Fatalf("only crossed %d heartbeats", beats)
	}
	if pubs < 100 {
		t.Fatalf("only published %d times", pubs)
	}
}

func TestAudit_R02_LastSubscriberHandsOff(t *testing.T) {
	const topic = "r2-room"
	b := &Bucket{}
	t.Cleanup(func() {
		holderMu.Lock()
		delete(holders, topic)
		holderMu.Unlock()
	})
	leaving := &FireTower{ClientId: "leaving", sendOut: make(chan *socket.SendMessage, 1)}
	staying := &FireTower{ClientId: "staying", sendOut: make(chan *socket.SendMessage, 1)}
	for i := 0; i < 100000; i++ {
		b.AddSubscribe(topic, leaving)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			b.DelSubscribe(topic, leaving)
		}()
		go func() {
			defer wg.Done()
			b.AddSubscribe(topic, staying)
		}()
		wg.Wait()
		msg := socket.GetSendMessage("r2", "audit")
		msg.Type = socket.PublishKey
		msg.Topic = topic
		msg.Data = []byte("x")
		if err := b.push(msg); err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
		select {
		case got := <-staying.sendOut:
			if string(got.Data) != "x" {
				t.Fatalf("round %d got %q", i, got.Data)
			}
		case <-time.After(time.Second):
			t.Fatalf("round %d: the new subscriber got nothing", i)
		}
		b.DelSubscribe(topic, staying)
	}
}

func TestAudit_R03a_MiddleChunkFailureDoesNotFinish(t *testing.T) {
	auditEnv(t)
	const n = 513
	topics := make([]string, n)
	for i := range topics {
		topics[i] = fmt.Sprintf("r3a-%d", i)
	}
	noteSubscribe(topics)
	t.Cleanup(func() { noteUnsubscribe(topics) })

	var calls atomic.Int32
	var ends atomic.Int32
	storeTopicGrpc(&scriptedTopic{
		sub: func(_ context.Context, in *pb.SubscribeTopicRequest) (*pb.SubscribeTopicResponse, error) {
			if calls.Add(1) == 2 {
				return nil, fmt.Errorf("middle chunk")
			}
			if in.RoundEnd {
				ends.Add(1)
			}
			return &pb.SubscribeTopicResponse{}, nil
		},
	})
	replayLocalTopics()
	if calls.Load() != 2 {
		t.Fatalf("replay calls=%d, want the first chunk and a failed middle chunk", calls.Load())
	}
	if ends.Load() != 0 {
		t.Fatal("a failed middle chunk still closed the round")
	}
}

func TestAudit_R03b_SnapshotMissesATopicSubscribedBeforeTheFirstChunk(t *testing.T) {
	auditEnv(t)
	noteSubscribe([]string{"r3b-old"})
	t.Cleanup(func() { noteUnsubscribe([]string{"r3b-old", "r3b-new"}) })

	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var sawNew atomic.Bool
	var ended atomic.Bool
	storeTopicGrpc(&scriptedTopic{
		sub: func(_ context.Context, in *pb.SubscribeTopicRequest) (*pb.SubscribeTopicResponse, error) {
			once.Do(func() { close(started) })
			<-release
			for _, topic := range in.Topic {
				if topic == "r3b-new" {
					sawNew.Store(true)
				}
			}
			if in.RoundEnd {
				ended.Store(true)
			}
			return &pb.SubscribeTopicResponse{}, nil
		},
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		replayLocalTopics()
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("replay did not reach the first chunk")
	}
	noteSubscribe([]string{"r3b-new"})
	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("replay did not finish")
	}
	if sawNew.Load() {
		t.Fatal("a topic subscribed after the snapshot was replayed as part of that round")
	}
	if !ended.Load() {
		t.Fatal("the round did not finish")
	}
}

func TestAudit_R04_FiveThousandPublishesAreAccounted(t *testing.T) {
	prev := hotTopicMin.Load()
	hotTopicMin.Store(0)
	t.Cleanup(func() { hotTopicMin.Store(prev) })

	const topic = "r4-room"
	const total = 5000
	full := &Bucket{}
	fullClient := &FireTower{ClientId: "full", sendOut: make(chan *socket.SendMessage, total)}
	full.AddSubscribe(topic, fullClient)
	t.Cleanup(func() { full.DelSubscribe(topic, fullClient) })
	before := sendDrops.Load()
	for i := 0; i < total; i++ {
		msg := socket.GetSendMessage("r4", "audit")
		msg.Type = socket.PublishKey
		msg.Topic = topic
		msg.Data = []byte("all")
		if err := full.push(msg); err != nil {
			t.Fatal(err)
		}
	}
	if got := sendDrops.Load() - before; got != 0 {
		t.Fatalf("a large queue dropped %d", got)
	}
	if len(fullClient.sendOut) != total {
		t.Fatalf("delivered %d/%d", len(fullClient.sendOut), total)
	}

	tight := &Bucket{}
	tightClient := &FireTower{ClientId: "tight", sendOut: make(chan *socket.SendMessage, 1)}
	tight.AddSubscribe(topic+"-tight", tightClient)
	t.Cleanup(func() { tight.DelSubscribe(topic+"-tight", tightClient) })
	before = sendDrops.Load()
	for i := 0; i < total; i++ {
		msg := socket.GetSendMessage("r4", "audit")
		msg.Type = socket.PublishKey
		msg.Topic = topic + "-tight"
		msg.Data = []byte("drop")
		_ = tight.push(msg)
	}
	got := 0
	if len(tightClient.sendOut) == 1 {
		got = 1
	}
	dropped := int(sendDrops.Load() - before)
	if got+dropped != total {
		t.Fatalf("delivered %d + dropped %d != %d", got, dropped, total)
	}
}

func TestAudit_R05_CensusChurnDoesNotKeepTopics(t *testing.T) {
	auditEnv(t)
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	const total = 1000000
	for i := 0; i < total; i++ {
		topic := fmt.Sprintf("r5g-%d", i)
		noteSubscribe([]string{topic})
		noteUnsubscribe([]string{topic})
		if (i+1)%100000 == 0 {
			censusMu.Lock()
			left := 0
			for topic := range census {
				if strings.HasPrefix(topic, "r5g-") {
					left++
				}
			}
			censusMu.Unlock()
			if left != 0 {
				t.Fatalf("after %d topics, census still holds %d", i+1, left)
			}
		}
	}
	runtime.GC()
	var after runtime.MemStats
	if after.HeapAlloc > before.HeapAlloc+64<<20 {
		t.Fatalf("gateway heap grew from %d to %d", before.HeapAlloc, after.HeapAlloc)
	}
}

func TestAudit_R06_FanoutDoesNotCopyEveryBody(t *testing.T) {
	prev := hotTopicMin.Load()
	hotTopicMin.Store(0)
	t.Cleanup(func() { hotTopicMin.Store(prev) })

	const topic = "r6-room"
	const n = 10000
	b := &Bucket{}
	clients := make([]*FireTower, n)
	for i := 0; i < n; i++ {
		clients[i] = &FireTower{ClientId: fmt.Sprintf("r6-%d", i), sendOut: make(chan *socket.SendMessage, 1)}
		b.AddSubscribe(topic, clients[i])
	}
	t.Cleanup(func() {
		for _, c := range clients {
			b.DelSubscribe(topic, c)
		}
	})
	body := make([]byte, 1024)
	for i := range body {
		body[i] = 'k'
	}
	msg := socket.GetSendMessage("r6", "audit")
	msg.Type = socket.PublishKey
	msg.Topic = topic
	msg.Data = body
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	if err := b.push(msg); err != nil {
		t.Fatal(err)
	}
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	delta := after.TotalAlloc - before.TotalAlloc
	if delta >= 4<<20 {
		t.Fatalf("one 1KB publish to %d subscribers allocated %d bytes", n, delta)
	}
	if &clients[0].sendOut != nil {
		got := <-clients[0].sendOut
		if len(got.Data) != 1024 || &got.Data[0] != &body[0] {
			t.Fatal("subscribers did not share the one payload")
		}
	}
}

func TestAudit_R07_SubscribeWhileTCPDownDoesNotRegister(t *testing.T) {
	auditEnv(t)
	t.Cleanup(func() { setPool(nil) })
	left, right := net.Pipe()
	t.Cleanup(func() {
		_ = left.Close()
		_ = right.Close()
	})
	tcp := socket.NewClient("r7")
	tcp.Conn = left
	tcp.SetRouteKey("1-r7-1")
	tcp.Shutdown()
	var calls atomic.Int32
	setPool([]*managerLink{{
		name: "down", tcp: tcp, rpc: &scriptedTopic{
			sub: func(context.Context, *pb.SubscribeTopicRequest) (*pb.SubscribeTopicResponse, error) {
				calls.Add(1)
				return &pb.SubscribeTopicResponse{}, nil
			},
		},
		register: true, publish: true,
	}})
	ft := &FireTower{ClientId: "r7", UserId: "r7"}
	if _, err := ft.bindTopic([]string{"r7-room"}); err == nil {
		t.Fatal("subscribe succeeded while the manager TCP was down")
	}
	if calls.Load() != 0 {
		t.Fatal("manager accepted a registration with no TCP connection")
	}
	censusMu.Lock()
	_, ok := census["r7-room"]
	censusMu.Unlock()
	if ok {
		t.Fatal("failed subscribe left a census entry")
	}
}

func TestAudit_R08_DuplicateTopicUnsubscribesToZero(t *testing.T) {
	auditEnv(t)
	var unsub *pb.UnSubscribeTopicRequest
	storeTopicGrpc(&scriptedTopic{
		sub: func(_ context.Context, in *pb.SubscribeTopicRequest) (*pb.SubscribeTopicResponse, error) {
			if len(in.Topic) != 1 || in.Topic[0] != "r8a" || len(in.Count) != 1 || in.Count[0] != 1 {
				return nil, fmt.Errorf("subscribe %#v", in)
			}
			return &pb.SubscribeTopicResponse{}, nil
		},
		unsub: func(_ context.Context, in *pb.UnSubscribeTopicRequest) (*pb.UnSubscribeTopicResponse, error) {
			unsub = in
			return &pb.UnSubscribeTopicResponse{}, nil
		},
	})
	ft := &FireTower{ClientId: "r8", UserId: "r8"}
	if _, err := ft.bindTopic([]string{"r8a", "r8a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ft.unbindTopic([]string{"r8a"}); err != nil {
		t.Fatal(err)
	}
	if unsub == nil || len(unsub.Topic) != 1 || unsub.Topic[0] != "r8a" || len(unsub.Count) != 1 || unsub.Count[0] != 0 {
		t.Fatalf("unsubscribe %#v", unsub)
	}
}

func TestAudit_StaleHandshakeDoesNotOverwriteTheNewKey(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 2)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- conn
		}
	}()

	client := socket.NewClient(ln.Addr().String())
	client.OnPush(func(*socket.SendMessage) {})
	client.SetOnConnect(func() { announceRouteClient(client) })
	connected := make(chan error, 1)
	go func() { connected <- client.Connect() }()

	first := acceptOne(t, accepted)
	readSome(first)
	_ = first.Close()

	second := acceptOne(t, accepted)
	readSome(second)
	ack, err := socket.Enpack("handshake_ack", "0", "system", socket.HandshakeTopic, []byte("ok"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Write(ack); err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	defer client.Shutdown()

	select {
	case err := <-connected:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the handshake that was already in flight did not return")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if client.RouteKey() == routeIdentity(client.Generation()) && client.RouteKey() != "" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("route key %q does not match generation %d", client.RouteKey(), client.Generation())
}

func acceptOne(t *testing.T, ch <-chan net.Conn) net.Conn {
	t.Helper()
	select {
	case conn := <-ch:
		return conn
	case <-time.After(2 * time.Second):
		t.Fatal("connection was not accepted")
		return nil
	}
}

func readSome(conn net.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 2048)
	_, _ = conn.Read(buf)
}
