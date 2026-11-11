package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// 阶段 2：N 台 Gateway + 1 台 Manager。
func stage2(n int, keep bool) {
	c, err := newCluster("s2")
	if !must("S2-00", "建目录", err) {
		return
	}
	defer finish(c, "S2-99", keep)
	if _, err := c.addManager("m1"); !must("S2-00", "拉起 Manager", err) {
		return
	}
	for i := 1; i <= n; i++ {
		if _, err := c.addGateway(i); !must("S2-00", fmt.Sprintf("拉起 Gateway %d", i), err) {
			return
		}
	}
	const per = 3
	var all []*client
	for _, g := range c.gateways {
		for k := 0; k < per; k++ {
			cl, err := dial(g, fmt.Sprintf("u%d-%d", g.id, k))
			if !must("S2-00", "客户端连接", err) {
				return
			}
			_ = cl.subscribe("room-all", fmt.Sprintf("room-g%d", g.id))
			all = append(all, cl)
		}
	}
	gs := c.gateways
	first, last := gs[0], gs[len(gs)-1]

	rep.check("S2-01", fmt.Sprintf("%d 台 Gateway 读到的 room-all 人数都是 %d", n, n*per), numEverywhere(gs, "room-all", n*per, 10*time.Second))
	var e error
	for _, g := range gs {
		if err := deliver(onGW(all, g)[0], "room-all", all, nil, 3*time.Second); err != nil && e == nil {
			e = err
		}
	}
	rep.check("S2-02", "每台 Gateway 发布的消息，所有 Gateway 上的订阅者都恰好收到一次", e)

	e = nil
	for _, g := range gs {
		topic := fmt.Sprintf("room-g%d", g.id)
		other := notOnGW(all, g)
		if err := deliver(other[0], topic, onGW(all, g), other, 3*time.Second); err != nil && e == nil {
			e = err
		}
	}
	rep.check("S2-03", "只在一台 Gateway 上有人订阅的房间，别的 Gateway 上的连接收不到", e)

	// 大消息：正文从几 KB 到接近单帧上限，跨 Gateway 必须原样到达。
	e = nil
	for _, pad := range []int{4000, 4100, 16 * 1024, 64 * 1024, 900 * 1024} {
		tok := newTok()
		if err := onGW(all, last)[0].publishBig("room-all", tok, pad); err != nil {
			e = err
			break
		}
		if err := settle(tok, fmt.Sprintf("正文约 %d 字节的消息", pad), all, nil, 5*time.Second); err != nil {
			e = err
			break
		}
		for _, cl := range all {
			cl.mu.Lock()
			size := cl.lastSize
			cl.mu.Unlock()
			if size < pad {
				e = fmt.Errorf("正文约 %d 字节的消息到 %s 时只剩 %d 字节", pad, cl.name(), size)
				break
			}
		}
		if e != nil {
			break
		}
	}
	rep.check("S2-03b", "4KB 到 900KB 的消息跨 Gateway 原样送达，没有被截断", e)

	// 退订和断开之后人数必须精确回落。
	quitter := onGW(all, first)[0]
	_ = quitter.unsubscribe("room-all")
	e = numEverywhere(gs, "room-all", n*per-1, 5*time.Second)
	if e == nil {
		e = deliver(onGW(all, last)[0], "room-all", filter(all, func(x *client) bool { return x != quitter }), []*client{quitter}, 3*time.Second)
	}
	rep.check("S2-04", "退订后人数减一，退订的连接不再收到", e)
	leaver := onGW(all, first)[1]
	leaver.close()
	rep.check("S2-05", "客户端断开后人数再减一", numEverywhere(gs, "room-all", n*per-2, 5*time.Second))
	_ = quitter.subscribe("room-all")
	_ = leaver.reconnect()
	rep.check("S2-06", "重新订阅、断线重连后人数回到原值", numEverywhere(gs, "room-all", n*per, 5*time.Second))

	// 业务回调 panic：进程不能退，这条连接要被关掉，别的连接不受影响。
	e = nil
	for _, bug := range []string{"connect", "gorun", "goclose", "offline", "read", "sub", "unsub"} {
		bad, err := dialQuery(first, "bug-"+bug, "bug="+bug)
		if err != nil {
			e = fmt.Errorf("bug=%s 连接失败: %v", bug, err)
			break
		}
		_ = bad.subscribe("room-bug")
		_ = bad.publish("room-bug", newTok())
		_ = bad.unsubscribe("room-bug")
		time.Sleep(400 * time.Millisecond)
		bad.close()
		if !first.p.alive() {
			e = fmt.Errorf("bug=%s 让 Gateway 进程退出了", bug)
			break
		}
	}
	if e == nil {
		e = deliver(onGW(all, first)[2], "room-all", all, nil, 3*time.Second)
	}
	if e == nil {
		e = numEverywhere(gs, "room-bug", 0, 5*time.Second)
	}
	if e == nil {
		e = numEverywhere(gs, "room-all", n*per, 5*time.Second)
	}
	rep.check("S2-07", "7 种业务回调 panic 之后 Gateway 还活着，其余连接照常收发，人数没被带歪", e)

	// 硬杀一台 Gateway。
	last.p.kill()
	rest := notOnGW(all, last)
	live := gs[:len(gs)-1]
	e = numEverywhere(live, "room-all", (n-1)*per, 10*time.Second)
	if e == nil {
		e = numEverywhere(live, fmt.Sprintf("room-g%d", last.id), 0, 5*time.Second)
	}
	rep.check("S2-08", "硬杀一台 Gateway 后，Manager 把它的订阅清干净，人数精确回落", e)
	rep.check("S2-09", "其余 Gateway 之间照常互通，恰好一次", deliver(rest[0], "room-all", rest, nil, 3*time.Second))

	// 同一个 ClusterId 重新拉起，客户端重连重订。
	e = c.startGateway(last)
	if e == nil {
		for _, cl := range onGW(all, last) {
			if err := cl.reconnect(); err != nil {
				e = err
			}
		}
	}
	if e == nil {
		e = numEverywhere(gs, "room-all", n*per, 10*time.Second)
	}
	if e == nil {
		e = deliver(onGW(all, last)[0], "room-all", all, nil, 3*time.Second)
	}
	rep.check("S2-10", "被杀的 Gateway 用同一个 ClusterId 拉起，客户端重连后人数和投递都恢复，没有残留", e)

	// 扩容一台，再排空。
	extra, err := c.addGateway(n + 1)
	var newcomers []*client
	if err == nil {
		for k := 0; k < per; k++ {
			cl, derr := dial(extra, fmt.Sprintf("u%d-%d", extra.id, k))
			if derr != nil {
				err = derr
				break
			}
			_ = cl.subscribe("room-all")
			newcomers = append(newcomers, cl)
		}
	}
	if err == nil {
		err = numEverywhere(c.gateways, "room-all", (n+1)*per, 10*time.Second)
	}
	if err == nil {
		err = deliver(newcomers[0], "room-all", append(append([]*client{}, all...), newcomers...), nil, 3*time.Second)
	}
	if err == nil {
		err = deliver(all[0], "room-all", append(append([]*client{}, all...), newcomers...), nil, 3*time.Second)
	}
	rep.check("S2-11", "加一台 Gateway，新老连接互通，人数加上新连接", err)

	if extra != nil && extra.p.alive() {
		_, err = httpGet(extra.http, "/drain")
		if err == nil {
			err = numEverywhere(gs, "room-all", n*per, 10*time.Second)
		}
		if err == nil {
			late, derr := dial(extra, "late")
			if derr == nil {
				_ = late.subscribe("room-all")
				time.Sleep(500 * time.Millisecond)
				if !late.dead.Load() {
					err = fmt.Errorf("排空之后新连接没有被拒绝")
				}
				if nerr := numEverywhere(gs, "room-all", n*per, 3*time.Second); nerr != nil && err == nil {
					err = nerr
				}
			}
		}
		if err == nil {
			err = deliver(all[0], "room-all", all, newcomers, 3*time.Second)
		}
		rep.check("S2-12", "Drain 排空一台：它的连接被关、人数回落、新连接被拒，其余不受影响", err)
		extra.p.kill()
	}

	// 配置写错：再起一台 Gateway，ClusterId 和 gw1 相同。不能崩，两台都要能收发，Manager 要告警。
	twin := &gw{id: 1, http: fmt.Sprintf("127.0.0.1:%d", freePort())}
	twinDir := filepath.Join(c.root, "gw1-twin")
	e = os.MkdirAll(twinDir, 0o755)
	if e == nil {
		twin.p = &proc{name: "gw1-twin", dir: twinDir, log: filepath.Join(twinDir, "out.log"),
			env: []string{"SC_ROLE=gateway", "SC_CLUSTER=1", "SC_HTTP=" + twin.http}}
		c.gateways = append(c.gateways, twin)
		e = c.startGateway(twin)
	}
	var twinClient *client
	if e == nil {
		twinClient, e = dial(twin, "twin-0")
	}
	if e == nil {
		_ = twinClient.subscribe("room-all")
		e = numEverywhere(gs, "room-all", n*per+1, 10*time.Second)
	}
	if e == nil {
		e = deliver(twinClient, "room-all", append(append([]*client{}, all...), twinClient), nil, 3*time.Second)
	}
	if e == nil {
		e = deliver(onGW(all, first)[0], "room-all", append(append([]*client{}, all...), twinClient), nil, 3*time.Second)
	}
	if e == nil && countInFile(c.managers[0].p.log, "reported startup") == 0 {
		e = fmt.Errorf("两台 Gateway 用了同一个 ClusterId，Manager 没有告警")
	}
	rep.check("S2-12b", "两台 Gateway 误用同一个 ClusterId：不崩，两台都能收发，Manager 打出告警", e)
	if twinClient != nil {
		twinClient.close()
	}
	twin.p.kill()
	rep.check("S2-12c", "那台多余的 Gateway 下线后人数回到原值", numEverywhere(gs, "room-all", n*per, 10*time.Second))

	// Manager 故障和恢复。
	m := c.managers[0]
	m.p.kill()
	downStart := time.Now()
	// 故障期间发布、订阅、断开都不能把 Gateway 卡住或搞崩。
	for _, cl := range all {
		_ = cl.publish("room-all", newTok())
	}
	ghost, _ := dial(first, "ghost")
	if ghost != nil {
		_ = ghost.subscribe("room-all")
		time.Sleep(300 * time.Millisecond)
		ghost.close()
	}
	e = nil
	for _, g := range gs {
		if _, err := httpGet(g.http, "/goroutines"); err != nil {
			e = fmt.Errorf("Manager 故障期间 gw%d 不响应: %v", g.id, err)
		}
	}
	rep.check("S2-13", "Manager 被杀期间，各 Gateway 仍然响应，发布、订阅、断开不卡住", e)
	time.Sleep(time.Until(downStart.Add(3 * time.Second)))
	e = c.startManager(m)
	var took time.Duration
	var tries int
	if e == nil {
		took, tries, e = deliverUntil(all[0], "room-all", all, 30*time.Second)
	}
	rep.check("S2-14", "Manager 拉起后，原有连接不重订就恢复跨 Gateway 投递", e)
	note("从 Manager 拉起到全部订阅者收到: %s（发布 %d 次）", took.Round(10*time.Millisecond), tries)
	e = numEverywhere(gs, "room-all", n*per, 10*time.Second)
	if e == nil {
		for _, g := range gs {
			if err := numIs(first, fmt.Sprintf("room-g%d", g.id), per, 5*time.Second); err != nil {
				e = err
			}
		}
	}
	rep.check("S2-15", "重放之后人数和故障前完全一致（故障期间失败的订阅没有留下残留）", e)
	e = nil
	for _, g := range gs {
		if err := deliver(onGW(all, g)[0], "room-all", all, nil, 3*time.Second); err != nil && e == nil {
			e = err
		}
	}
	rep.check("S2-16", "恢复之后每台 Gateway 发布仍是恰好一次，没有重复路由", e)

	// 抖动：连接不停进出，同时发布，中途再重启一次 Manager。结束后人数必须精确。
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for _, g := range gs {
		wg.Add(1)
		go func(g *gw) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				cl, err := dial(g, fmt.Sprintf("churn%d-%d", g.id, i))
				if err != nil {
					time.Sleep(50 * time.Millisecond)
					continue
				}
				_ = cl.subscribe("room-all", "room-churn")
				_ = cl.publish("room-churn", newTok())
				if i%3 == 0 {
					_ = cl.unsubscribe("room-churn")
				}
				if i%2 == 0 {
					time.Sleep(15 * time.Millisecond)
				}
				cl.close()
			}
		}(g)
	}
	time.Sleep(4 * time.Second)
	m.p.kill()
	time.Sleep(1500 * time.Millisecond)
	e = c.startManager(m)
	time.Sleep(4 * time.Second)
	close(stop)
	wg.Wait()
	if e == nil {
		_, _, e = deliverUntil(all[0], "room-all", all, 30*time.Second)
	}
	rep.check("S2-17", "连接频繁进出并中途重启 Manager 之后，原有连接仍能互通", e)
	e = numEverywhere(gs, "room-all", n*per, 15*time.Second)
	e2 := numEverywhere(gs, "room-churn", 0, 15*time.Second)
	if e == nil {
		e = e2
	}
	if e != nil {
		note("人数没有立刻对上: %v。等一轮 3 分钟对账再看。", e)
		wait := time.Now()
		e = numEverywhere(gs, "room-all", n*per, reconcileWait)
		if e == nil {
			e = numEverywhere(gs, "room-churn", 0, 30*time.Second)
		}
		if e == nil {
			e = fmt.Errorf("人数在抖动后多出来，等了 %s 的对账才回到正确值", time.Since(wait).Round(time.Second))
		}
	}
	rep.check("S2-18", "抖动结束后人数立刻精确：room-all 回到原值，room-churn 归零", e)

	base := goroutines(first)
	for _, cl := range all {
		cl.close()
	}
	e = numEverywhere(gs, "room-all", 0, 10*time.Second)
	time.Sleep(time.Second)
	after := goroutines(first)
	note("gw%d 协程数: 有连接时 %d，全部断开后 %d", first.id, base, after)
	if e == nil && after > 60 {
		e = fmt.Errorf("全部断开后 gw%d 还有 %d 个协程", first.id, after)
	}
	rep.check("S2-19", "全部客户端断开后人数归零，协程回落", e)
}

// stage2Big：约 20 台 Gateway 时重启 Manager，记录恢复耗时（必测五项第 1 项）。
func stage2Big(n int, keep bool) {
	c, err := newCluster("s2big")
	if !must("S2B-00", "建目录", err) {
		return
	}
	defer finish(c, "S2B-99", keep)
	m, err := c.addManager("m1")
	if !must("S2B-00", "拉起 Manager", err) {
		return
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var startErr error
	gs := make([]*gw, 0, n)
	for i := 1; i <= n; i++ {
		ng, err := c.addGatewayAsync(i)
		if err != nil {
			startErr = err
			break
		}
		gs = append(gs, ng)
	}
	for _, g := range gs {
		wg.Add(1)
		go func(g *gw) {
			defer wg.Done()
			if err := c.waitGateway(g, 1); err != nil {
				mu.Lock()
				startErr = err
				mu.Unlock()
			}
		}(g)
	}
	wg.Wait()
	if !must("S2B-00", fmt.Sprintf("拉起 %d 台 Gateway", n), startErr) {
		return
	}
	const per, topics = 4, 50
	var all []*client
	names := make([]string, topics)
	for i := range names {
		names[i] = fmt.Sprintf("t%d", i)
	}
	for _, g := range gs {
		for k := 0; k < per; k++ {
			cl, err := dial(g, fmt.Sprintf("u%d-%d", g.id, k))
			if !must("S2B-00", "客户端连接", err) {
				return
			}
			_ = cl.subscribe(names...)
			_ = cl.subscribe("own-" + cl.uid)
			all = append(all, cl)
		}
	}
	// 每台 Gateway 再挂一个订了 600 个独有 Topic 的连接。重放按 256 个一块发，这样每台都要发 3 块以上，
	// 20 台同时重放时会超过 Manager 的登记并发上限（8），后来的要排队。
	const bulk = 600
	var bulkClients []*client
	for _, g := range gs {
		cl, err := dial(g, fmt.Sprintf("bulk%d", g.id))
		if !must("S2B-00", "客户端连接", err) {
			return
		}
		for from := 0; from < bulk; from += 200 {
			part := make([]string, 0, 200)
			for i := from; i < from+200; i++ {
				part = append(part, fmt.Sprintf("bulk-%d-%d", g.id, i))
			}
			_ = cl.subscribe(part...)
		}
		bulkClients = append(bulkClients, cl)
	}
	bulkCheck := func(d time.Duration) error {
		for _, g := range gs {
			for _, i := range []int{0, 199, 255, 256, 257, 511, 512, 599} {
				if err := directNumIs(m, []string{fmt.Sprintf("bulk-%d-%d", g.id, i)}, 1, d); err != nil {
					return err
				}
			}
		}
		return nil
	}
	e := numEverywhere(gs[:1], "t0", n*per, 20*time.Second)
	if e == nil {
		e = numEverywhere(gs[:1], names[topics-1], n*per, 20*time.Second)
	}
	if e == nil {
		e = bulkCheck(20 * time.Second)
	}
	if e == nil {
		e = deliver(all[0], "t7", all, nil, 5*time.Second)
	}
	rep.check("S2B-01", fmt.Sprintf("%d 台 Gateway、%d 个连接、每个连接 %d 个 Topic，另有每台 600 个独有 Topic：人数和投递正确", n, len(all), topics+1), e)

	m.p.kill()
	time.Sleep(2 * time.Second)
	e = c.startManager(m)
	up := time.Now()
	var took time.Duration
	if e == nil {
		took, _, e = deliverUntil(all[len(all)-1], "t3", all, 60*time.Second)
	}
	rep.check("S2B-02", "重启 Manager 后全部连接不重订就恢复", e)
	note("从 Manager 拉起到 %d 个连接全部收到: %s", len(all), took.Round(10*time.Millisecond))
	e = nil
	for i := 0; i < topics; i += 7 {
		if err := numIs(gs[i%len(gs)], names[i], n*per, 20*time.Second); err != nil && e == nil {
			e = err
		}
	}
	for i := 0; i < len(all); i += 9 {
		if err := numIs(gs[0], "own-"+all[i].uid, 1, 10*time.Second); err != nil && e == nil {
			e = err
		}
	}
	if e == nil {
		e = bulkCheck(30 * time.Second)
	}
	if e == nil {
		// 每台挑一个靠后的 Topic 实际发一条，确认路由真的在。
		for i, cl := range bulkClients {
			if err := deliver(all[(i*7)%len(all)], fmt.Sprintf("bulk-%d-%d", cl.g.id, 577), []*client{cl}, nil, 5*time.Second); err != nil {
				e = err
				break
			}
		}
	}
	rep.check("S2B-03", "重放后抽查的 Topic 人数全部精确，包括每台 600 个 Topic 的分块边界", e)
	note("从 Manager 拉起到抽查人数全部对上: %s", time.Since(up).Round(10*time.Millisecond))
	e = nil
	for i := 0; i < topics; i += 5 {
		if err := deliver(all[(i*3)%len(all)], names[i], all, nil, 5*time.Second); err != nil && e == nil {
			e = err
		}
	}
	rep.check("S2B-04", "重放后各 Topic 仍是恰好一次", e)
}
