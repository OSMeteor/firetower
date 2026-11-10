package gateway

import (
	"strings"
	"testing"
	"time"

	"github.com/OSMeteor/firetower/socket"
	"github.com/gorilla/websocket"
)

func TestAudit_OnlyBucketsHoldingTheTopicReceive(t *testing.T) {
	empty := &Bucket{BuffChan: make(chan *socket.SendMessage, 2)}
	holding := &Bucket{BuffChan: make(chan *socket.SendMessage, 2)}
	client := &FireTower{ClientId: "hold", sendOut: make(chan *socket.SendMessage, 2)}
	holding.AddSubscribe("room-1b", client)
	t.Cleanup(func() { holding.DelSubscribe("room-1b", client) })

	tm := &TowerManager{
		bucket:      []*Bucket{empty, holding},
		centralChan: make(chan *socket.SendMessage, 2),
	}
	go tm.dispatch(2)
	msg := socket.GetSendMessage("1b", "audit")
	msg.Type = socket.PublishKey
	msg.Topic = "room-1b"
	msg.Data = []byte("only-holders")
	tm.centralChan <- msg

	select {
	case got := <-holding.BuffChan:
		if string(got.Data) != "only-holders" {
			t.Fatalf("holding bucket got %q", got.Data)
		}
	case <-time.After(time.Second):
		t.Fatal("holding bucket did not receive")
	}
	select {
	case got := <-empty.BuffChan:
		t.Fatalf("empty bucket received %q", got.Data)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestAudit_HotTopicSkipsTheSharedQueue(t *testing.T) {
	prev := hotTopicMin.Load()
	hotTopicMin.Store(1)
	t.Cleanup(func() { hotTopicMin.Store(prev) })

	b := &Bucket{BuffChan: make(chan *socket.SendMessage, 2)}
	client := &FireTower{ClientId: "hot", sendOut: make(chan *socket.SendMessage, 2)}
	b.AddSubscribe("hot-room", client)
	t.Cleanup(func() { b.DelSubscribe("hot-room", client) })

	msg := socket.GetSendMessage("hot", "audit")
	msg.Type = socket.PublishKey
	msg.Topic = "hot-room"
	msg.Data = []byte("hot")
	b.enqueue(msg)

	select {
	case got := <-client.sendOut:
		if string(got.Data) != "hot" {
			t.Fatalf("subscriber got %q", got.Data)
		}
	case <-time.After(time.Second):
		t.Fatal("hot topic did not reach the subscriber")
	}
	if len(b.BuffChan) != 0 {
		t.Fatal("hot topic used the shared bucket queue")
	}
}

func TestAudit_PassiveSkipsStateOnly(t *testing.T) {
	b := &Bucket{}
	active := &FireTower{ClientId: "active", sendOut: make(chan *socket.SendMessage, 2)}
	quiet := &FireTower{ClientId: "quiet", sendOut: make(chan *socket.SendMessage, 2)}
	quiet.SetPassive("room", true)
	b.AddSubscribe("room", active)
	b.AddSubscribe("room", quiet)
	t.Cleanup(func() {
		b.DelSubscribe("room", active)
		b.DelSubscribe("room", quiet)
	})

	state := socket.GetSendMessage("state", "audit")
	state.Type = socket.StateKey
	state.Topic = "room"
	state.Data = []byte("state")
	if err := b.push(state); err != nil {
		t.Fatal(err)
	}
	if len(active.sendOut) != 1 || len(quiet.sendOut) != 0 {
		t.Fatalf("state delivery active=%d quiet=%d", len(active.sendOut), len(quiet.sendOut))
	}

	pub := socket.GetSendMessage("pub", "audit")
	pub.Type = socket.PublishKey
	pub.Topic = "room"
	pub.Data = []byte("pub")
	if err := b.push(pub); err != nil {
		t.Fatal(err)
	}
	if len(quiet.sendOut) != 1 {
		t.Fatal("passive subscriber missed the business message")
	}
}

func TestAudit_GapHintIsOffUntilEnabled(t *testing.T) {
	msg := socket.GetSendMessage("gap", "audit")
	msg.Type = socket.PublishKey
	msg.Topic = "room"
	msg.Data = []byte("dropped")

	server, client, cleanup := wsPair(t)
	defer cleanup()
	ft := &FireTower{
		ClientId:  "gap",
		sendOut:   make(chan *socket.SendMessage, 1),
		ws:        server,
		closeChan: make(chan struct{}),
		gapWake:   make(chan struct{}, 1),
	}
	ft.sendOut <- &socket.SendMessage{Data: []byte("full")}
	if err := ft.Send(msg); err == nil {
		t.Fatal("expected the full queue to drop")
	}
	ft.gapMu.Lock()
	disabled := len(ft.gaps)
	ft.gapMu.Unlock()
	if disabled != 0 {
		t.Fatal("a disabled connection stored a gap flag")
	}
	go ft.sendLoop()
	kind, body, err := client.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if kind != websocket.TextMessage || string(body) != "full" {
		t.Fatalf("first frame %d %q", kind, body)
	}
	close(ft.closeChan)
	client.SetReadDeadline(time.Now().Add(80 * time.Millisecond))
	if _, extra, err := client.ReadMessage(); err == nil {
		t.Fatalf("default client received an extra frame %q", extra)
	}

	server2, client2, cleanup2 := wsPair(t)
	defer cleanup2()
	on := &FireTower{
		ClientId:  "gap-on",
		sendOut:   make(chan *socket.SendMessage, 1),
		ws:        server2,
		closeChan: make(chan struct{}),
		gapWake:   make(chan struct{}, 1),
	}
	on.EnableGapHints()
	on.sendOut <- &socket.SendMessage{Data: []byte("next")}
	if err := on.Send(msg); err == nil {
		t.Fatal("expected the enabled queue to drop")
	}
	go on.sendLoop()
	client2.SetReadDeadline(time.Now().Add(time.Second))
	sawHint := false
	var last []byte
	for i := 0; i < 2; i++ {
		_, last, err = client2.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(last), `"type":"gap"`) && strings.Contains(string(last), `"topic":"room"`) {
			sawHint = true
		}
	}
	if !sawHint {
		t.Fatalf("enabled connection did not get a gap hint, last=%q", last)
	}
	close(on.closeChan)
}

func TestAudit_CapacityWarnNamesTheLayer(t *testing.T) {
	prevHold := warnHold
	prevLog := loadTowerLogger()
	warnHold = 0
	var got string
	storeTowerLogger(func(_ *FireTower, level, info string) {
		if level == "WARN" {
			got = info
		}
	})
	t.Cleanup(func() {
		warnHold = prevHold
		storeTowerLogger(prevLog)
		warnMu.Lock()
		delete(warnStates, "connections")
		warnMu.Unlock()
	})
	warnCapacity("connections", 70, 70, "add gateway")
	if !strings.Contains(got, "connections") || !strings.Contains(got, "value=70") || !strings.Contains(got, "add gateway") {
		t.Fatalf("warn log = %q", got)
	}
	got = ""
	warnCapacity("connections", 70, 70, "add gateway")
	if got != "" {
		t.Fatalf("repeated warn = %q", got)
	}
}
