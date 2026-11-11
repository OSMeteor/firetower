package gateway

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/OSMeteor/firetower/socket"
)

// 业务回调和业务替换的日志函数都可能 panic。它们跑在谁的协程上，都不能带走进程。

func TestGuard_OnConnectPanicInGoRunClosesTower(t *testing.T) {
	auditEnv(t)
	server, _, cleanup := wsPair(t)
	defer cleanup()
	tower := BuildTower(server, "guard-connect")
	if tower == nil {
		t.Fatal("BuildTower returned nil")
	}
	tower.SetOnConnectHandler(func() bool { panic("business bug in onConnect") })
	done := make(chan struct{})
	go func() {
		defer close(done)
		tower.Run()
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after onConnect panicked")
	}
	tower.mutex.Lock()
	closed := tower.isClose
	tower.mutex.Unlock()
	if !closed {
		t.Fatal("a panicking onConnect handler must close the tower")
	}
}

func TestGuard_CloseSurvivesUnsubscribePanicAndStillCallsOffline(t *testing.T) {
	auditEnv(t)
	server, _, cleanup := wsPair(t)
	defer cleanup()
	tower := BuildTower(server, "guard-close")
	if tower == nil {
		t.Fatal("BuildTower returned nil")
	}
	if _, err := tower.bindTopic([]string{"room"}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	var unsubCalled, offlineCalled atomic.Bool
	tower.SetUnSubscribeHandler(func(*FireLife, []string) bool {
		unsubCalled.Store(true)
		panic("business bug in unsubscribe")
	})
	tower.SetOnOfflineHandler(func() { offlineCalled.Store(true) })

	var escaped interface{}
	finishWithin(t, 3*time.Second, "Close with a panicking unsubscribe handler", func() {
		defer func() { escaped = recover() }()
		tower.Close()
	})
	if escaped != nil {
		t.Fatalf("panic escaped Close: %v", escaped)
	}
	if !unsubCalled.Load() {
		t.Fatal("unsubscribe handler was not called")
	}
	if !offlineCalled.Load() {
		t.Fatal("offline handler was skipped after the unsubscribe handler panicked")
	}
}

func TestGuard_CloseSurvivesOfflinePanic(t *testing.T) {
	auditEnv(t)
	server, _, cleanup := wsPair(t)
	defer cleanup()
	tower := BuildTower(server, "guard-offline")
	if tower == nil {
		t.Fatal("BuildTower returned nil")
	}
	tower.SetOnOfflineHandler(func() { panic("business bug in offline") })
	var escaped interface{}
	finishWithin(t, 3*time.Second, "Close with a panicking offline handler", func() {
		defer func() { escaped = recover() }()
		tower.Close()
	})
	if escaped != nil {
		t.Fatalf("panic escaped Close: %v", escaped)
	}
	// 再关一次也不能出事。
	tower.Close()
}

func TestGuard_PanickingLoggersDoNotEscape(t *testing.T) {
	auditEnv(t)
	storeTowerLogger(func(*FireTower, string, string) { panic("tower logger bug") })
	storeFireLogger(func(*FireInfo, string, string) { panic("fire logger bug") })
	prev := socket.SendLogger
	socket.SendLogger = func(*socket.SendMessage, string, string) { panic("send logger bug") }
	t.Cleanup(func() { socket.SendLogger = prev })

	server, _, cleanup := wsPair(t)
	defer cleanup()
	var escaped interface{}
	finishWithin(t, 5*time.Second, "calls with panicking loggers", func() {
		defer func() { escaped = recover() }()
		tower := BuildTower(server, "guard-logger")
		if tower == nil {
			return
		}
		towerLog(tower, "ERROR", "x")
		logInfo(tower, "x")
		fire := NewFireInfo(tower, nil)
		fire.Message.Topic = "room"
		fire.Message.Data = []byte(`{}`)
		fire.Info("x")
		fire.Error("x")
		_ = tower.Publish(fire)
		_ = tower.ToSelf([]byte(`{}`))
		msg := socket.GetSendMessage("0", "guard")
		msg.Info("x")
		msg.Error("x")
		msg.Panic("x")
		BuildTower(nil, "nil-ws")
		tower.Close()
	})
	if escaped != nil {
		t.Fatalf("logger panic escaped: %v", escaped)
	}
}
