package gateway

import (
	"fmt"
	"testing"
	"time"

	"github.com/OSMeteor/firetower/socket"
	"github.com/gorilla/websocket"
	"github.com/pelletier/go-toml"
)

// memTower 造一条登记在 liveTowers 里的连接。silent 是距最后一次心跳多久，backlog 是发送队列里堆了几条。
func memTower(t *testing.T, id uint64, silent time.Duration, backlog int, age time.Duration) *FireTower {
	t.Helper()
	ft := auditTower(t, fmt.Sprintf("mem-%d", id), "u")
	ft.connId = id
	ft.sendOut = make(chan *socket.SendMessage, 64)
	ft.startTime = time.Now().Add(-age)
	ft.lastActive.Store(time.Now().Add(-silent).UnixNano())
	for i := 0; i < backlog; i++ {
		ft.sendOut <- &socket.SendMessage{Topic: "room", Data: []byte("x")}
	}
	liveTowers.Store(id, ft)
	t.Cleanup(func() { liveTowers.Delete(id) })
	return ft
}

func memIsClosed(ft *FireTower) bool {
	ft.mutex.Lock()
	defer ft.mutex.Unlock()
	return ft.isClose
}

func memWaitClosed(t *testing.T, fts ...*FireTower) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		all := true
		for _, ft := range fts {
			if !memIsClosed(ft) {
				all = false
			}
		}
		if all {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("connections chosen by the memory guard were not closed")
}

func memSetup(t *testing.T) *memState {
	t.Helper()
	auditEnv(t)
	liveTowers.Range(func(k, _ any) bool { liveTowers.Delete(k); return true })
	st := &memState{limit: 100 << 20, soft: 70 << 20, hard: 85 << 20, critical: 95 << 20, shedPct: 5}
	return st
}

// 一直有心跳的连接一直续命：不管多老、在什么水位，都不关，队列空的也不动。
func TestGuard_MemoryNeverClosesConnectionsThatKeepHeartbeating(t *testing.T) {
	st := memSetup(t)
	hb := heartbeatInterval()
	old := memTower(t, 1, hb/2, 0, 30*24*time.Hour) // 连了一个月，心跳正常
	fresh := memTower(t, 2, 0, 0, time.Second)
	memShed(st, true, nil)
	time.Sleep(200 * time.Millisecond)
	if memIsClosed(old) || memIsClosed(fresh) {
		t.Fatal("a connection that keeps heartbeating was closed")
	}
}

// 有心跳但积压的连接不关，丢掉队列里的旧消息：hard 时留 1/4，critical 时清空。打开了缺失提示的会记下 gap。
func TestGuard_MemoryTrimsSlowButAliveConnections(t *testing.T) {
	st := memSetup(t)
	slow := memTower(t, 1, 0, 60, time.Hour)
	slow.gapHints.Store(true)
	first := <-slow.sendOut
	first.Data = []byte("oldest")
	// 把一条能认出来的放回队尾，确认丢的是头部的旧消息。
	slow.sendOut <- &socket.SendMessage{Topic: "room", Data: []byte("newest")}
	memShed(st, false, nil)
	if got := len(slow.sendOut); got != cap(slow.sendOut)/4 {
		t.Fatalf("hard: queue left %d, want %d", got, cap(slow.sendOut)/4)
	}
	var last *socket.SendMessage
	for len(slow.sendOut) > 0 {
		last = <-slow.sendOut
	}
	if last == nil || string(last.Data) != "newest" {
		t.Fatal("trim dropped the newest messages instead of the oldest")
	}
	if memIsClosed(slow) {
		t.Fatal("a slow but alive connection was closed")
	}
	slow.gapMu.Lock()
	_, gap := slow.gaps["room"]
	slow.gapMu.Unlock()
	if !gap {
		t.Fatal("dropped messages were not reported as a gap")
	}
	for i := 0; i < 60; i++ {
		slow.sendOut <- &socket.SendMessage{Topic: "room"}
	}
	memShed(st, true, nil)
	if got := len(slow.sendOut); got != 0 {
		t.Fatalf("critical: queue left %d, want 0", got)
	}
	if memIsClosed(slow) {
		t.Fatal("critical level closed a slow but alive connection")
	}
}

// 不存活的连接按「沉默最久 → 积压最多 → 建立最早」排序，hard 时每轮有配额。
func TestGuard_MemoryClosesSilentConnectionsInOrder(t *testing.T) {
	st := memSetup(t)
	hb := heartbeatInterval()
	var all []*FireTower
	// 20 条沉默的：沉默时长 2hb+i 秒；再加 3 条沉默时长相同、用积压和建立时间区分的。
	for i := 0; i < 20; i++ {
		all = append(all, memTower(t, uint64(100+i), 2*hb+time.Duration(i)*time.Second, 0, time.Hour))
	}
	tieOldest := memTower(t, 200, 2*hb-time.Second, 0, 48*time.Hour)
	tieNewer := memTower(t, 201, 2*hb-time.Second, 0, time.Hour)
	tieBacklog := memTower(t, 202, 2*hb-time.Second, 10, time.Minute)
	alive := memTower(t, 300, 0, 0, time.Hour)

	memShed(st, false, nil) // hard：配额 max(16, 24*5%) = 16
	// 沉默最久的 16 条是 i=4..19。
	memWaitClosed(t, all[4:]...)
	time.Sleep(100 * time.Millisecond)
	for i := 0; i < 4; i++ {
		if memIsClosed(all[i]) {
			t.Fatalf("closed silent connection %d out of order", i)
		}
	}
	if memIsClosed(alive) {
		t.Fatal("closed a connection that is still heartbeating")
	}
	// 下一轮：剩下 4 条沉默更久的先关，再按积压、按建立早晚关同样沉默的三条。
	for _, ft := range all[4:] {
		liveTowers.Delete(ft.connId)
	}
	memShed(st, false, nil)
	memWaitClosed(t, all[:4]...)
	memWaitClosed(t, tieBacklog, tieOldest, tieNewer)
	if memIsClosed(alive) {
		t.Fatal("closed a connection that is still heartbeating")
	}

	// 排序本身：同样沉默时，积压多的在前，再按建立时间早的在前。
	now := time.Now()
	cands := []memCandidate{
		{t: tieNewer, silent: time.Minute, backlog: 0, start: now.Add(-time.Hour)},
		{t: tieOldest, silent: time.Minute, backlog: 0, start: now.Add(-48 * time.Hour)},
		{t: tieBacklog, silent: time.Minute, backlog: 10, start: now},
	}
	order := memOrder(cands)
	if order[0].t != tieBacklog || order[1].t != tieOldest || order[2].t != tieNewer {
		t.Fatal("tie-break order is not backlog first, then oldest connection")
	}
}

// 心跳续命：一条连接刚被判为沉默，只要在下一轮之前回了 Pong，就不再是候选。
func TestGuard_MemoryHeartbeatRenewsTheConnection(t *testing.T) {
	st := memSetup(t)
	hb := heartbeatInterval()
	ft := memTower(t, 1, 3*hb, 0, time.Hour)
	ft.touch() // 收到了 Pong
	memShed(st, true, nil)
	time.Sleep(200 * time.Millisecond)
	if memIsClosed(ft) {
		t.Fatal("a connection that just answered a heartbeat was closed")
	}
}

// 真实 WebSocket：服务端发 Ping，客户端自动回 Pong，最后活跃时间会被刷新。
func TestGuard_PongRefreshesLastActive(t *testing.T) {
	auditEnv(t)
	server, client, cleanup := wsPair(t)
	defer cleanup()
	tower := BuildTower(server, "pong")
	if tower == nil {
		t.Fatal("BuildTower returned nil")
	}
	defer tower.Close()
	go tower.readLoop()
	go func() {
		for {
			if _, _, err := client.ReadMessage(); err != nil {
				return
			}
		}
	}()
	tower.lastActive.Store(1)
	if err := server.WriteControl(websocket.PingMessage, []byte("ping"), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if tower.lastActive.Load() > 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("a pong from the client did not refresh the connection")
}

// 超过第一道水位时拒绝新连接，降下来后恢复。
func TestGuard_MemoryRejectsNewConnectionsAboveSoft(t *testing.T) {
	auditEnv(t)
	server, _, cleanup := wsPair(t)
	defer cleanup()
	memRejecting.Store(true)
	if tower := BuildTower(server, "mem-reject"); tower != nil {
		tower.Close()
		memRejecting.Store(false)
		t.Fatal("a new connection was accepted above the soft watermark")
	}
	memRejecting.Store(false)
	server2, _, cleanup2 := wsPair(t)
	defer cleanup2()
	tower := BuildTower(server2, "mem-accept")
	if tower == nil {
		t.Fatal("new connections are still rejected after memory came back down")
	}
	tower.Close()
}

// 没有配置时按机器自动取上限并开启；memory.disable = true 时不开启。
func TestGuard_MemoryGuardDetectsTheLimitByItself(t *testing.T) {
	auditEnv(t)
	prev := memCfg.Load()
	t.Cleanup(func() { memCfg.Store(prev) })

	memCfg.Store(nil)
	startMemoryGuard()
	st := memCfg.Load()
	if want, _ := socket.DetectMemoryLimit(); want > 0 {
		if st == nil || st.limit != want {
			t.Fatalf("memory guard limit = %v, want the detected %d", st, want)
		}
		if st.soft >= st.hard || st.hard >= st.critical || st.critical > st.limit {
			t.Fatalf("watermarks out of order: %+v", *st)
		}
	}

	tree, err := toml.Load("[memory]\ndisable = true\n")
	if err != nil {
		t.Fatal(err)
	}
	storeConfigTree(tree)
	memCfg.Store(nil)
	startMemoryGuard()
	if memCfg.Load() != nil {
		t.Fatal("memory.disable = true did not keep the guard off")
	}

	tree, _ = toml.Load("[memory]\nlimitMB = 123\n")
	storeConfigTree(tree)
	startMemoryGuard()
	if st := memCfg.Load(); st == nil || st.limit != 123<<20 {
		t.Fatal("memory.limitMB did not override the detected limit")
	}
}
