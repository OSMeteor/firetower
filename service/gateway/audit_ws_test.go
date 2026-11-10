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
	oldConfig := ConfigTree
	// 最后执行：中心循环读的是全局 TM，收尾时停在一条空通道上。
	t.Cleanup(func() {
		TM = &TowerManager{
			bucket: []*Bucket{
				{BuffChan: make(chan *socket.SendMessage, 8)},
				{BuffChan: make(chan *socket.SendMessage, 8)},
			},
			centralChan: make(chan *socket.SendMessage),
		}
		ConfigTree = oldConfig
	})
	loadAuditConfig(t, 3600, 1, 1, 2)
	buildBuckets()
	if len(TM.bucket) < 2 {
		t.Fatal("FT-03: expected 2 buckets")
	}

	client := newMockTower("c-open", 8)
	TM.bucket[1].AddSubscribe("room", client)
	TM.bucket[0].mu.Lock()
	t.Cleanup(func() { TM.bucket[0].mu.Unlock() })

	for i := 0; i < 3; i++ {
		msg := socket.GetSendMessage("ft03", "audit")
		msg.Type = socket.PublishKey
		msg.Topic = "room"
		msg.Data = []byte{byte('a' + i)}
		select {
		case TM.centralChan <- msg:
		case <-time.After(time.Second):
			t.Fatalf("FT-03: central enqueue %d blocked", i)
		}
	}

	deadline := time.After(500 * time.Millisecond)
	got := 0
	for got < 3 {
		select {
		case <-client.sendOut:
			got++
		case <-deadline:
			t.Fatalf("FT-03: open bucket received %d/3 messages while another bucket was wedged", got)
		}
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
	_ = client.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, data, err := client.ReadMessage(); err == nil {
		t.Fatalf("FT-08: ToSelf wrote the socket directly, got %q", data)
	}

	go ft.sendLoop()
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	_, data, err := client.ReadMessage()
	if err != nil || string(data) != "queued-body" {
		t.Fatalf("FT-08: queued payload was not delivered by the single writer: %v %q", err, data)
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
