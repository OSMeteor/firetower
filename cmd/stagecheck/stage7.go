package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/OSMeteor/firetower/grpc/manager"
)

type gwMem struct {
	limit, live, heap uint64
	rejecting         bool
	closed, trimmed   int64
}

func readGwMem(g *gw) (gwMem, error) {
	s, err := httpGet(g.http, "/mem")
	if err != nil {
		return gwMem{}, err
	}
	f := strings.Fields(s)
	if len(f) != 6 {
		return gwMem{}, fmt.Errorf("bad /mem answer %q", s)
	}
	var m gwMem
	m.limit, _ = strconv.ParseUint(f[0], 10, 64)
	m.live, _ = strconv.ParseUint(f[1], 10, 64)
	m.heap, _ = strconv.ParseUint(f[2], 10, 64)
	m.rejecting = f[3] == "true"
	m.closed, _ = strconv.ParseInt(f[4], 10, 64)
	m.trimmed, _ = strconv.ParseInt(f[5], 10, 64)
	return m, nil
}

// stageMemory：用大量连接把 Gateway 的内存推过水位。
// 心跳 10 秒：回心跳的连接一直续命；不回的 15 秒后在保护机制眼里就是死的，而原有的读超时要 20 秒才关它。
// 水位故意设成 hard 低于 soft：先进入清理，再进入拒绝新连接，两段都能看到。
func stageMemory(keep bool) {
	c, err := newCluster("mem")
	if !must("MEM-00", "建目录", err) {
		return
	}
	defer finish(c, "MEM-99", keep)
	m, err := c.addManager("m1")
	if !must("MEM-00", "拉起 Manager", err) {
		return
	}
	const limitMB = 64
	c.extraTop = "chanLens = 4000\nheartbeat = 10\nmaxConnections = 100000\n"
	c.extraTables = fmt.Sprintf("\n[memory]\nlimitMB = %d\nsoftPercent = 70\nhardPercent = 40\ncriticalPercent = 99\n", limitMB)
	g, err := c.addGateway(1)
	c.extraTop, c.extraTables = "", ""
	if !must("MEM-00", "拉起带内存上限的 Gateway", err) {
		return
	}
	mem0, err := readGwMem(g)
	if !must("MEM-00", "读内存状态", err) {
		return
	}
	if mem0.limit != limitMB<<20 {
		rep.check("MEM-00", "内存保护按配置打开", fmt.Errorf("limit=%d，应为 %d", mem0.limit, limitMB<<20))
		return
	}
	note("起步时存活堆 %dMB，上限 %dMB（清理从 40%%，拒绝新连接从 70%%）", mem0.live>>20, limitMB)

	var peakLive atomic.Uint64
	var sawReject atomic.Bool
	stopSample := make(chan struct{})
	var sw sync.WaitGroup
	sw.Add(1)
	go func() {
		defer sw.Done()
		for {
			select {
			case <-stopSample:
				return
			case <-time.After(100 * time.Millisecond):
			}
			if s, err := readGwMem(g); err == nil {
				if s.live > peakLive.Load() {
					peakLive.Store(s.live)
				}
				if s.rejecting {
					sawReject.Store(true)
				}
			}
		}
	}()

	// 卡死的连接：订了房间，然后不读、不回心跳。
	var stuck []*client
	for i := 0; i < 40; i++ {
		cl, err := dial(g, fmt.Sprintf("stuck-%d", i))
		if !must("MEM-00", "客户端连接", err) {
			return
		}
		_ = cl.subscribe("room")
		stuck = append(stuck, cl)
	}
	time.Sleep(300 * time.Millisecond)
	for _, cl := range stuck {
		cl.stall.Store(true)
	}
	stuckAt := time.Now()

	// 正常连接：一批批开，直到被拒。
	var healthy []*client
	rejected := 0
	for len(healthy) < 6000 && rejected < 20 {
		cl, err := dial(g, fmt.Sprintf("ok-%d", len(healthy)))
		if err != nil {
			rejected++
			continue
		}
		_ = cl.subscribe("room")
		if len(healthy)%100 == 99 {
			time.Sleep(150 * time.Millisecond)
		}
		if cl.dead.Load() {
			rejected++
			continue
		}
		healthy = append(healthy, cl)
	}
	time.Sleep(time.Second)
	// 被拒的连接是建立后马上被服务端关掉的，这里把它们剔出去。
	healthy = filter(healthy, func(x *client) bool { return !x.dead.Load() })
	note("开了 %d 条正常连接后开始被拒", len(healthy))

	// 等卡死的连接沉默超过 15 秒、但还不到 20 秒的读超时。
	if wait := 17*time.Second - time.Since(stuckAt); wait > 0 {
		time.Sleep(wait)
	}
	st, _ := readGwMem(g)
	note("卡死 %s 时，因内存关掉 %d 条连接", time.Since(stuckAt).Round(100*time.Millisecond), st.closed)
	var e error
	switch {
	case !g.p.alive():
		e = fmt.Errorf("Gateway 进程退出了")
	case peakLive.Load() >= limitMB<<20:
		e = fmt.Errorf("存活堆到了 %dMB，超过上限 %dMB", peakLive.Load()>>20, limitMB)
	case !sawReject.Load():
		e = fmt.Errorf("没有进入拒绝新连接的状态，用例没测到第一道水位")
	case st.closed < int64(len(stuck)):
		e = fmt.Errorf("读超时之前，保护机制只关了 %d 条卡死的连接，应至少 %d 条", st.closed, len(stuck))
	}
	rep.check("MEM-01", "进程不崩，存活堆一直低于上限；读超时之前，不回心跳的连接已被保护机制优先关掉", e)
	note("存活堆峰值 %dMB", peakLive.Load()>>20)

	e = nil
	for _, cl := range healthy {
		if cl.dead.Load() {
			e = fmt.Errorf("一直回心跳的连接 %s 被关了", cl.name())
			break
		}
	}
	if e == nil {
		e = directNumIs(m, []string{"room"}, len(healthy), 15*time.Second)
	}
	if e == nil {
		e = deliver(healthy[0], "room", healthy, stuck, 10*time.Second)
	}
	rep.check("MEM-02", "一直回心跳的连接全部保留：人数正好等于它们的个数，每条都恰好收到一次", e)

	probe, derr := dial(g, "probe-high")
	e = derr
	if e == nil {
		_ = probe.subscribe("room")
		time.Sleep(500 * time.Millisecond)
		if !probe.dead.Load() && st.rejecting {
			e = fmt.Errorf("内存高于第一道水位时新连接没被拒")
		}
		probe.close()
	}
	rep.check("MEM-03", "内存高于第一道水位时新连接被拒", e)

	// 关掉三分之二的连接，保持少量流量让 GC 自然发生，看内存能不能降回来。
	keepN := len(healthy) / 3
	for _, cl := range healthy[keepN:] {
		cl.close()
	}
	healthy = healthy[:keepN]
	start := time.Now()
	e = fmt.Errorf("内存没有回落")
	for time.Since(start) < 150*time.Second {
		_ = healthy[0].publish("room", newTok())
		if s, err := readGwMem(g); err == nil && !s.rejecting {
			e = nil
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	close(stopSample)
	sw.Wait()
	down := time.Since(start)
	if e == nil {
		late, derr := dial(g, "after")
		if derr != nil {
			e = derr
		} else {
			_ = late.subscribe("room")
			time.Sleep(500 * time.Millisecond)
			if late.dead.Load() {
				e = fmt.Errorf("内存降回来以后新连接仍被拒绝")
			} else {
				healthy = append(healthy, late)
			}
		}
	}
	if e == nil {
		e = directNumIs(m, []string{"room"}, len(healthy), 15*time.Second)
	}
	if e == nil {
		e = deliver(healthy[0], "room", healthy, stuck, 10*time.Second)
	}
	rep.check("MEM-04", "断开一部分连接后内存降回第一道水位以下，重新接受新连接，人数和投递都正确", e)
	note("内存回落用了 %s（没有主动触发 GC）", down.Round(100*time.Millisecond))
	for _, cl := range healthy {
		cl.close()
	}
}

// stageManagerMemory：Manager 到某台 Gateway 的链路卡住，后端猛发大消息，Manager 的待发队列会把内存推上去。
func stageManagerMemory(keep bool) {
	c, err := newCluster("mmem")
	if !must("MM-00", "建目录", err) {
		return
	}
	defer finish(c, "MM-99", keep)
	const limitMB = 200
	memHTTP := fmt.Sprintf("127.0.0.1:%d", freePort())
	c.managerEnv = []string{fmt.Sprintf("SC_MEMMB=%d", limitMB), "SC_MEMHTTP=" + memHTTP}
	m, err := c.addManagerBehindLinks("m1")
	if !must("MM-00", "拉起 Manager", err) {
		return
	}
	var gs []*gw
	for i := 1; i <= 2; i++ {
		g, err := c.addGateway(i)
		if !must("MM-00", "拉起 Gateway", err) {
			return
		}
		gs = append(gs, g)
	}
	var all []*client
	for _, g := range gs {
		for k := 0; k < 3; k++ {
			cl, err := dial(g, fmt.Sprintf("u%d-%d", g.id, k))
			if !must("MM-00", "客户端连接", err) {
				return
			}
			_ = cl.subscribe("big")
			all = append(all, cl)
		}
	}
	if !rep.check("MM-01", "基线", directNumIs(m, []string{"big"}, 6, 10*time.Second)) {
		return
	}
	readMgr := func() (uint64, int64) {
		s, err := httpGet(memHTTP, "/mem")
		if err != nil {
			return 0, 0
		}
		f := strings.Fields(s)
		if len(f) < 2 {
			return 0, 0
		}
		a, _ := strconv.ParseUint(f[0], 10, 64)
		b, _ := strconv.ParseInt(f[1], 10, 64)
		return a, b
	}
	m.viaTCP.stall()
	cl, done, err := rpcClient(m)
	if !must("MM-02", "连 Manager", err) {
		return
	}
	payload := []byte(`{"pad":"` + strings.Repeat("p", 64*1024) + `"}`)
	var peak uint64
	for i := 0; i < 8000; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, _ = cl.Publish(ctx, &pb.PublishRequest{Topic: "big", Data: payload, MessageId: "m", Source: "backend"})
		cancel()
		if i%200 == 0 {
			if live, _ := readMgr(); live > peak {
				peak = live
			}
		}
	}
	done()
	time.Sleep(2 * time.Second)
	live, dropped := readMgr()
	if live > peak {
		peak = live
	}
	note("链路卡住时后端发了 8000 条 64KB。Manager 存活堆峰值 %dMB（上限 %dMB），保护机制丢掉 %d 帧", peak>>20, limitMB, dropped)
	var e error
	switch {
	case !m.p.alive():
		e = fmt.Errorf("Manager 进程退出了")
	case peak >= limitMB<<20:
		e = fmt.Errorf("Manager 存活堆到了 %dMB，超过上限", peak>>20)
	case dropped == 0:
		e = fmt.Errorf("没有推到保护水位，用例没有测到保护机制")
	}
	rep.check("MM-02", "Manager 待发队列积压时进程不崩，存活堆低于上限，保护机制丢掉了积压的帧", e)
	m.viaTCP.resume()
	_, _, e = deliverUntil(all[0], "big", all, 30*time.Second)
	if e == nil {
		e = directNumIs(m, []string{"big"}, 6, 10*time.Second)
	}
	rep.check("MM-03", "链路恢复后 Gateway 连接还在，投递恰好一次，人数正确", e)
}
