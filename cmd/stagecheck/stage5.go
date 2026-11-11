package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/OSMeteor/firetower/grpc/manager"
)

// ---------- 网络故障 ----------

// stageNet：Gateway 和 Manager 之间的网络出问题，但两边进程都活着。
// 和杀进程不同，对端收不到断开通知，只能靠超时发现。
func stageNet(keep bool) {
	c, err := newCluster("net")
	if !must("N-00", "建目录", err) {
		return
	}
	defer finish(c, "N-99", keep)
	m, err := c.addManagerBehindLinks("m1")
	if !must("N-00", "拉起 Manager 和转发", err) {
		return
	}
	const n, per = 3, 3
	for i := 1; i <= n; i++ {
		if _, err := c.addGateway(i); !must("N-00", fmt.Sprintf("拉起 Gateway %d", i), err) {
			return
		}
	}
	gs := c.gateways
	var all []*client
	for _, g := range gs {
		for k := 0; k < per; k++ {
			cl, err := dial(g, fmt.Sprintf("u%d-%d", g.id, k))
			if !must("N-00", "客户端连接", err) {
				return
			}
			_ = cl.subscribe("room-all", fmt.Sprintf("room-g%d", g.id))
			all = append(all, cl)
		}
	}
	total := n * per
	e := directNumIs(m, []string{"room-all"}, total, 10*time.Second)
	if e == nil {
		e = deliver(all[0], "room-all", all, nil, 3*time.Second)
	}
	rep.check("N-01", "经过转发（等同于中间有地址转换）时，握手键生效，投递和人数正确", e)

	// 短暂卡顿：连接没断，数据晚到。
	m.partition()
	tok := newTok()
	_ = all[1].publish("room-all", tok)
	time.Sleep(3 * time.Second)
	m.heal()
	e = settle(tok, "卡顿期间发布的消息", all, nil, 8*time.Second)
	if e == nil {
		e = deliver(all[2], "room-all", all, nil, 3*time.Second)
	}
	if e == nil {
		e = directNumIs(m, []string{"room-all"}, total, 5*time.Second)
	}
	rep.check("N-02", "网络卡住 3 秒再恢复：卡住期间发布的消息晚到但恰好一次，之后一切照常", e)

	// 握手确认来得晚。TCP 口来回各加 700ms，确认超过 Gateway 等待的 500ms。
	m.viaTCP.setDelay(700 * time.Millisecond)
	m.viaTCP.reset()
	// 一条消息来回要 1.4 秒以上，每次多等一会儿。
	begin := time.Now()
	tries := 0
	var e2 error
	for time.Since(begin) < 40*time.Second {
		tries++
		if e2 = deliver(all[0], "room-all", all, nil, 5*time.Second); e2 == nil {
			break
		}
	}
	took, e := time.Since(begin), e2
	rep.check("N-03", "握手确认超过 500ms 才到（高延迟链路上重连）：Gateway 最终仍能收到消息", e)
	note("从掐断连接到全部收到: %s（发布 %d 次）", took.Round(10*time.Millisecond), tries)
	e = directNumIs(m, []string{"room-all"}, total, 15*time.Second)
	for _, g := range gs {
		if err := directNumIs(m, []string{fmt.Sprintf("room-g%d", g.id)}, per, 10*time.Second); err != nil && e == nil {
			e = err
		}
	}
	rep.check("N-04", "确认晚到之后人数精确，没有留下按地址登记的重复条目", e)
	m.viaTCP.setDelay(0)
	m.viaTCP.reset()
	_, _, e = deliverUntil(all[0], "room-all", all, 20*time.Second)
	if e == nil {
		e = directNumIs(m, []string{"room-all"}, total, 10*time.Second)
	}
	rep.check("N-05", "延迟去掉、连接再掐一次之后恢复正常", e)

	// 后端猛发，同时 Manager 到 Gateway 的链路卡住：Manager 的发送队列会写满。
	m.viaTCP.stall()
	cl, done, err := rpcClient(m)
	sent := 0
	if err == nil {
		for i := 0; i < 20000; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			if _, perr := cl.Publish(ctx, &pb.PublishRequest{Topic: "room-all", Data: []byte(`{"flood":1}`), MessageId: "f", Source: "backend"}); perr == nil {
				sent++
			}
			cancel()
		}
		done()
	}
	e = err
	if e == nil {
		if _, nerr := directNum(m, "room-all"); nerr != nil {
			e = fmt.Errorf("发送队列写满时 Manager 不响应: %v", nerr)
		}
	}
	if e == nil && !m.p.alive() {
		e = fmt.Errorf("Manager 进程退出了")
	}
	m.viaTCP.resume()
	if e == nil {
		_, _, e = deliverUntil(all[0], "room-all", all, 30*time.Second)
	}
	if e == nil {
		e = directNumIs(m, []string{"room-all"}, total, 15*time.Second)
	}
	rep.check("N-06", "链路卡住时后端连发 2 万条把 Manager 的发送队列写满：Manager 不卡不崩，链路恢复后投递和人数正常", e)
	note("卡住期间 Manager 接受了 %d 条发布", sent)

	// 长时间不通，超过 Gateway 的 30 秒读超时。
	m.partition()
	gone := all[len(all)-1]
	gone.close()
	ghost, _ := dial(gs[0], "ghost")
	if ghost != nil {
		_ = ghost.subscribe("room-all")
	}
	for i := 0; i < 5; i++ {
		_ = all[0].publish("room-all", newTok())
	}
	e = nil
	for _, g := range gs {
		if _, herr := httpGet(g.http, "/goroutines"); herr != nil {
			e = fmt.Errorf("网络不通期间 gw%d 不响应: %v", g.id, herr)
		}
	}
	rep.check("N-07", "网络不通期间 Gateway 仍然响应，客户端的订阅、发布、断开不卡住", e)
	time.Sleep(40 * time.Second)
	if ghost != nil {
		ghost.close()
	}
	m.heal()
	rest := all[:len(all)-1]
	took, tries, e = deliverUntil(rest[0], "room-all", rest, 60*time.Second)
	rep.check("N-08", "网络不通 40 秒（超过 30 秒读超时）再恢复：原连接不重订就恢复", e)
	note("从网络恢复到全部收到: %s（发布 %d 次）", took.Round(10*time.Millisecond), tries)
	e = directNumIs(m, []string{"room-all"}, total-1, 20*time.Second)
	if e == nil {
		e = deliver(rest[1], "room-all", rest, []*client{gone}, 3*time.Second)
	}
	rep.check("N-09", "恢复后人数精确：不通期间断开的连接被减掉，不通期间失败的订阅没有留下", e)
}

// ---------- 慢消费者和热点房间 ----------

func stageLoad(keep bool) {
	c, err := newCluster("load")
	if !must("L-00", "建目录", err) {
		return
	}
	defer finish(c, "L-99", keep)
	m, err := c.addManager("m1")
	if !must("L-00", "拉起 Manager", err) {
		return
	}
	const n = 3
	for i := 1; i <= n; i++ {
		if _, err := c.addGateway(i); !must("L-00", fmt.Sprintf("拉起 Gateway %d", i), err) {
			return
		}
	}
	gs := c.gateways
	var hot, slow, quiet []*client
	for _, g := range gs {
		for k := 0; k < 10; k++ {
			cl, err := dial(g, fmt.Sprintf("hot%d-%d", g.id, k))
			if !must("L-00", "客户端连接", err) {
				return
			}
			_ = cl.subscribe("hot")
			if k == 0 {
				slow = append(slow, cl)
			} else {
				hot = append(hot, cl)
			}
		}
		for k := 0; k < 2; k++ {
			cl, err := dial(g, fmt.Sprintf("quiet%d-%d", g.id, k))
			if !must("L-00", "客户端连接", err) {
				return
			}
			_ = cl.subscribe("quiet")
			quiet = append(quiet, cl)
		}
	}
	e := directNumIs(m, []string{"hot"}, 30, 10*time.Second)
	if e == nil {
		e = directNumIs(m, []string{"quiet"}, 6, 5*time.Second)
	}
	if !rep.check("L-01", "30 个连接在热点房间，6 个连接在安静房间", e) {
		return
	}
	for _, cl := range slow {
		cl.stall.Store(true) // 10% 的连接停止读取
	}

	const seconds, perSecond = 6, 900
	var hotToks sync.Map
	var sentHot atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i, g := range gs {
		pub := onGW(hot, g)[0]
		wg.Add(1)
		go func(i int, pub *client) {
			defer wg.Done()
			tick := time.NewTicker(time.Second / time.Duration(perSecond/n))
			defer tick.Stop()
			for {
				select {
				case <-stop:
					return
				case <-tick.C:
					tok := newTok()
					hotToks.Store(tok, struct{}{})
					if pub.publishBig("hot", tok, 2000) == nil {
						sentHot.Add(1)
					}
				}
			}
		}(i, pub)
	}
	var worst time.Duration
	var quietErr error
	probes := 0
	end := time.Now().Add(seconds * time.Second)
	for time.Now().Before(end) {
		start := time.Now()
		tok := newTok()
		if err := quiet[probes%len(quiet)].publish("quiet", tok); err != nil {
			quietErr = err
			break
		}
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			ok := true
			for _, q := range quiet {
				if q.count(tok) < 1 {
					ok = false
					break
				}
			}
			if ok {
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		lat := time.Since(start)
		for _, q := range quiet {
			if k := q.count(tok); k != 1 && quietErr == nil {
				quietErr = fmt.Errorf("热点发布期间安静房间的一条消息在 %s 上收到 %d 次", q.name(), k)
			}
		}
		if lat > worst {
			worst = lat
		}
		probes++
		time.Sleep(100 * time.Millisecond)
	}
	close(stop)
	wg.Wait()
	time.Sleep(2 * time.Second)
	if quietErr == nil && worst > time.Second {
		quietErr = fmt.Errorf("安静房间最慢一次用了 %s", worst.Round(time.Millisecond))
	}
	rep.check("L-02", "热点房间每秒 900 条（每条约 2KB）、10% 连接不读的同时，安静房间每条都恰好一次且不超过 1 秒", quietErr)
	note("安静房间探测 %d 次，最慢 %s；热点共发出 %d 条", probes, worst.Round(time.Millisecond), sentHot.Load())

	// 串内容：没订热点的连接一条热点消息都不能收到；订了的不能重复。
	var leaked, dups int
	var got, want int64
	sent := sentHot.Load()
	for _, q := range quiet {
		q.mu.Lock()
		for tok := range q.got {
			if _, isHot := hotToks.Load(tok); isHot {
				leaked++
			}
		}
		q.mu.Unlock()
	}
	for _, h := range hot {
		h.mu.Lock()
		for tok, k := range h.got {
			if _, isHot := hotToks.Load(tok); !isHot {
				continue
			}
			got++
			if k > 1 {
				dups++
			}
		}
		h.mu.Unlock()
		want += sent
	}
	e = nil
	if leaked > 0 {
		e = fmt.Errorf("没订热点房间的连接收到了 %d 条热点消息", leaked)
	} else if dups > 0 {
		e = fmt.Errorf("热点房间有 %d 条消息被重复投递", dups)
	}
	rep.check("L-03", "没有串内容，也没有重复投递", e)
	ratio := 100 * float64(got) / float64(want)
	e = nil
	if ratio < 99.5 {
		e = fmt.Errorf("正常读取的 27 个连接只收到 %.2f%%", ratio)
	}
	rep.check("L-04", "慢连接不拖累别人：正常读取的连接收到 99.5% 以上", e)
	note("正常连接应收 %d 条，实收 %d 条（%.2f%%）", want, got, ratio)

	for _, cl := range slow {
		cl.stall.Store(false)
	}
	time.Sleep(time.Second)
	live := append([]*client{}, hot...)
	for _, cl := range slow {
		if !cl.dead.Load() {
			live = append(live, cl)
		}
	}
	e = deliver(hot[0], "hot", live, quiet, 5*time.Second)
	if e == nil {
		e = deliver(quiet[0], "quiet", quiet, hot, 3*time.Second)
	}
	if e == nil {
		e = directNumIs(m, []string{"hot"}, len(live), 10*time.Second)
	}
	rep.check("L-05", "压力结束、慢连接恢复读取后，投递和人数都正确", e)
	note("3 个慢连接里有 %d 个被服务端关掉", 30-len(live))
}

// ---------- 定期对账 ----------

func stageReconcile(keep bool) {
	c, err := newCluster("rec")
	if !must("R-00", "建目录", err) {
		return
	}
	defer finish(c, "R-99", keep)
	m, err := c.addManager("m1")
	if !must("R-00", "拉起 Manager", err) {
		return
	}
	for i := 1; i <= 2; i++ {
		if _, err := c.addGateway(i); !must("R-00", fmt.Sprintf("拉起 Gateway %d", i), err) {
			return
		}
	}
	gs := c.gateways
	started := time.Now()
	var all []*client
	for _, g := range gs {
		for k := 0; k < 2; k++ {
			cl, err := dial(g, fmt.Sprintf("u%d-%d", g.id, k))
			if !must("R-00", "客户端连接", err) {
				return
			}
			_ = cl.subscribe("room-a", "room-b")
			all = append(all, cl)
		}
	}
	e := directNumIs(m, []string{"room-a", "room-b"}, 4, 10*time.Second)
	key, kerr := httpGet(gs[0].http, "/key")
	if e == nil && (kerr != nil || key == "") {
		e = fmt.Errorf("读不到 gw1 的路由键: %v", kerr)
	}
	if !rep.check("R-01", "基线", e) {
		return
	}
	// 在 Manager 上人为制造偏差。用不带版本号的旧式请求，这样不会挡住之后的对账。
	cl, done, err := rpcClient(m)
	if !must("R-02", "连 Manager", err) {
		return
	}
	defer done()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	for i := 0; i < 3; i++ {
		_, _ = cl.SubscribeTopic(ctx, &pb.SubscribeTopicRequest{Ip: key, Topic: []string{"room-a", "room-ghost"}})
	}
	for i := 0; i < 2; i++ {
		_, _ = cl.UnSubscribeTopic(ctx, &pb.UnSubscribeTopicRequest{Ip: key, Topic: []string{"room-b"}})
	}
	cancel()
	na, _ := directNum(m, "room-a")
	ng, _ := directNum(m, "room-ghost")
	lost := deliver(all[2], "room-b", all, nil, 2*time.Second)
	e = nil
	if na != 7 || ng != 3 || lost == nil {
		e = fmt.Errorf("偏差没有造出来: room-a=%d room-ghost=%d room-b 丢失=%v", na, ng, lost != nil)
	}
	if !rep.check("R-02", "造出三种偏差：人数多 3、多出一个没人的房间、gw1 在 room-b 上的路由被删", e) {
		return
	}
	// 等定期对账。间隔 3 分钟，从 Gateway 启动算起。
	e = directNumIs(m, []string{"room-a"}, 4, 200*time.Second)
	if e == nil {
		e = directNumIs(m, []string{"room-ghost"}, 0, 10*time.Second)
	}
	if e == nil {
		e = deliver(all[2], "room-b", all, nil, 3*time.Second)
	}
	rep.check("R-03", "不重启、不重连，定期对账把三种偏差全部纠正", e)
	note("对账在 Gateway 启动后 %s 内完成纠正", time.Since(started).Round(time.Second))
}
