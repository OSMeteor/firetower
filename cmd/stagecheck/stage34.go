package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	pb "github.com/OSMeteor/firetower/grpc/manager"
	"github.com/OSMeteor/firetower/socket"
	"google.golang.org/grpc"
)

// ---------- 直接访问 Manager ----------

func rpcClient(m *mgr) (pb.TopicServiceClient, func(), error) {
	conn, err := grpc.Dial(fmt.Sprintf("127.0.0.1:%d", m.grpc), grpc.WithInsecure())
	if err != nil {
		return nil, nil, err
	}
	return pb.NewTopicServiceClient(conn), func() { _ = conn.Close() }, nil
}

func directNum(m *mgr, topic string) (int, error) {
	cl, done, err := rpcClient(m)
	if err != nil {
		return -1, err
	}
	defer done()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	res, err := cl.GetConnectNum(ctx, &pb.GetConnectNumRequest{Topic: topic})
	if err != nil {
		return -1, err
	}
	return int(res.Number), nil
}

// directNumIs 直接问某一台 Manager。用来确认这一台自己有全量路由，而不是靠别的 Manager 兜着。
func directNumIs(m *mgr, topics []string, want int, d time.Duration) error {
	deadline := time.Now().Add(d)
	for {
		var bad string
		for _, t := range topics {
			n, err := directNum(m, t)
			if err != nil {
				bad = fmt.Sprintf("%s 读 %q 失败: %v", m.name, t, err)
				break
			}
			if n != want {
				bad = fmt.Sprintf("%s 上 %q 人数 %d，应为 %d", m.name, t, n, want)
				break
			}
		}
		if bad == "" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s", bad)
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// backendPublish 模拟业务后端经 gRPC 向某一台 Manager 发布。
func backendPublish(m *mgr, topic, tok string) error {
	cl, done, err := rpcClient(m)
	if err != nil {
		return err
	}
	defer done()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = cl.Publish(ctx, &pb.PublishRequest{Topic: topic, Data: []byte(`{"tok":"` + tok + `"}`), MessageId: tok, Source: "backend"})
	return err
}

// backendDeliver 后端发布一条，want 里每个连接恰好收到一次。
func backendDeliver(m *mgr, topic string, want, deny []*client) error {
	tok := newTok()
	if err := backendPublish(m, topic, tok); err != nil {
		return fmt.Errorf("经 %s 的 gRPC 发布 %q 失败: %v", m.name, topic, err)
	}
	return settle(tok, fmt.Sprintf("topic %q 经 %s 的 gRPC 发布", topic, m.name), want, deny, 3*time.Second)
}

func settle(tok, what string, want, deny []*client, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		all := true
		for _, c := range want {
			if c.count(tok) < 1 {
				all = false
				break
			}
		}
		if all {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(250 * time.Millisecond)
	var missing, dup, leaked []string
	for _, c := range want {
		switch n := c.count(tok); {
		case n == 0:
			missing = append(missing, c.name())
		case n > 1:
			dup = append(dup, fmt.Sprintf("%s×%d", c.name(), n))
		}
	}
	for _, c := range deny {
		if n := c.count(tok); n > 0 {
			leaked = append(leaked, fmt.Sprintf("%s×%d", c.name(), n))
		}
	}
	if len(missing)+len(dup)+len(leaked) == 0 {
		return nil
	}
	var parts []string
	if len(missing) > 0 {
		parts = append(parts, fmt.Sprintf("没收到 %d/%d: %s", len(missing), len(want), clipList(missing)))
	}
	if len(dup) > 0 {
		parts = append(parts, "重复: "+clipList(dup))
	}
	if len(leaked) > 0 {
		parts = append(parts, "不该收到却收到: "+clipList(leaked))
	}
	return fmt.Errorf("%s: %s", what, strings.Join(parts, "; "))
}

// adminFrame 经 Manager 的 TCP 口发一条管理帧（踢人、踢 Topic），和 stress/broadcast 走同一条路。
func adminFrame(m *mgr, kind, topic string, data []byte) error {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", m.tcp), 3*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	frame, err := socket.Enpack(kind, "admin-"+newTok(), "admin", topic, data)
	if err != nil {
		return err
	}
	_ = conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write(frame); err != nil {
		return err
	}
	time.Sleep(300 * time.Millisecond)
	return nil
}

// sweep 并发检查一批 Topic：每个 Topic 发一条，want 全部恰好一次。返回第一条错误和失败个数。
func sweep(pubs []*client, topics []string, want []*client) error {
	var mu sync.Mutex
	var first error
	failed := 0
	var wg sync.WaitGroup
	for i, t := range topics {
		wg.Add(1)
		go func(i int, t string) {
			defer wg.Done()
			if err := deliver(pubs[i%len(pubs)], t, want, nil, 3*time.Second); err != nil {
				mu.Lock()
				failed++
				if first == nil {
					first = err
				}
				mu.Unlock()
			}
		}(i, t)
	}
	wg.Wait()
	if first != nil {
		return fmt.Errorf("%d/%d 个 Topic 不对，第一个: %v", failed, len(topics), first)
	}
	return nil
}

// sweepUntil 故障或重启之后用：反复检查直到一整轮全部通过，返回耗时。
func sweepUntil(pubs []*client, topics []string, want []*client, total time.Duration) (time.Duration, error) {
	start := time.Now()
	var last error
	for time.Since(start) < total {
		if last = sweep(pubs, topics, want); last == nil {
			return time.Since(start), nil
		}
		if strings.Contains(last.Error(), "重复") {
			return time.Since(start), last
		}
	}
	return time.Since(start), last
}

// roll 用当前配置重启一台 Gateway，它上面的客户端重连并重新订阅。等同于一次滚动发布。
func roll(c *cluster, g *gw, all []*client) error {
	g.p.kill()
	if err := c.startGateway(g); err != nil {
		return err
	}
	for _, cl := range onGW(all, g) {
		if err := cl.reconnect(); err != nil {
			return fmt.Errorf("%s 重连失败: %v", cl.name(), err)
		}
	}
	return nil
}

// abuse 往 Manager 的两个口灌坏数据。之后进程必须还在，正常业务不受影响。
func abuse(m *mgr) {
	tcp := fmt.Sprintf("127.0.0.1:%d", m.tcp)
	junk := make([]byte, 4096)
	_, _ = rand.Read(junk)
	payloads := [][]byte{
		junk,
		{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		{0, 0, 0, 0},
		[]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"),
		{},
	}
	if f, err := socket.Enpack("handshake", "0", "gateway", socket.HandshakeTopic, []byte(strings.Repeat("k", 4000))); err == nil {
		payloads = append(payloads, f, f[:len(f)/2])
	}
	if f, err := socket.Enpack("no_such_type", "1", "x", "no-such-topic", junk[:100]); err == nil {
		payloads = append(payloads, f)
	}
	if f, err := socket.Enpack(socket.PublishKey, "", "", "", nil); err == nil {
		payloads = append(payloads, f)
	}
	// 冒充一台已有的 Gateway：同一个 ClusterId，另一个启动随机数。
	if f, err := socket.Enpack("handshake", "0", "gateway", socket.HandshakeTopic, []byte("1-deadbeefdeadbeef-1")); err == nil {
		payloads = append(payloads, f)
	}
	for round := 0; round < 3; round++ {
		for _, p := range payloads {
			conn, err := net.DialTimeout("tcp", tcp, 2*time.Second)
			if err != nil {
				continue
			}
			_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
			_, _ = conn.Write(p)
			if round == 1 {
				time.Sleep(20 * time.Millisecond)
			}
			_ = conn.Close()
		}
	}
	cl, done, err := rpcClient(m)
	if err != nil {
		return
	}
	defer done()
	call := func(fn func(ctx context.Context)) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		fn(ctx)
	}
	many := make([]string, 5000)
	for i := range many {
		many[i] = fmt.Sprintf("abuse-%d", i)
	}
	call(func(ctx context.Context) { _, _ = cl.SubscribeTopic(ctx, &pb.SubscribeTopicRequest{}) })
	call(func(ctx context.Context) {
		_, _ = cl.SubscribeTopic(ctx, &pb.SubscribeTopicRequest{Ip: "x", Topic: []string{"a", "b"}, Count: []int64{1}, Version: []uint64{1, 2, 3}})
	})
	call(func(ctx context.Context) {
		_, _ = cl.SubscribeTopic(ctx, &pb.SubscribeTopicRequest{Ip: "", Topic: many})
	})
	call(func(ctx context.Context) {
		_, _ = cl.SubscribeTopic(ctx, &pb.SubscribeTopicRequest{Ip: "x", Topic: []string{"", socket.EpochTopic, socket.HandshakeTopic}, Round: 9, RoundEnd: true})
	})
	call(func(ctx context.Context) {
		_, _ = cl.UnSubscribeTopic(ctx, &pb.UnSubscribeTopicRequest{Ip: "nobody", Topic: []string{"never"}, Count: []int64{-5}, Version: []uint64{1}})
	})
	call(func(ctx context.Context) { _, _ = cl.Publish(ctx, &pb.PublishRequest{}) })
	call(func(ctx context.Context) { _, _ = cl.Publish(ctx, &pb.PublishRequest{Topic: "never", Data: junk}) })
	call(func(ctx context.Context) { _, _ = cl.GetConnectNum(ctx, &pb.GetConnectNumRequest{}) })
	call(func(ctx context.Context) { _, _ = cl.CheckTopicExist(ctx, &pb.CheckTopicExistRequest{}) })
}

// abuseGateway 往一台 Gateway 的 WebSocket 灌坏帧。
func abuseGateway(g *gw) {
	frames := []any{
		map[string]any{"type": "subscribe", "topic": "", "data": map[string]any{}},
		map[string]any{"type": "subscribe", "topic": strings.Repeat("x,", 3000), "data": map[string]any{}},
		map[string]any{"type": "subscribe", "topic": strings.Repeat("y", 70000), "data": map[string]any{}},
		map[string]any{"type": "unSubscribe", "topic": "never-subscribed", "data": map[string]any{}},
		map[string]any{"type": "publish", "topic": "nobody-home", "data": nil},
		map[string]any{"type": "", "topic": "", "data": ""},
		map[string]any{"type": 7, "topic": []int{1}, "data": "x"},
		map[string]any{"type": "offline_user", "topic": "room-all", "data": "u1-0"},
		map[string]any{"type": "handshake", "topic": socket.HandshakeTopic, "data": "1-x-1"},
		map[string]any{"type": "subscribe", "topic": socket.EpochTopic + "," + socket.HandshakeTopic, "data": map[string]any{}},
		"not an object",
		[]int{1, 2, 3},
	}
	for i, f := range frames {
		cl, err := dial(g, fmt.Sprintf("abuser-%d", i))
		if err != nil {
			continue
		}
		_ = cl.write(f)
		cl.mu.Lock()
		if cl.c != nil {
			_ = cl.c.WriteMessage(2, []byte{0xff, 0xfe, 0x00})
			_ = cl.c.WriteMessage(1, []byte(`{"type":"subscribe","topic":`))
		}
		cl.mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		cl.close()
	}
	// 连上就断、半开握手。
	for i := 0; i < 20; i++ {
		conn, err := net.DialTimeout("tcp", g.http, time.Second)
		if err != nil {
			continue
		}
		if i%2 == 0 {
			_, _ = conn.Write([]byte("GET /ws HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\n"))
		}
		_ = conn.Close()
	}
}

// ---------- 阶段 3 ----------

func stage3(keep bool) {
	c, err := newCluster("s3")
	if !must("S3-00", "建目录", err) {
		return
	}
	defer finish(c, "S3-99", keep)
	mA, err := c.addManager("mA")
	if !must("S3-00", "拉起 Manager A", err) {
		return
	}
	mB, err := c.addManager("mB")
	if !must("S3-00", "拉起 Manager B", err) {
		return
	}
	const n, per, nt = 3, 2, 30
	for i := 1; i <= n; i++ {
		if _, err := c.addGateway(i); !must("S3-00", fmt.Sprintf("拉起 Gateway %d", i), err) {
			return
		}
	}
	gs := c.gateways
	topics := make([]string, nt)
	for i := range topics {
		topics[i] = fmt.Sprintf("t%d", i)
	}
	var all []*client
	for _, g := range gs {
		for k := 0; k < per; k++ {
			cl, err := dial(g, fmt.Sprintf("u%d-%d", g.id, k))
			if !must("S3-00", "客户端连接", err) {
				return
			}
			_ = cl.subscribe(topics...)
			all = append(all, cl)
		}
	}
	total := n * per

	e := directNumIs(mA, topics, total, 10*time.Second)
	if e == nil {
		e = directNumIs(mB, topics, total, 10*time.Second)
	}
	rep.check("S3-01", "两台 Manager 各自都有全量路由，30 个 Topic 人数一致", e)
	e = nil
	for _, g := range gs {
		for _, t := range []string{topics[0], topics[11], topics[29]} {
			if err := numIs(g, t, total, 5*time.Second); err != nil && e == nil {
				e = err
			}
		}
	}
	rep.check("S3-02", "每台 Gateway 读到的人数相同", e)
	rep.check("S3-03", "30 个 Topic 分散到两台 Manager，全部恰好一次", sweep(all, topics, all))
	e = nil
	for _, m := range []*mgr{mA, mB} {
		for _, t := range []string{topics[1], topics[2], topics[3], topics[4]} {
			if err := backendDeliver(m, t, all, nil); err != nil && e == nil {
				e = err
			}
		}
	}
	rep.check("S3-04", "业务后端经任意一台 Manager 的 gRPC 发布，全部订阅者恰好收到一次", e)

	// 杀一台。
	mA.p.kill()
	took, e := sweepUntil(all, topics, all, 10*time.Second)
	rep.check("S3-05", "杀掉 Manager A 后，30 个 Topic 全部改由 B 送达", e)
	note("从杀掉 A 到 30 个 Topic 全部恰好一次: %s", took.Round(10*time.Millisecond))
	late, err := dial(gs[1], "late")
	if err == nil {
		_ = late.subscribe(topics...)
		_ = late.subscribe("late-only")
		err = numIs(gs[0], "late-only", 1, 5*time.Second)
	}
	withLate := append(append([]*client{}, all...), late)
	if err == nil {
		err = sweep(all, topics, withLate)
	}
	rep.check("S3-06", "一台 Manager 不在时，新订阅仍然成功并能收到", err)

	e = c.startManager(mA)
	if e == nil {
		e = directNumIs(mA, topics, total+1, 20*time.Second)
	}
	if e == nil {
		e = directNumIs(mA, []string{"late-only"}, 1, 5*time.Second)
	}
	rep.check("S3-07", "A 拉起后收到整表重放，包括它不在时才出现的订阅", e)
	rep.check("S3-08", "A 回来后 30 个 Topic 仍是恰好一次（发布目标切回去不产生重复）", sweep(all, topics, withLate))

	mB.p.kill()
	took, e = sweepUntil(all, topics, withLate, 10*time.Second)
	rep.check("S3-09", "再杀 Manager B，全部改由 A 送达", e)
	note("从杀掉 B 到全部恰好一次: %s", took.Round(10*time.Millisecond))
	e = c.startManager(mB)
	if e == nil {
		e = directNumIs(mB, topics, total+1, 20*time.Second)
	}
	if e == nil {
		e = sweep(all, topics, withLate)
	}
	rep.check("S3-10", "B 拉起后路由补齐，投递恰好一次", e)

	mA.p.kill()
	mB.p.kill()
	for _, cl := range all {
		_ = cl.publish(topics[0], newTok())
	}
	time.Sleep(2 * time.Second)
	e = c.startManager(mA)
	if e == nil {
		e = c.startManager(mB)
	}
	if e == nil {
		took, e = sweepUntil(all, topics, withLate, 30*time.Second)
	}
	rep.check("S3-11", "两台 Manager 同时被杀再拉起，原有连接不重订就恢复", e)
	note("两台都拉起后到全部恰好一次: %s", took.Round(10*time.Millisecond))
	e = directNumIs(mA, topics, total+1, 15*time.Second)
	if e == nil {
		e = directNumIs(mB, topics, total+1, 15*time.Second)
	}
	rep.check("S3-12", "两台 Manager 上的人数都恢复到故障前", e)

	// 管理命令。
	victim := all[0]
	e = adminFrame(mB, socket.OfflineTopicByUserIdKey, topics[5], []byte(victim.uid))
	if e == nil {
		e = directNumIs(mA, []string{topics[5]}, total, 5*time.Second)
	}
	if e == nil {
		e = directNumIs(mB, []string{topics[5]}, total, 5*time.Second)
	}
	if e == nil {
		e = deliver(all[3], topics[5], filter(withLate, func(x *client) bool { return x != victim }), []*client{victim}, 3*time.Second)
	}
	rep.check("S3-13", "管理命令「把某个用户踢出某个 Topic」经任意一台 Manager 生效，两台人数同步", e)
	e = adminFrame(mA, socket.OfflineTopicKey, topics[6], []byte("x"))
	if e == nil {
		e = directNumIs(mA, []string{topics[6]}, 0, 5*time.Second)
	}
	if e == nil {
		e = directNumIs(mB, []string{topics[6]}, 0, 5*time.Second)
	}
	rep.check("S3-14", "管理命令「解散某个 Topic」生效，两台 Manager 上都清空", e)
	live := []string{}
	for i, t := range topics {
		if i != 5 && i != 6 {
			live = append(live, t)
		}
	}

	// 加第三台 Manager，分两步滚动。
	mC, err := c.addManager("mC")
	if must("S3-15", "拉起 Manager C", err) {
		c.publish["mC"] = false
		e = nil
		for _, g := range gs {
			if err := roll(c, g, withLate); err != nil {
				e = err
				break
			}
			// 滚到一半时新老配置混跑，这时也不能丢。
			if _, err := sweepUntil(notOnGW(all, g), live, withLate, 15*time.Second); err != nil {
				e = fmt.Errorf("滚动 gw%d 之后: %v", g.id, err)
				break
			}
		}
		if e == nil {
			e = directNumIs(mC, live, total+1, 20*time.Second)
		}
		rep.check("S3-15", "第一步：C 只登记不发布，逐台滚动 Gateway，期间和之后都不丢；C 拿到全量路由", e)
		c.publish["mC"] = true
		e = nil
		for _, g := range gs {
			if err := roll(c, g, withLate); err != nil {
				e = err
				break
			}
			if _, err := sweepUntil(notOnGW(all, g), live, withLate, 15*time.Second); err != nil {
				e = fmt.Errorf("滚动 gw%d 之后: %v", g.id, err)
				break
			}
		}
		if e == nil {
			e = sweep(all, live, withLate)
		}
		rep.check("S3-16", "第二步：C 打开发布，逐台滚动，三台 Manager 混着用仍是恰好一次", e)
		for _, m := range []*mgr{mA, mB, mC} {
			if err := directNumIs(m, live, total+1, 10*time.Second); err != nil && e == nil {
				e = err
			}
		}
		rep.check("S3-17", "滚动结束后三台 Manager 人数一致，没有重启留下的残留", e)
		mB.p.kill()
		took, e = sweepUntil(all, live, withLate, 10*time.Second)
		rep.check("S3-18", "三台里强制停一台，没有 Topic 收不到", e)
		note("从杀掉 B 到全部恰好一次: %s", took.Round(10*time.Millisecond))
		e = c.startManager(mB)
		if e == nil {
			e = directNumIs(mB, live, total+1, 20*time.Second)
		}
		rep.check("S3-19", "B 拉起后补齐", e)
	}

	// 减一台 Manager，和加的时候反过来分两步：先让所有 Gateway 不再往 C 发布（仍然登记），
	// 再把 C 从配置里拿掉，最后才停 C。直接拿掉的话，还没滚到的 Gateway 会继续经 C 发布，
	// 而已经滚过的 Gateway 不在 C 上登记，它的订阅者就收不到。
	if len(c.managers) == 3 {
		c.publish["mC"] = false
		e = nil
		for _, g := range gs {
			if err := roll(c, g, withLate); err != nil {
				e = err
				break
			}
			if _, err := sweepUntil(notOnGW(all, g), live, withLate, 15*time.Second); err != nil {
				e = fmt.Errorf("滚动 gw%d 之后: %v", g.id, err)
				break
			}
		}
		if e == nil {
			e = sweep(all, live, withLate)
		}
		rep.check("S3-19a", "减 Manager 第一步：C 改成只登记不发布，逐台滚动，期间和之后都不丢", e)
		c.hidden["mC"] = true
		e = nil
		for _, g := range gs {
			if err := roll(c, g, withLate); err != nil {
				e = err
				break
			}
			if _, err := sweepUntil(notOnGW(all, g), live, withLate, 15*time.Second); err != nil {
				e = fmt.Errorf("滚动 gw%d 之后: %v", g.id, err)
				break
			}
		}
		if e == nil {
			e = directNumIs(c.managers[2], live, 0, 10*time.Second)
		}
		rep.check("S3-19b", "减 Manager 第二步：逐台把 C 从配置里拿掉，期间不丢；拿完后 C 上不再有任何路由", e)
		c.managers[2].p.kill()
		e = sweep(all, live, withLate)
		for _, m := range c.managers[:2] {
			if err := directNumIs(m, live, total+1, 10*time.Second); err != nil && e == nil {
				e = err
			}
		}
		rep.check("S3-19c", "停掉 C 之后没有 Topic 受影响，剩下两台人数一致", e)
		c.managers = c.managers[:2]
	}

	// 坏数据。
	for _, m := range c.managers {
		abuse(m)
	}
	for _, g := range gs {
		abuseGateway(g)
	}
	e = sweep(all, live, withLate)
	for _, m := range c.managers {
		if err := directNumIs(m, live, total+1, 10*time.Second); err != nil && e == nil {
			e = err
		}
	}
	rep.check("S3-20", "往每台 Manager 和 Gateway 灌坏帧、坏请求之后，投递和人数不受影响", e)

	// 抖动 + 轮流重启 Manager。
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
				_ = cl.subscribe(live[i%len(live)], "churn-room")
				_ = cl.publish("churn-room", newTok())
				if i%2 == 0 {
					time.Sleep(10 * time.Millisecond)
				}
				cl.close()
			}
		}(g)
	}
	for _, m := range c.managers {
		time.Sleep(2 * time.Second)
		m.p.kill()
		time.Sleep(time.Second)
		if err := c.startManager(m); err != nil {
			e = err
		}
	}
	time.Sleep(3 * time.Second)
	close(stop)
	wg.Wait()
	_, e = sweepUntil(all, live, withLate, 30*time.Second)
	rep.check("S3-21", "连接频繁进出、各台 Manager 轮流重启之后，原有连接全部互通", e)
	e = nil
	for _, m := range c.managers {
		if err := directNumIs(m, live, total+1, 15*time.Second); err != nil && e == nil {
			e = err
		}
		if err := directNumIs(m, []string{"churn-room"}, 0, 15*time.Second); err != nil && e == nil {
			e = err
		}
	}
	rep.check("S3-22", "抖动结束后每台 Manager 上的人数立刻精确，没有残留", e)
}

// ---------- 阶段 4 ----------

const shardSep = "\x1f"

func stage4(keep bool) {
	c, err := newCluster("s4")
	if !must("S4-00", "建目录", err) {
		return
	}
	defer finish(c, "S4-99", keep)
	const shards = 8
	c.groups["big"] = shards
	mA, err := c.addManager("mA")
	if !must("S4-00", "拉起 Manager A", err) {
		return
	}
	mB, err := c.addManager("mB")
	if !must("S4-00", "拉起 Manager B", err) {
		return
	}
	const n, per = 3, 10
	for i := 1; i <= n; i++ {
		if _, err := c.addGateway(i); !must("S4-00", fmt.Sprintf("拉起 Gateway %d", i), err) {
			return
		}
	}
	gs := c.gateways
	var all []*client
	for _, g := range gs {
		for k := 0; k < per; k++ {
			cl, err := dial(g, fmt.Sprintf("u%d-%d", g.id, k))
			if !must("S4-00", "客户端连接", err) {
				return
			}
			_ = cl.subscribe("big", "plain")
			all = append(all, cl)
		}
	}
	total := n * per
	e := numEverywhere(gs, "big", total, 10*time.Second)
	if e == nil {
		e = numEverywhere(gs, "plain", total, 5*time.Second)
	}
	if e == nil {
		if s, err := httpGet(gs[0].http, "/exists?topic=big"); err != nil || s != "true" {
			e = fmt.Errorf("CheckTopicExist(big) = %q, %v", s, err)
		}
	}
	rep.check("S4-01", "分组房间的人数是各片之和，CheckTopicExist 为真；没分组的房间不受影响", e)

	// 确认真的分了片，否则后面的用例都是空测。
	used := 0
	sum := 0
	e = nil
	for i := 0; i < shards; i++ {
		k, err := directNum(mA, fmt.Sprintf("big%s%d", shardSep, i))
		if err != nil {
			e = err
			break
		}
		if k > 0 {
			used++
		}
		sum += k
	}
	if e == nil && (sum != total || used < 2) {
		e = fmt.Errorf("Manager 上 %d 片里有 %d 片有人，合计 %d，应合计 %d 且不止一片", shards, used, sum, total)
	}
	rep.check("S4-02", "Manager 上房间确实按用户拆成了多片", e)
	note("%d 个用户落在 %d/%d 片上", total, used, shards)

	e = nil
	for _, g := range gs {
		if err := deliver(onGW(all, g)[0], "big", all, nil, 3*time.Second); err != nil && e == nil {
			e = err
		}
		if err := deliver(onGW(all, g)[1], "plain", all, nil, 3*time.Second); err != nil && e == nil {
			e = err
		}
	}
	rep.check("S4-03", "任意 Gateway 上的客户端向分组房间发布，所有片的订阅者都恰好收到一次", e)

	quit := all[:5]
	for _, cl := range quit {
		_ = cl.unsubscribe("big")
	}
	stay := all[5:]
	e = numEverywhere(gs, "big", total-5, 5*time.Second)
	if e == nil {
		e = deliver(stay[0], "big", stay, quit, 3*time.Second)
	}
	rep.check("S4-04", "从分组房间退订：人数精确减少，退订的不再收到", e)
	anon, err := dial(gs[2], "")
	if err == nil {
		_ = anon.subscribe("big")
		err = numEverywhere(gs, "big", total-4, 5*time.Second)
	}
	members := append(append([]*client{}, stay...), anon)
	if err == nil {
		err = deliver(stay[3], "big", members, quit, 3*time.Second)
	}
	rep.check("S4-05", "没有 UserId 的连接也能进分组房间", err)

	e = nil
	for _, m := range []*mgr{mA, mB} {
		if err := backendDeliver(m, "plain", all, nil); err != nil && e == nil {
			e = err
		}
		if err := backendDeliver(m, "big", members, quit); err != nil && e == nil {
			e = err
		}
	}
	rep.check("S4-06", "业务后端按房间名经 gRPC 向分组房间发布，所有片恰好收到一次", e)
	e = nil
	for _, m := range []*mgr{mA, mB} {
		k, err := directNum(m, "big")
		if err != nil {
			e = err
		} else if k != len(members) && e == nil {
			e = fmt.Errorf("直接问 %s 分组房间人数得到 %d，应为 %d", m.name, k, len(members))
		}
	}
	rep.check("S4-07", "业务后端按房间名直接问 Manager 人数，得到各片之和", e)

	mA.p.kill()
	took, _, e := deliverUntil(stay[0], "big", members, 10*time.Second)
	rep.check("S4-08", "杀掉一台 Manager，分组房间所有片仍然送达", e)
	note("从杀掉 A 到分组房间全部恰好一次: %s", took.Round(10*time.Millisecond))
	e = c.startManager(mA)
	if e == nil {
		e = directNumIs(mA, []string{"plain"}, total, 20*time.Second)
	}
	if e == nil {
		e = deliver(stay[7], "big", members, quit, 3*time.Second)
	}
	if e == nil {
		e = numEverywhere(gs, "big", len(members), 10*time.Second)
	}
	rep.check("S4-09", "那台 Manager 拉起后，分组房间仍是恰好一次，人数不变", e)

	// 管理命令作用在分组房间上。
	kicked := stay[1]
	e = adminFrame(mB, socket.OfflineTopicByUserIdKey, "big", []byte(kicked.uid))
	rest := filter(members, func(x *client) bool { return x != kicked })
	if e == nil {
		e = numEverywhere(gs, "big", len(rest), 5*time.Second)
	}
	if e == nil {
		e = deliver(stay[2], "big", rest, append(append([]*client{}, quit...), kicked), 3*time.Second)
	}
	rep.check("S4-10", "管理命令「把某个用户踢出分组房间」按房间名生效", e)

	last := gs[len(gs)-1]
	gone := len(onGW(rest, last))
	last.p.kill()
	rest = notOnGW(rest, last)
	e = numEverywhere(gs[:len(gs)-1], "big", len(rest), 10*time.Second)
	if e == nil {
		e = deliver(rest[0], "big", rest, nil, 3*time.Second)
	}
	rep.check("S4-11", fmt.Sprintf("硬杀一台 Gateway，分组房间人数减去它上面的 %d 人，其余照常", gone), e)

	// 客户端自己拼片键去订阅和发布：不能崩，也不能把人数带歪。
	sneak, err := dial(gs[0], "sneak")
	if err == nil {
		_ = sneak.subscribe("big" + shardSep + "3")
		_ = sneak.publish("big"+shardSep+"3", newTok())
		_ = sneak.publish("big"+shardSep+"999", newTok())
		_ = sneak.subscribe(shardSep, "big"+shardSep, shardSep+"big")
		time.Sleep(300 * time.Millisecond)
		sneak.close()
		err = numEverywhere(gs[:len(gs)-1], "big", len(rest), 5*time.Second)
	}
	if err == nil {
		err = deliver(rest[1], "big", rest, nil, 3*time.Second)
	}
	rep.check("S4-12", "客户端自己拼片键订阅、发布之后，进程不崩，房间人数和投递不变", err)

	e = adminFrame(mA, socket.OfflineTopicKey, "big", []byte("x"))
	if e == nil {
		e = numEverywhere(gs[:len(gs)-1], "big", 0, 5*time.Second)
	}
	if e == nil {
		e = numEverywhere(gs[:len(gs)-1], "plain", total-per, 5*time.Second)
	}
	rep.check("S4-13", "管理命令「解散分组房间」按房间名生效，别的房间不受影响", e)
}
