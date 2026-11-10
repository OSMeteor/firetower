package gateway

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/OSMeteor/firetower/grpc/manager"
	"github.com/OSMeteor/firetower/socket"
)

func TestAudit_DL01_CloseWhenUnsubscribeFails(t *testing.T) {
	auditEnv(t)
	storeTopicGrpc(&scriptedTopic{
		unsub: func(context.Context, *pb.UnSubscribeTopicRequest) (*pb.UnSubscribeTopicResponse, error) {
			return nil, errors.New("unsubscribe failed")
		},
	})
	ft := auditTower(t, "c1", "u1")

	finishWithin(t, time.Second, "DL01 Close", func() { ft.Close() })

	select {
	case <-ft.closeChan:
	default:
		t.Fatal("DL01: closeChan is still open")
	}
	if !ft.isClose {
		t.Fatal("DL01: connection was not marked closed")
	}
}

func TestAudit_DL02_BlockedRPCStillNotifiesClose(t *testing.T) {
	auditEnv(t)
	started := make(chan struct{})
	finished := make(chan struct{})
	storeTopicGrpc(&scriptedTopic{
		unsub: func(ctx context.Context, in *pb.UnSubscribeTopicRequest) (*pb.UnSubscribeTopicResponse, error) {
			close(started)
			select {
			case <-ctx.Done():
				close(finished)
				return nil, ctx.Err()
			case <-time.After(5 * time.Second):
				close(finished)
				return nil, errors.New("rpc leash expired")
			}
		},
	})
	ft := auditTower(t, "c1", "u1")

	returned := make(chan struct{})
	go func() {
		ft.Close()
		close(returned)
	}()

	select {
	case <-ft.closeChan:
	case <-time.After(time.Second):
		t.Fatal("DL02: closeChan was not closed while unsubscribe RPC was still blocked")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("DL02: unsubscribe RPC was not called")
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("DL02: unsubscribe RPC was not cancelled")
	}
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("DL02: Close did not return")
	}
}

func TestAudit_DL03_ConcurrentClose(t *testing.T) {
	auditEnv(t)
	ft := auditTower(t, "c1", "u1")
	var offline atomic.Int32
	ft.onOfflineHandler = func() { offline.Add(1) }

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	errCh := make(chan error, 1)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					select {
					case errCh <- fmt.Errorf("panic: %v", r):
					default:
					}
				}
			}()
			<-start
			ft.Close()
		}()
	}
	close(start)
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("DL03: concurrent Close did not finish")
	}
	select {
	case err := <-errCh:
		t.Fatal(err)
	default:
	}
	select {
	case <-ft.closeChan:
	default:
		t.Fatal("DL03: closeChan is still open")
	}
	if got := offline.Load(); got != 1 {
		t.Fatalf("DL03: offline handler ran %d times, want 1", got)
	}
}

func TestAudit_DL04_ReentrantClose(t *testing.T) {
	t.Run("offline handler", func(t *testing.T) {
		auditEnv(t)
		ft := auditTower(t, "c1", "u1")
		ft.onOfflineHandler = func() { ft.Close() }
		finishWithin(t, time.Second, "DL04 offline Close", func() { ft.Close() })
		select {
		case <-ft.closeChan:
		default:
			t.Fatal("DL04: closeChan is still open")
		}
	})

	t.Run("unsubscribe handler", func(t *testing.T) {
		auditEnv(t)
		ft := auditTower(t, "c1", "u1")
		ft.unSubscribeHandler = func(*FireLife, []string) bool {
			ft.Close()
			return true
		}
		finishWithin(t, time.Second, "DL04 unsubscribe Close", func() { ft.Close() })
		select {
		case <-ft.closeChan:
		default:
			t.Fatal("DL04: closeChan is still open")
		}
	})

	t.Run("logger calls Close once", func(t *testing.T) {
		auditEnv(t)
		ft := auditTower(t, "c1", "u1")
		var called atomic.Bool
		storeTowerLogger(func(target *FireTower, _, _ string) {
			if target == ft && called.CompareAndSwap(false, true) {
				ft.Close()
			}
		})
		finishWithin(t, time.Second, "DL04 logger Close", func() { ft.Close() })
		select {
		case <-ft.closeChan:
		default:
			t.Fatal("DL04: closeChan is still open")
		}
	})
}

func TestAudit_DL05_UnsubscribeByUser(t *testing.T) {
	bucket := auditEnv(t)
	ft := auditTower(t, "c1", "u1")
	bucket.AddSubscribe("room", ft)

	finishWithin(t, time.Second, "DL05 unSubscribeByUserId", func() {
		_ = bucket.unSubscribeByUserId(&socket.SendMessage{Topic: "room", Data: []byte("u1")})
	})

	bucket.mu.RLock()
	defer bucket.mu.RUnlock()
	if _, ok := bucket.topicRelevance["room"]["c1"]; ok {
		t.Fatal("DL05: user subscription is still present")
	}
}

func TestAudit_DL06_UnsubscribeAll(t *testing.T) {
	bucket := auditEnv(t)
	first := auditTower(t, "c1", "u1")
	second := auditTower(t, "c2", "u2")
	second.topic = map[string]bool{"room": true}
	bucket.AddSubscribe("room", first)
	bucket.AddSubscribe("room", second)

	finishWithin(t, time.Second, "DL06 unSubscribeAll", func() {
		_ = bucket.unSubscribeAll(&socket.SendMessage{Topic: "room"})
	})

	bucket.mu.RLock()
	defer bucket.mu.RUnlock()
	if subs := bucket.topicRelevance["room"]; len(subs) != 0 {
		t.Fatalf("DL06: topic still has %d subscribers", len(subs))
	}
}

func TestAudit_DL07_KickUser(t *testing.T) {
	bucket := auditEnv(t)
	ft := auditTower(t, "c1", "u1")
	bucket.AddSubscribe("room", ft)

	finishWithin(t, time.Second, "DL07 offlineUsers", func() {
		_ = bucket.offlineUsers(&socket.SendMessage{Topic: "room", Data: []byte("u1")})
	})
	select {
	case <-ft.closeChan:
	default:
		t.Fatal("DL07: kicked connection did not close")
	}
}

func TestAudit_DL08_FullQueueLogCloses(t *testing.T) {
	bucket := auditEnv(t)
	ft := auditTower(t, "c1", "u1")
	ft.sendOut = make(chan *socket.SendMessage, 1)
	ft.sendOut <- &socket.SendMessage{}
	bucket.AddSubscribe("room", ft)
	storeTowerLogger(func(target *FireTower, kind, _ string) {
		if target == ft && kind == "WARN" {
			ft.Close()
		}
	})

	finishWithin(t, time.Second, "DL08 push", func() {
		_ = bucket.push(&socket.SendMessage{Type: socket.PublishKey, Topic: "room", Data: []byte("x")})
	})
	select {
	case <-ft.closeChan:
	default:
		t.Fatal("DL08: close requested from the full-queue log did not finish")
	}
}

func TestAudit_FT02_OldCleanupKeepsNewConnection(t *testing.T) {
	bucket := &Bucket{topicRelevance: make(map[string]map[string]*FireTower)}
	old := &FireTower{ClientId: "same"}
	newer := &FireTower{ClientId: "same"}
	bucket.AddSubscribe("room", old)
	bucket.AddSubscribe("room", newer)

	bucket.DelSubscribe("room", old)

	bucket.mu.RLock()
	defer bucket.mu.RUnlock()
	if got := bucket.topicRelevance["room"]["same"]; got != newer {
		t.Fatalf("FT-02: old connection cleanup removed the new connection (%p), want %p", got, newer)
	}
}
