package gateway

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	pb "github.com/OSMeteor/firetower/grpc/manager"
	"github.com/OSMeteor/firetower/socket"
	"github.com/gorilla/websocket"
)

func TestAudit_FT03_QueuedMessageKeepsPayload(t *testing.T) {
	oldConfig := ConfigTree
	t.Cleanup(func() {
		TM = &TowerManager{
			bucket:      []*Bucket{{BuffChan: make(chan *socket.SendMessage, 4)}},
			centralChan: make(chan *socket.SendMessage),
		}
		ConfigTree = oldConfig
	})
	loadAuditConfig(t, 3600, 4, 1, 1)
	buildBuckets()

	client := newMockTower("payload", 4)
	TM.bucket[0].AddSubscribe("room", client)
	TM.bucket[0].mu.Lock()
	unlocked := false
	t.Cleanup(func() {
		if !unlocked {
			TM.bucket[0].mu.Unlock()
		}
	})

	msg := socket.GetSendMessage("ft03-own", "audit")
	msg.Type = socket.PublishKey
	msg.Topic = "room"
	msg.Data = []byte("original-payload")
	select {
	case TM.centralChan <- msg:
	case <-time.After(time.Second):
		t.Fatal("FT-03: central enqueue blocked")
	}
	deadline := time.After(time.Second)
	for len(TM.bucket[0].BuffChan) != 0 {
		select {
		case <-deadline:
			t.Fatal("FT-03: bucket did not take the queued message")
		case <-time.After(5 * time.Millisecond):
		}
	}
	msg.Data = []byte("hijacked-payload")
	TM.bucket[0].mu.Unlock()
	unlocked = true

	select {
	case got := <-client.sendOut:
		if string(got.Data) != "original-payload" {
			t.Fatalf("FT-03: queued message changed after it was handed to the bucket: %q", got.Data)
		}
	case <-time.After(time.Second):
		t.Fatal("FT-03: subscriber did not receive the queued message")
	}
}

func TestAudit_FT07_NilIdWorkerDoesNotKillRead(t *testing.T) {
	saveAuditGlobals(t)
	loadAuditConfig(t, 3600, 4, 1, 1)
	noopAuditLogs()
	server, client, cleanup := wsPair(t)
	defer cleanup()
	ft := &FireTower{
		startTime: time.Now(),
		readIn:    make(chan *FireInfo, 4),
		sendOut:   make(chan *socket.SendMessage, 4),
		ws:        server,
		closeChan: make(chan struct{}),
	}
	IdWorker = nil
	go ft.readLoop()
	if err := client.WriteMessage(websocket.TextMessage, []byte(`{"topic":"room","type":"subscribe","data":{}}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case fire := <-ft.readIn:
		if fire == nil || fire.Context == nil || fire.Context.id == "" {
			t.Fatal("FT-07: nil IdWorker dropped the frame or left the context id empty")
		}
	case <-time.After(time.Second):
		t.Fatal("FT-07: nil IdWorker stalled readLoop")
	}
	select {
	case <-ft.closeChan:
		t.Fatal("FT-07: nil IdWorker closed the connection")
	default:
	}
}

func TestAudit_FT07_ReadPanicCloses(t *testing.T) {
	saveAuditGlobals(t)
	loadAuditConfig(t, 3600, 4, 1, 1)
	noopAuditLogs()
	server, client, cleanup := wsPair(t)
	defer cleanup()
	ft := &FireTower{
		startTime: time.Now(),
		readIn:    make(chan *FireInfo),
		sendOut:   make(chan *socket.SendMessage, 4),
		ws:        server,
		closeChan: make(chan struct{}),
	}
	close(ft.readIn)
	ensureIDWorker(t)
	go ft.readLoop()
	if err := client.WriteMessage(websocket.TextMessage, []byte(`{"topic":"room","type":"subscribe","data":{}}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ft.closeChan:
	case <-time.After(time.Second):
		t.Fatal("FT-07: readLoop panic left the connection open")
	}
}

func TestAudit_FT07_OversizedFrameCloses(t *testing.T) {
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
	go ft.readLoop()
	payload := make([]byte, 1<<20+64)
	for i := range payload {
		payload[i] = 'a'
	}
	_ = client.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if err := client.WriteMessage(websocket.TextMessage, payload); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ft.closeChan:
	case <-time.After(time.Second):
		t.Fatal("FT-07: client frame above 1MiB did not close the connection")
	}
}

func TestAudit_FT09_SubscribeHasDeadline(t *testing.T) {
	auditEnv(t)
	finished := make(chan struct{})
	topicManageGrpc = &scriptedTopic{
		sub: func(ctx context.Context, in *pb.SubscribeTopicRequest) (*pb.SubscribeTopicResponse, error) {
			select {
			case <-ctx.Done():
				close(finished)
				return nil, ctx.Err()
			case <-time.After(5 * time.Second):
				close(finished)
				return nil, errors.New("subscribe leash expired")
			}
		},
	}
	ft := auditTower(t, "c1", "u1")
	ft.topic = map[string]bool{}

	returned := make(chan error, 1)
	go func() {
		_, err := ft.bindTopic([]string{"room"})
		returned <- err
	}()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("FT-09: subscribe RPC was not cancelled")
	}
	select {
	case err := <-returned:
		if err == nil {
			t.Fatal("FT-09: bindTopic returned success after the RPC was cancelled")
		}
	case <-time.After(time.Second):
		t.Fatal("FT-09: bindTopic did not return")
	}
}

func TestAudit_FT09_SubscribeBeforeManagerReady(t *testing.T) {
	bucket := auditEnv(t)
	topicManage.Conn = nil
	ft := &FireTower{topic: map[string]bool{}}
	_, err := ft.bindTopic([]string{"room"})
	if err == nil {
		t.Fatal("FT-09: subscribe succeeded before the manager connection existed")
	}
	if _, ok := bucket.topicRelevance["room"]; ok {
		t.Fatal("FT-09: local subscription was stored before the manager was ready")
	}
}

func TestAudit_NW03_OpenPeerWithNoFramesTimesOut(t *testing.T) {
	saveAuditGlobals(t)
	loadAuditConfig(t, 1, 4, 1, 1)
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
	go func() {
		for {
			if _, _, err := client.ReadMessage(); err != nil {
				return
			}
		}
	}()
	go ft.readLoop()
	select {
	case <-ft.closeChan:
	case <-time.After(3 * time.Second):
		t.Fatal("NW03: peer that stays open but sends no frames left ReadMessage blocked")
	}
}

func TestAudit_NW05_ReconnectReplaysSubscriptions(t *testing.T) {
	auditEnv(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	accepted := make(chan net.Conn, 4)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- conn
		}
	}()

	cli := socket.NewClient(ln.Addr().String())
	if err := cli.Connect(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cli.Shutdown() })
	var first net.Conn
	select {
	case first = <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("NW05: manager did not accept the gateway connection")
	}
	topicManage = cli

	var mu sync.Mutex
	var calls []string
	topicManageGrpc = &scriptedTopic{
		sub: func(ctx context.Context, in *pb.SubscribeTopicRequest) (*pb.SubscribeTopicResponse, error) {
			mu.Lock()
			calls = append(calls, in.Ip+" "+strings.Join(in.Topic, ","))
			mu.Unlock()
			return &pb.SubscribeTopicResponse{}, nil
		},
	}
	ft := &FireTower{topic: map[string]bool{}, connId: 0}
	if _, err := ft.bindTopic([]string{"room"}); err != nil {
		t.Fatalf("initial subscribe: %v", err)
	}
	oldIP := cli.CurrentAddr()
	_ = first.Close()

	select {
	case <-accepted:
	case <-time.After(3 * time.Second):
		t.Fatal("NW05: manager TCP connection was not re-established")
	}
	deadline := time.After(2 * time.Second)
	var newIP string
	for {
		newIP = cli.CurrentAddr()
		if newIP != "" && newIP != oldIP {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("NW05: reconnect kept the old subscription address %s", oldIP)
		case <-time.After(10 * time.Millisecond):
		}
	}
	deadline = time.After(2 * time.Second)
	for {
		mu.Lock()
		replayed := false
		for _, call := range calls {
			if strings.HasPrefix(call, newIP+" ") && strings.Contains(call, "room") {
				replayed = true
			}
		}
		recorded := append([]string(nil), calls...)
		mu.Unlock()
		if replayed {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("NW05: subscriptions were not replayed on %s, calls=%v", newIP, recorded)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func TestAudit_LG01_RepeatedCloseClearsSubscriptions(t *testing.T) {
	bucket := auditEnv(t)
	for i := 0; i < 8; i++ {
		ft := auditTower(t, "lg-"+itoa(i), "user")
		ft.topic = map[string]bool{}
		if _, err := ft.bindTopic([]string{"room"}); err != nil {
			t.Fatalf("subscribe %d: %v", i, err)
		}
		finishWithin(t, time.Second, "LG01 Close", func() { ft.Close() })
	}
	bucket.mu.RLock()
	defer bucket.mu.RUnlock()
	if subs := bucket.topicRelevance["room"]; len(subs) != 0 {
		t.Fatalf("LG01: %d subscriptions remained after repeated close", len(subs))
	}
}

func TestAudit_FT11_StressPackageBuilds(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	out := filepath.Join(t.TempDir(), "stress.test.exe")
	cmd := exec.Command("go", "test", "-c", "-o", out, "./stress")
	cmd.Dir = root
	cmd.Env = os.Environ()
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("FT-11: go test cannot build ./stress:\n%s", output)
	}
}

func TestAudit_NilPathsDoNotPanic(t *testing.T) {
	saveAuditGlobals(t)
	IdWorker = nil
	defer func() {
		if recover() != nil {
			t.Fatal("nil path panicked")
		}
	}()

	var tower *FireTower
	tower.Close()
	tower.Run()
	if err := tower.Send(nil); err == nil {
		t.Fatal("nil tower Send returned success")
	}
	if _, err := tower.read(); err == nil {
		t.Fatal("nil tower read returned success")
	}
	towerLog(nil, "ERROR", "nil tower")

	var info *FireInfo
	info.Panic("nil")
	info.Info("nil")
	info.Error("nil")
	if info.GetContextId() != "" {
		t.Fatal("nil FireInfo returned a context id")
	}

	var tm *TowerManager
	if tm.GetBucket(&FireTower{}) != nil {
		t.Fatal("nil manager returned a bucket")
	}
	if (&TowerManager{}).GetBucket(&FireTower{}) != nil {
		t.Fatal("manager with no buckets returned a bucket")
	}

	fire := NewFireInfo(nil, nil)
	if fire == nil || fire.Context == nil || fire.Context.id == "" || fire.Message == nil {
		t.Fatal("nil IdWorker produced an incomplete FireInfo")
	}
}
