package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/OSMeteor/firetower/socket"
)

// stageOptions：上限，以及两个默认关闭、由接入方按连接打开的功能（缺失提示、被动订阅）。
func stageOptions(keep bool) {
	c, err := newCluster("opt")
	if !must("O-00", "建目录", err) {
		return
	}
	defer finish(c, "O-99", keep)
	m, err := c.addManager("m1")
	if !must("O-00", "拉起 Manager", err) {
		return
	}
	// gw1 用很小的上限，gw2 用默认值。
	c.extraTop = "maxConnections = 6\nmaxTopicsPerConn = 3\nmaxTopicsPerFrame = 2\n"
	small, err := c.addGateway(1)
	if !must("O-00", "拉起带上限的 Gateway", err) {
		return
	}
	c.extraTop = ""
	normal, err := c.addGateway(2)
	if !must("O-00", "拉起普通 Gateway", err) {
		return
	}

	// 单帧 Topic 数上限。
	a, err := dial(small, "limit-a")
	if !must("O-01", "客户端连接", err) {
		return
	}
	_ = a.subscribe("f1", "f2", "f3") // 3 个，超过单帧上限 2，整帧拒绝
	time.Sleep(500 * time.Millisecond)
	e := directNumIs(m, []string{"f1", "f2", "f3"}, 0, 2*time.Second)
	_ = a.subscribe("f1", "f2")
	if e == nil {
		e = directNumIs(m, []string{"f1", "f2"}, 1, 5*time.Second)
	}
	rep.check("O-01", "一帧里的 Topic 数超过上限时整帧被拒，没有只订上一部分；不超的照常", e)
	// 单连接 Topic 数上限 3：已有 2 个，再订 2 个会超。
	_ = a.subscribe("c3", "c4")
	time.Sleep(500 * time.Millisecond)
	e = directNumIs(m, []string{"c3", "c4"}, 0, 2*time.Second)
	_ = a.subscribe("c3")
	if e == nil {
		e = directNumIs(m, []string{"c3"}, 1, 5*time.Second)
	}
	_ = a.subscribe("c5")
	time.Sleep(500 * time.Millisecond)
	if e == nil {
		e = directNumIs(m, []string{"c5"}, 0, 2*time.Second)
	}
	if e == nil {
		_ = a.unsubscribe("f1")
		_ = a.subscribe("c5")
		e = directNumIs(m, []string{"c5"}, 1, 5*time.Second)
	}
	rep.check("O-02", "单连接 Topic 数到上限后新的订阅被拒；退订一个腾出位置后又能订", e)

	// 连接数上限 6：已有 1 条。
	var held []*client
	for i := 0; i < 5; i++ {
		cl, derr := dial(small, fmt.Sprintf("limit-%d", i))
		if derr != nil {
			e = derr
			break
		}
		_ = cl.subscribe("room")
		held = append(held, cl)
	}
	if e == nil {
		e = directNumIs(m, []string{"room"}, 5, 5*time.Second)
	}
	over, derr := dial(small, "limit-over")
	if e == nil && derr == nil {
		_ = over.subscribe("room")
		time.Sleep(700 * time.Millisecond)
		if !over.dead.Load() {
			e = fmt.Errorf("第 7 条连接没有被拒绝")
		}
	}
	if e == nil {
		e = directNumIs(m, []string{"room"}, 5, 2*time.Second)
	}
	held[0].close()
	time.Sleep(500 * time.Millisecond)
	again, derr := dial(small, "limit-again")
	if e == nil {
		if derr != nil {
			e = derr
		} else {
			_ = again.subscribe("room")
			e = directNumIs(m, []string{"room"}, 5, 5*time.Second)
			if e == nil && again.dead.Load() {
				e = fmt.Errorf("腾出一个位置后新连接仍被拒绝")
			}
		}
	}
	other, derr := dial(normal, "limit-other")
	if e == nil && derr == nil {
		_ = other.subscribe("room")
		e = directNumIs(m, []string{"room"}, 6, 5*time.Second)
	}
	rep.check("O-03", "连接数到上限后新连接被拒；断开一条后又能进；别的 Gateway 不受这台的上限影响", e)

	// 被动订阅：不收状态事件，业务消息照收。
	p, err1 := dialQuery(normal, "passive-1", "passive=st-room")
	q, err2 := dial(normal, "active-1")
	if !must("O-04", "客户端连接", firstErr(err1, err2)) {
		return
	}
	_ = p.subscribe("st-room")
	_ = q.subscribe("st-room")
	e = directNumIs(m, []string{"st-room"}, 2, 5*time.Second)
	stateTok := newTok()
	if e == nil {
		e = adminFrame(m, socket.StateKey, "st-room", []byte(`{"tok":"`+stateTok+`"}`))
	}
	if e == nil {
		e = settle(stateTok, "状态事件", []*client{q}, []*client{p}, 3*time.Second)
	}
	if e == nil {
		e = deliver(q, "st-room", []*client{p, q}, nil, 3*time.Second)
	}
	rep.check("O-04", "被动订阅的连接不收状态事件，业务消息照收；主动的两种都收", e)

	// 缺失提示：连接的发送缓冲写满、消息被丢时，打开了提示的连接会收到一帧 gap。
	hinted, err1 := dialQuery(normal, "gap-slow", "gap=1")
	healthy, err2 := dialQuery(normal, "gap-fast", "gap=1")
	plain, err3 := dial(normal, "nogap-slow")
	pub, err4 := dial(normal, "gap-pub")
	if !must("O-05", "客户端连接", firstErr(err1, err2, err3, err4)) {
		return
	}
	for _, cl := range []*client{hinted, healthy, plain} {
		_ = cl.subscribe("gap-room")
	}
	e = directNumIs(m, []string{"gap-room"}, 3, 5*time.Second)
	hinted.stall.Store(true)
	plain.stall.Store(true)
	sent := 0
	var toks []string
	stopAt := time.Now().Add(2500 * time.Millisecond)
	for time.Now().Before(stopAt) {
		tok := newTok()
		if pub.publishBig("gap-room", tok, 16000) == nil {
			sent++
			toks = append(toks, tok)
		}
		time.Sleep(3 * time.Millisecond)
	}
	hinted.stall.Store(false)
	plain.stall.Store(false)
	time.Sleep(3 * time.Second)
	count := func(cl *client) (got, gaps int) {
		cl.mu.Lock()
		defer cl.mu.Unlock()
		for _, t := range toks {
			if cl.got[t] > 0 {
				got++
			}
		}
		return got, cl.gaps
	}
	hg, hgaps := count(hinted)
	fg, fgaps := count(healthy)
	pg, pgaps := count(plain)
	note("发出 %d 条。卡住又恢复的连接（开提示）收到 %d 条、%d 帧提示；一直在读的连接收到 %d 条、%d 帧提示；卡住的连接（没开提示）收到 %d 条、%d 帧提示", sent, hg, hgaps, fg, fgaps, pg, pgaps)
	if e == nil {
		switch {
		case fg != sent:
			e = fmt.Errorf("一直在读的连接只收到 %d/%d", fg, sent)
		case fgaps != 0:
			e = fmt.Errorf("一直在读的连接没丢消息却收到了 %d 帧缺失提示", fgaps)
		case pgaps != 0:
			e = fmt.Errorf("没开提示的连接收到了 %d 帧缺失提示", pgaps)
		case hg == sent && hgaps == 0:
			e = fmt.Errorf("卡住的连接一条都没丢，这次没有测到丢弃")
		case hg < sent && hgaps == 0 && !hinted.dead.Load():
			e = fmt.Errorf("开了提示的连接丢了 %d 条却没有收到缺失提示", sent-hg)
		}
	}
	rep.check("O-05", "缺失提示：丢了消息且开了提示的连接收到 gap 帧；没丢的、没开的都不会收到", e)
	if hinted.dead.Load() {
		note("卡住的那条连接在恢复读取前已被服务端关闭（写超时），这种情况客户端靠重连补齐")
	}
	e = deliver(pub, "gap-room", []*client{healthy}, nil, 3*time.Second)
	if e == nil && strings.Contains(fmt.Sprint(e), "x") {
		e = nil
	}
	rep.check("O-06", "这一轮之后 Gateway 还在，正常连接照常收发", e)
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// stageConfig：配置写错或不一致时不能崩，要么照常工作，要么能从告警里看出来。
func stageConfig(keep bool) {
	c, err := newCluster("cfg")
	if !must("C-00", "建目录", err) {
		return
	}
	defer finish(c, "C-99", keep)
	mgrHTTP := fmt.Sprintf("127.0.0.1:%d", freePort())
	c.managerEnv = []string{"SC_MEMHTTP=" + mgrHTTP}
	mA, err := c.addManager("mA")
	c.managerEnv = nil
	if !must("C-00", "拉起 Manager A", err) {
		return
	}
	mB, err := c.addManager("mB")
	if !must("C-00", "拉起 Manager B", err) {
		return
	}
	// B 在配置里，但既不登记也不发布：等于停用。
	c.register["mB"] = false
	c.publish["mB"] = false
	// gw1 把 big 分 4 片，gw2 忘了配。
	c.groups["big"] = 4
	g1, err := c.addGateway(1)
	if !must("C-00", "拉起 Gateway 1", err) {
		return
	}
	delete(c.groups, "big")
	g2, err := c.addGateway(2)
	if !must("C-00", "拉起 Gateway 2", err) {
		return
	}
	var all []*client
	for _, g := range []*gw{g1, g2} {
		for k := 0; k < 3; k++ {
			cl, err := dial(g, fmt.Sprintf("u%d-%d", g.id, k))
			if !must("C-00", "客户端连接", err) {
				return
			}
			_ = cl.subscribe("room", "big")
			all = append(all, cl)
		}
	}
	e := directNumIs(mA, []string{"room"}, 6, 10*time.Second)
	if e == nil {
		e = directNumIs(mB, []string{"room"}, 0, 2*time.Second)
	}
	if e == nil {
		e = deliver(all[0], "room", all, nil, 3*time.Second)
	}
	if e == nil {
		e = deliver(all[4], "room", all, nil, 3*time.Second)
	}
	rep.check("C-01", "register=false、publish=false 的 Manager 收不到任何登记，也不经它发布；投递照常", e)
	mB.p.kill()
	e = deliver(all[1], "room", all, nil, 3*time.Second)
	rep.check("C-02", "停掉这台停用的 Manager 对投递没有任何影响", e)

	// 不写任何内存配置：Gateway 和 Manager 都按这台机器自动开启内存保护。
	want, source := socket.DetectMemoryLimit()
	gm, gerr := readGwMem(g1)
	e = gerr
	if e == nil && want > 0 && int64(gm.limit) != want {
		e = fmt.Errorf("Gateway 自动取到的上限 %dMB，这台机器应为 %dMB（%s）", gm.limit>>20, want>>20, source)
	}
	if e == nil {
		s, herr := httpGet(mgrHTTP, "/mem")
		f := strings.Fields(s)
		switch {
		case herr != nil:
			e = herr
		case len(f) < 3:
			e = fmt.Errorf("Manager 内存状态格式不对: %q", s)
		case want > 0 && f[2] != fmt.Sprint(want):
			e = fmt.Errorf("Manager 自动取到的上限 %s，这台机器应为 %d（%s）", f[2], want, source)
		}
	}
	rep.check("C-02b", "不写内存配置时，Gateway 和 Manager 都按机器自动开启内存保护", e)
	note("这台机器自动取到的上限 %dMB（来自 %s）", want>>20, source)

	// 分组配置不一致：不崩，Manager 打告警。
	for _, cl := range all {
		_ = cl.publish("big", newTok())
	}
	time.Sleep(time.Second)
	e = nil
	if !g1.p.alive() || !g2.p.alive() || !mA.p.alive() {
		e = fmt.Errorf("有进程退出了")
	} else if countInFile(mA.p.log, "registered both whole and in shards") == 0 {
		e = fmt.Errorf("两台 Gateway 的分组配置不一致，Manager 没有告警")
	}
	rep.check("C-03", "各 Gateway 的分组配置不一致：进程不崩，Manager 打出告警", e)

	// 把 gw2 的配置改对，滚动重启后整个房间恢复互通。
	c.groups["big"] = 4
	e = roll(c, g2, all)
	if e == nil {
		e = numIs(g1, "big", 6, 10*time.Second)
	}
	if e == nil {
		e = deliver(all[0], "big", all, nil, 3*time.Second)
	}
	if e == nil {
		e = deliver(all[5], "big", all, nil, 3*time.Second)
	}
	if e == nil {
		e = backendDeliver(mA, "big", all, nil)
	}
	rep.check("C-04", "把配置改一致并滚动重启后，分组房间全员互通，人数正确", e)
}
