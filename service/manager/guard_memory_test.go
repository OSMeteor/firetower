package manager

import (
	"testing"

	"github.com/OSMeteor/firetower/socket"
)

// 内存保护只丢积压 Gateway 的待发帧，不碰没积压的；同一条连接挂在几个键下只处理一次。
func TestGuard_MemoryTrimsOnlyBackedUpGateways(t *testing.T) {
	busy := &connectBucket{outbox: make(chan []byte, 16), closeChan: make(chan struct{})}
	idle := &connectBucket{outbox: make(chan []byte, 16), closeChan: make(chan struct{})}
	for i := 0; i < 12; i++ {
		busy.outbox <- []byte{byte(i)}
	}
	for i := 0; i < 3; i++ {
		idle.outbox <- []byte{byte(i)}
	}
	ConnIndexTable.Store("guard-mem-busy-addr", busy)
	ConnIndexTable.Store("guard-mem-busy-key", busy)
	ConnIndexTable.Store("guard-mem-idle", idle)
	t.Cleanup(func() {
		ConnIndexTable.Delete("guard-mem-busy-addr")
		ConnIndexTable.Delete("guard-mem-busy-key")
		ConnIndexTable.Delete("guard-mem-idle")
	})

	if got := memTrimOutboxes(false); got != 8 {
		t.Fatalf("hard: dropped %d frames, want 8", got)
	}
	if len(busy.outbox) != 4 || len(idle.outbox) != 3 {
		t.Fatalf("hard: busy=%d idle=%d, want 4 and untouched 3", len(busy.outbox), len(idle.outbox))
	}
	// 留下的是最新的几帧。
	if b := <-busy.outbox; b[0] != 8 {
		t.Fatalf("hard kept frame %d first, want the newest four starting at 8", b[0])
	}
	if got := memTrimOutboxes(true); got != 3+3 {
		t.Fatalf("critical: dropped %d frames, want 6", got)
	}
	if len(busy.outbox) != 0 || len(idle.outbox) != 0 {
		t.Fatal("critical did not empty every outbox")
	}
}

func TestGuard_ManagerMemoryGuardDetectsTheLimitByItself(t *testing.T) {
	prevLimit, prevOff := memLimit.Load(), memGuardOff.Load()
	t.Cleanup(func() { memLimit.Store(prevLimit); memGuardOff.Store(prevOff) })

	memLimit.Store(0)
	memGuardOff.Store(false)
	EnableMemoryGuard(0, 0, 0) // 0 不改变任何东西
	if memLimit.Load() != 0 {
		t.Fatal("EnableMemoryGuard(0) changed the limit")
	}
	autoMemoryGuard()
	if want, _ := socket.DetectMemoryLimit(); want > 0 && MemoryGuardLimit() != want {
		t.Fatalf("limit = %d, want the detected %d", MemoryGuardLimit(), want)
	}
	EnableMemoryGuard(321, 0, 0)
	if MemoryGuardLimit() != 321<<20 {
		t.Fatal("EnableMemoryGuard did not override the detected limit")
	}
	autoMemoryGuard() // 手动设置过就不再自动覆盖
	if MemoryGuardLimit() != 321<<20 {
		t.Fatal("auto detection overwrote a limit that was set by hand")
	}

	memLimit.Store(0)
	DisableMemoryGuard()
	autoMemoryGuard()
	if MemoryGuardLimit() != 0 {
		t.Fatal("DisableMemoryGuard did not keep the guard off")
	}
}
