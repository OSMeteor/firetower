package gateway

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OSMeteor/firetower/socket"
	"github.com/gorilla/websocket"
)

// 阶段 0：空调用、坏消息和并发关闭不能把 Gateway 进程打崩。
// 回调自己 panic 时只关掉这一条连接。

func TestStage0_NilCallsDoNotPanic(t *testing.T) {
	saveAuditGlobals(t)
	noopAuditLogs()
	storeTopicManage(nil)
	storeTopicGrpc(nil)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil call panicked: %v", r)
		}
	}()

	var tower *FireTower
	tower.SetOnConnectHandler(func() bool { return true })
	tower.SetOnOfflineHandler(func() {})
	tower.SetReadHandler(func(*FireInfo) bool { return true })
	tower.SetSubscribeHandler(func(*FireLife, []string) bool { return true })
	tower.SetUnSubscribeHandler(func(*FireLife, []string) bool { return true })
	tower.SetBeforeSubscribeHandler(func(*FireLife, []string) ([]string, bool) { return nil, false })
	tower.SetReadTimeoutHandler(func(*FireInfo) {})
	tower.SetOnSystemRemove(func(string) {})
	if err := tower.Publish(nil); err == nil {
		t.Fatal("nil tower Publish returned success")
	}
	if tower.CheckTopicExist("room") {
		t.Fatal("nil grpc client reported a topic")
	}
	if tower.GetConnectNum("room") != 0 {
		t.Fatal("nil grpc client reported a count")
	}

	live := &FireTower{}
	fire := NewFireInfo(live, nil)
	fire.Message.Type = "publish"
	fire.Message.Topic = "room"
	fire.Message.Data = []byte("x")
	if err := live.Publish(nil); err == nil {
		t.Fatal("nil fire was published")
	}
	if err := live.Publish(fire); err == nil {
		t.Fatal("publish without a manager returned success")
	}
	if live.CheckTopicExist("") || live.CheckTopicExist("room") {
		t.Fatal("missing manager reported a topic")
	}
	if live.GetConnectNum("room") != 0 {
		t.Fatal("missing manager reported a count")
	}

	storeTopicManage(&socket.TcpClient{})
	if err := live.Publish(fire); err == nil {
		t.Fatal("publish on a client without a queue returned success")
	}
}

func TestStage0_BadClientMessagesDoNotPanic(t *testing.T) {
	auditEnv(t)
	ft := auditTower(t, "stage0-read", "user")
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("readDispose panicked: %v", r)
			}
		}()
		ft.readDispose()
	}()

	empty := NewFireInfo(ft, nil)
	empty.Message.Type = "subscribe"
	empty.Message.Topic = ""
	ft.readIn <- empty

	select {
	case <-done:
		t.Fatal("empty topic ended the read loop")
	case <-time.After(150 * time.Millisecond):
	}

	ft.SetReadHandler(func(*FireInfo) bool { panic("handler") })
	bad := NewFireInfo(ft, nil)
	bad.Message.Type = "publish"
	bad.Message.Topic = "room"
	bad.Message.Data = []byte("{")
	ft.readIn <- bad

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("panicking handler left readDispose running")
	}
	select {
	case <-ft.closeChan:
	default:
		t.Fatal("panicking handler left the connection open")
	}
}

func TestStage0_BucketAndDispatchDoNotPanic(t *testing.T) {
	auditEnv(t)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("dispatch panicked: %v", r)
		}
	}()

	ft := auditTower(t, "stage0-bucket", "user")
	bucket := &Bucket{
		topicRelevance: map[string]map[string]*FireTower{
			"room": {"stage0-bucket": ft, "gone": nil},
		},
		BuffChan: make(chan *socket.SendMessage, 2),
	}
	msg := socket.GetSendMessage("1", "user")
	msg.Type = socket.PublishKey
	msg.Topic = "room"
	msg.Data = []byte("hello")
	if err := bucket.push(nil); err == nil {
		t.Fatal("nil push returned success")
	}
	_ = bucket.push(msg)
	_ = bucket.unSubscribeByUserId(socket.GetSendMessage("2", "user"))
	_ = bucket.unSubscribeAll(msg)
	_ = bucket.offlineUsers(msg)
	bucket.AddSubscribe("", nil)
	bucket.DelSubscribe("room", nil)
	_ = ft.ToSelf(nil)
	_ = ft.ToSelf([]byte("{}"))
	ft.Close()
	ft.Close()

	tm := &TowerManager{
		bucket:      []*Bucket{nil, bucket},
		centralChan: make(chan *socket.SendMessage, 4),
	}
	go tm.dispatch(2)
	other := socket.GetSendMessage("3", "user")
	other.Type = socket.PublishKey
	other.Topic = "room"
	other.Data = []byte("other")
	tm.centralChan <- nil
	tm.centralChan <- other
	close(tm.centralChan)
	time.Sleep(50 * time.Millisecond)
}

func TestStage0_ConcurrentCloseDoesNotPanic(t *testing.T) {
	auditEnv(t)
	var wg sync.WaitGroup
	errCh := make(chan error, 1)
	for i := 0; i < 8; i++ {
		ft := auditTower(t, fmt.Sprintf("stage0-%d", i), "user")
		wg.Add(1)
		go func(ft *FireTower) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					select {
					case errCh <- fmt.Errorf("%v", r):
					default:
					}
				}
			}()
			for n := 0; n < 20; n++ {
				_ = ft.ToSelf([]byte("x"))
				_ = ft.Send(socket.GetSendMessage("1", "user"))
				ft.Close()
			}
		}(ft)
	}
	wg.Wait()
	select {
	case err := <-errCh:
		t.Fatal(err)
	default:
	}
}

func TestStage0_OfflinePanicDoesNotSkipTheRest(t *testing.T) {
	b := &Bucket{}
	first := &FireTower{ClientId: "first", UserId: "same", closeChan: make(chan struct{})}
	second := &FireTower{ClientId: "second", UserId: "same", closeChan: make(chan struct{})}
	first.SetOnOfflineHandler(func() { panic("offline") })
	b.AddSubscribe("offline-room", first)
	b.AddSubscribe("offline-room", second)
	t.Cleanup(func() {
		b.DelSubscribe("offline-room", first)
		b.DelSubscribe("offline-room", second)
	})
	msg := socket.GetSendMessage("off", "system")
	msg.Type = socket.OfflineUserKey
	msg.Topic = "offline-room"
	msg.Data = []byte("same")
	if err := b.offlineUsers(msg); err != nil {
		t.Fatal(err)
	}
	if !second.isClose {
		t.Fatal("a panicking offline callback left the other connection in place")
	}
}

func TestStage0_DeliverClosedChannelDoesNotPanic(t *testing.T) {
	ch := make(chan *socket.SendMessage)
	close(ch)
	deliverOrDrop(&Bucket{BuffChan: ch}, 1, &socket.SendMessage{Data: []byte("x")})
}

func TestStage0_ClosedSendDoesNotPanic(t *testing.T) {
	ch := make(chan *socket.SendMessage)
	close(ch)
	ft := &FireTower{ClientId: "closed", sendOut: ch}
	msg := socket.GetSendMessage("1", "user")
	msg.Type = socket.PublishKey
	msg.Topic = "room"
	msg.Data = []byte("x")
	for i := 0; i < 200; i++ {
		if err := ft.Send(msg); err == nil {
			t.Fatal("send on a closed channel succeeded")
		}
	}
}

func TestStage0_BadFramesKeepTheConnection(t *testing.T) {
	auditEnv(t)
	server, client, cleanup := wsPair(t)
	defer cleanup()
	ft := &FireTower{
		ClientId:  "bad-frames",
		UserId:    "user",
		startTime: time.Now(),
		readIn:    make(chan *FireInfo, 8),
		sendOut:   make(chan *socket.SendMessage, 4),
		ws:        server,
		closeChan: make(chan struct{}),
	}
	go ft.readLoop()
	go ft.readDispose()

	frames := [][]byte{
		[]byte("not-json"),
		[]byte("null"),
		[]byte(`{"type":"subscribe","topic":""}`),
		[]byte(`{"type":"subscribe"}`),
		[]byte(`{"type":"nope","topic":"room","data":null}`),
		[]byte(`{"type":"subscribe","topic":"` + strings.Repeat("t,", 3000) + `"}`),
	}
	for _, frame := range frames {
		if err := client.WriteMessage(websocket.TextMessage, frame); err != nil {
			t.Fatalf("write bad frame: %v", err)
		}
	}
	if err := client.WriteMessage(websocket.TextMessage, []byte(`{"type":"subscribe","topic":"crash-room","data":{}}`)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ft.mutex.Lock()
		ok := ft.topic["crash-room"]
		ft.mutex.Unlock()
		if ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	ft.mutex.Lock()
	subscribed := ft.topic["crash-room"]
	ft.mutex.Unlock()
	if !subscribed {
		t.Fatal("valid subscribe did not land after bad frames")
	}
	select {
	case <-ft.closeChan:
		t.Fatal("bad frames closed the connection")
	default:
	}
}

func TestStage0_HandlerPanicDoesNotStopTheBucket(t *testing.T) {
	saveAuditGlobals(t)
	storeTM(nil)
	storeTopicManage(nil)
	storeTopicGrpc(nil)
	prev := hotTopicMin.Load()
	hotTopicMin.Store(0)
	t.Cleanup(func() { hotTopicMin.Store(prev) })

	b := &Bucket{BuffChan: make(chan *socket.SendMessage, 4)}
	go b.consumer()
	victim := &FireTower{
		ClientId:  "victim",
		UserId:    "victim",
		sendOut:   make(chan *socket.SendMessage, 2),
		closeChan: make(chan struct{}),
		topic:     map[string]bool{"room": true},
	}
	started := make(chan struct{})
	victim.SetUnSubscribeHandler(func(*FireLife, []string) bool {
		close(started)
		panic("handler")
	})
	other := &FireTower{ClientId: "other", UserId: "other", sendOut: make(chan *socket.SendMessage, 2)}
	b.AddSubscribe("room", victim)
	b.AddSubscribe("room", other)
	t.Cleanup(func() {
		b.DelSubscribe("room", victim)
		b.DelSubscribe("room", other)
	})

	offline := socket.GetSendMessage("off", "system")
	offline.Type = socket.OfflineTopicByUserIdKey
	offline.Topic = "room"
	offline.Data = []byte("victim")
	b.BuffChan <- offline
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("unsubscribe handler did not run")
	}

	pub := socket.GetSendMessage("pub", "audit")
	pub.Type = socket.PublishKey
	pub.Topic = "room"
	pub.Data = []byte("still-here")
	sent := time.Now()
	b.BuffChan <- pub
	select {
	case got := <-other.sendOut:
		if string(got.Data) != "still-here" {
			t.Fatalf("got %q", got.Data)
		}
		if time.Since(sent) >= 90*time.Millisecond {
			t.Fatalf("delivery took %s; the bucket consumer restarted", time.Since(sent))
		}
	case <-time.After(time.Second):
		t.Fatal("bucket stopped delivering after a handler panic")
	}
}

func TestAudit_HotLoopSurvivesPanic(t *testing.T) {
	prev := hotTopicMin.Load()
	hotTopicMin.Store(1)
	var n atomic.Int32
	hook := pushFn(func(*socket.SendMessage) {
		if n.Add(1) == 1 {
			panic("hot once")
		}
	})
	testBeforePush.Store(&hook)
	t.Cleanup(func() {
		hotTopicMin.Store(prev)
		testBeforePush.Store(nil)
	})

	b := &Bucket{}
	client := &FireTower{ClientId: "hot", sendOut: make(chan *socket.SendMessage, 2)}
	b.AddSubscribe("hot-panic", client)
	t.Cleanup(func() { b.DelSubscribe("hot-panic", client) })

	first := socket.GetSendMessage("1", "audit")
	first.Type = socket.PublishKey
	first.Topic = "hot-panic"
	first.Data = []byte("boom")
	b.enqueue(first)

	second := socket.GetSendMessage("2", "audit")
	second.Type = socket.PublishKey
	second.Topic = "hot-panic"
	second.Data = []byte("after")
	b.enqueue(second)

	select {
	case got := <-client.sendOut:
		if string(got.Data) != "after" {
			t.Fatalf("subscriber got %q", got.Data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("hot loop did not deliver after a panic")
	}
}

func TestAudit_ConsumerSurvivesPanic(t *testing.T) {
	prev := hotTopicMin.Load()
	hotTopicMin.Store(0)
	var n atomic.Int32
	hook := pushFn(func(*socket.SendMessage) {
		if n.Add(1) == 1 {
			panic("consumer once")
		}
	})
	testBeforePush.Store(&hook)
	t.Cleanup(func() {
		hotTopicMin.Store(prev)
		testBeforePush.Store(nil)
	})

	b := &Bucket{BuffChan: make(chan *socket.SendMessage, 4)}
	go b.consumer()
	client := &FireTower{ClientId: "c", sendOut: make(chan *socket.SendMessage, 2)}
	b.AddSubscribe("consumer-panic", client)
	t.Cleanup(func() { b.DelSubscribe("consumer-panic", client) })

	first := socket.GetSendMessage("1", "audit")
	first.Type = socket.PublishKey
	first.Topic = "consumer-panic"
	first.Data = []byte("boom")
	second := socket.GetSendMessage("2", "audit")
	second.Type = socket.PublishKey
	second.Topic = "consumer-panic"
	second.Data = []byte("after")
	b.BuffChan <- first
	b.BuffChan <- second

	select {
	case got := <-client.sendOut:
		if string(got.Data) != "after" {
			t.Fatalf("subscriber got %q", got.Data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bucket consumer did not deliver after a panic")
	}
}
