package gateway

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OSMeteor/firetower/socket"
	"github.com/gorilla/websocket"
)

func TestAudit_FT03_FullBucketDoesNotBlockOthers(t *testing.T) {
	prev := hotTopicMin.Load()
	hotTopicMin.Store(0)
	t.Cleanup(func() { hotTopicMin.Store(prev) })

	wedged := &Bucket{BuffChan: make(chan *socket.SendMessage, 1)}
	open := &Bucket{BuffChan: make(chan *socket.SendMessage, 4)}
	wedged.BuffChan <- &socket.SendMessage{Topic: "hot-room"}
	hotClient := &FireTower{ClientId: "hot", sendOut: make(chan *socket.SendMessage, 1)}
	openClient := &FireTower{ClientId: "open", sendOut: make(chan *socket.SendMessage, 1)}
	wedged.AddSubscribe("hot-room", hotClient)
	open.AddSubscribe("open-room", openClient)
	t.Cleanup(func() {
		wedged.DelSubscribe("hot-room", hotClient)
		open.DelSubscribe("open-room", openClient)
	})

	storeTM(&TowerManager{
		bucket:      []*Bucket{wedged, open},
		centralChan: make(chan *socket.SendMessage, 16),
	})
	go TM.dispatch(2)

	before := bucketDrops.Load()
	start := time.Now()
	for i := 0; i < 5; i++ {
		msg := socket.GetSendMessage("ft03", "audit")
		msg.Type = socket.PublishKey
		msg.Topic = "hot-room"
		msg.Data = []byte{byte('a' + i)}
		TM.centralChan <- msg
	}
	openMsg := socket.GetSendMessage("ft03", "audit")
	openMsg.Type = socket.PublishKey
	openMsg.Topic = "open-room"
	openMsg.Data = []byte("open")
	TM.centralChan <- openMsg

	select {
	case got := <-open.BuffChan:
		if string(got.Data) != "open" {
			t.Fatalf("open topic got %q", got.Data)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("FT-03: a full bucket held up another topic")
	}
	if elapsed := time.Since(start); elapsed >= 150*time.Millisecond {
		t.Fatalf("FT-03: dispatch waited %s on a full bucket", elapsed)
	}
	if bucketDrops.Load()-before < 5 {
		t.Fatalf("FT-03: full bucket drops=%d", bucketDrops.Load()-before)
	}
}

func TestAudit_CO01_ConcurrentSubscribeSendClose(t *testing.T) {
	// concurrent map 读写会让整个测试进程 fatal，单独拉起子进程，避免打断同包的其他验收用例。
	if os.Getenv("FIRETOWER_AUDIT_CO01") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run", "^TestAudit_CO01_ConcurrentSubscribeSendClose$", "-test.count=1", "-test.timeout", "20s")
		cmd.Env = append(os.Environ(), "FIRETOWER_AUDIT_CO01=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("CO01: concurrent subscribe/send/close crashed or failed:\n%s", out)
		}
		return
	}
	auditEnv(t)
	ft := auditTower(t, "c1", "u1")
	ft.topic = map[string]bool{}

	const n = 32
	var wg sync.WaitGroup
	wg.Add(n)
	errCh := make(chan error, 1)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					select {
					case errCh <- fmt.Errorf("CO01 panic: %v", r):
					default:
					}
				}
			}()
			for j := 0; j < 40; j++ {
				_, _ = ft.bindTopic([]string{"room", "room-b"})
				_, _ = ft.unbindTopic([]string{"room", "room-b"})
				_ = ft.Send(&socket.SendMessage{Topic: "room", Data: []byte("m")})
			}
			ft.Close()
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("CO01: subscribe/send/close deadlocked")
	}
	select {
	case err := <-errCh:
		t.Fatal(err)
	default:
	}
}

func TestAudit_FT08_ToSelfIsQueued(t *testing.T) {
	saveAuditGlobals(t)
	loadAuditConfig(t, 3600, 4, 1, 1)
	noopAuditLogs()
	server, client, cleanup := wsPair(t)
	defer cleanup()
	ft := &FireTower{
		startTime: time.Now(),
		sendOut:   make(chan *socket.SendMessage, 4),
		ws:        server,
		closeChan: make(chan struct{}),
	}

	if err := ft.ToSelf([]byte("queued-body")); err != nil {
		t.Fatalf("ToSelf: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if len(ft.sendOut) != 1 {
		t.Fatal("FT-08: ToSelf wrote the socket directly instead of leaving the payload queued")
	}

	go ft.sendLoop()
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	_, data, err := client.ReadMessage()
	if err != nil || string(data) != "queued-body" {
		t.Fatalf("FT-08: queued payload was not delivered by the single writer: %v %q", err, data)
	}
}

func TestAudit_CO06_SendLoopLeavesMessageType(t *testing.T) {
	saveAuditGlobals(t)
	loadAuditConfig(t, 3600, 4, 1, 1)
	noopAuditLogs()
	server, client, cleanup := wsPair(t)
	defer cleanup()
	ft := &FireTower{
		sendOut:   make(chan *socket.SendMessage, 2),
		ws:        server,
		closeChan: make(chan struct{}),
	}
	msg := &socket.SendMessage{Type: "publish", Topic: "room", Data: []byte("body")}
	ft.sendOut <- msg
	go ft.sendLoop()
	t.Cleanup(func() { ft.Close() })
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	_, data, err := client.ReadMessage()
	if err != nil || string(data) != "body" {
		t.Fatalf("CO06: payload = %q, err = %v", data, err)
	}
	if msg.MessageType != 0 {
		t.Fatalf("CO06: sendLoop wrote MessageType=%d on the shared message", msg.MessageType)
	}
}

func TestAudit_CO02_SingleWriter(t *testing.T) {
	saveAuditGlobals(t)
	loadAuditConfig(t, 3600, 4, 1, 1)
	noopAuditLogs()
	var logs lockedBuffer
	old := DefaultErrorWriter
	DefaultErrorWriter = &logs
	t.Cleanup(func() { DefaultErrorWriter = old })

	server, _, cleanup := wsPair(t)
	defer cleanup()
	ft := &FireTower{
		startTime: time.Now(),
		sendOut:   make(chan *socket.SendMessage, 8),
		ws:        server,
		closeChan: make(chan struct{}),
	}
	go ft.sendLoop()

	payload := bytes.Repeat([]byte("a"), 256*1024)
	go func() {
		for i := 0; i < 40; i++ {
			ft.sendOut <- &socket.SendMessage{MessageType: websocket.TextMessage, Data: payload}
		}
	}()
	time.Sleep(100 * time.Millisecond)

	var callerPanic atomic.Int32
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if recover() != nil {
				callerPanic.Add(1)
			}
		}()
		for i := 0; i < 20; i++ {
			_ = ft.ToSelf([]byte("from-self"))
		}
	}()
	wg.Wait()
	time.Sleep(100 * time.Millisecond)

	logged := logs.String()
	if callerPanic.Load() > 0 || strings.Contains(logged, "concurrent write") || strings.Contains(logged, "PANIC") {
		t.Fatalf("CO02: concurrent websocket write, callerPanics=%d log=%s", callerPanic.Load(), logged)
	}
}

func TestAudit_NW01_CloseReapsConnection(t *testing.T) {
	saveAuditGlobals(t)
	loadAuditConfig(t, 3600, 4, 1, 1)
	noopAuditLogs()
	ensureIDWorker(t)
	server, client, cleanup := wsPair(t)
	defer cleanup()
	ft := &FireTower{
		startTime: time.Now(),
		readIn:    make(chan *FireInfo, 4),
		sendOut:   make(chan *socket.SendMessage, 4),
		ws:        server,
		closeChan: make(chan struct{}),
	}
	go ft.Run()
	time.Sleep(50 * time.Millisecond)

	finishWithin(t, time.Second, "NW01 Close", func() { ft.Close() })
	select {
	case <-ft.closeChan:
	default:
		t.Fatal("NW01: closeChan is still open")
	}
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := client.ReadMessage(); err == nil {
		t.Fatal("NW01: client still reads a healthy connection after Close")
	}
}

func TestAudit_NW02_SilentClientIsClosed(t *testing.T) {
	saveAuditGlobals(t)
	loadAuditConfig(t, 1, 4, 1, 1)
	noopAuditLogs()
	ensureIDWorker(t)
	server, _, cleanup := wsPair(t)
	defer cleanup()
	ft := &FireTower{
		startTime: time.Now(),
		readIn:    make(chan *FireInfo, 4),
		sendOut:   make(chan *socket.SendMessage, 4),
		ws:        server,
		closeChan: make(chan struct{}),
	}
	// 对端不读。读会自动回 pong，读超时会被续期，静默连接就测不出来。
	go ft.Run()

	select {
	case <-ft.closeChan:
	case <-time.After(3 * time.Second):
		t.Fatal("NW02: client that never sends data was still open after 2 heartbeats")
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
