package main

// 单机压测（-stage bench，不在 all 里）。量三样部署手册要用的数：
//   B1 每条空闲连接占多少内存、多少协程；
//   B2 一台 Gateway 的投递预算：一个房间 N 人时，每秒能发多少条而不丢、不慢；
//   B3 一台 Manager 的转发能力：多台 Gateway 时，每秒能转发多少帧。
// 所有进程和压测客户端都在同一台机器上，压测客户端自己也吃 CPU，所以结果是这台机器上的下限，
// 不是生产机器的容量。生产选型以在目标机器上重跑的结果为准。

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

var (
	benchConns = 10000
	benchSubs  = 2000
	benchGWs   = 8
	benchSize  = 1024
	benchStep  = 5 * time.Second
)

const (
	benchMaxSteps = 32
	benchLossMax  = 0.01 // 丢失超过 1% 视为这一档没过
	benchP99Max   = time.Second
)

// 延迟分桶的上界（毫秒），最后一桶是更大的。
var latBounds = []int64{1, 2, 5, 10, 20, 50, 100, 200, 500, 1000, 2000, 5000, 10000}

type benchClient struct {
	conn *websocket.Conn
	wmu  sync.Mutex
	got  [benchMaxSteps]atomic.Int64
	hist [benchMaxSteps][14]atomic.Int64
	dead atomic.Bool
}

func dialBench(g *gw, uid string, topics ...string) (*benchClient, error) {
	d := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, _, err := d.Dial("ws://"+g.http+"/ws?uid="+url.QueryEscape(uid), nil)
	if err != nil {
		return nil, err
	}
	bc := &benchClient{conn: conn}
	if len(topics) > 0 {
		if err := bc.writeJSON(map[string]any{"type": "subscribe", "topic": strings.Join(topics, ","), "data": map[string]any{}}); err != nil {
			conn.Close()
			return nil, err
		}
	}
	go bc.read()
	return bc, nil
}

func (b *benchClient) writeJSON(v any) error {
	b.wmu.Lock()
	defer b.wmu.Unlock()
	_ = b.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return b.conn.WriteJSON(v)
}

func (b *benchClient) writeRaw(p []byte) error {
	b.wmu.Lock()
	defer b.wmu.Unlock()
	_ = b.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return b.conn.WriteMessage(websocket.TextMessage, p)
}

func (b *benchClient) read() {
	for {
		_, msg, err := b.conn.ReadMessage()
		if err != nil {
			b.dead.Store(true)
			return
		}
		step, ok1 := findInt(msg, "bs")
		sent, ok2 := findInt(msg, "bt")
		if !ok1 || !ok2 || step < 0 || step >= benchMaxSteps {
			continue
		}
		b.got[step].Add(1)
		ms := (time.Now().UnixNano() - sent) / int64(time.Millisecond)
		i := sort.Search(len(latBounds), func(i int) bool { return ms <= latBounds[i] })
		b.hist[step][i].Add(1)
	}
}

func (b *benchClient) close() { _ = b.conn.Close() }

// findInt 在帧里找 "key":数字。正文可能被转义成字符串，所以也认 \"key\":。
func findInt(msg []byte, key string) (int64, bool) {
	s := string(msg)
	for _, pat := range []string{`"` + key + `":`, `\"` + key + `\":`} {
		i := strings.Index(s, pat)
		if i < 0 {
			continue
		}
		j := i + len(pat)
		k := j
		for k < len(s) && s[k] >= '0' && s[k] <= '9' {
			k++
		}
		if k == j {
			continue
		}
		v, err := strconv.ParseInt(s[j:k], 10, 64)
		return v, err == nil
	}
	return 0, false
}

type pstat struct {
	live, total uint64
	goroutines  int
	cpu         float64
}

func readStats(addr string) (pstat, error) {
	s, err := httpGet(addr, "/stats")
	if err != nil {
		return pstat{}, err
	}
	var p pstat
	_, err = fmt.Sscanf(s, "%d %d %d %f", &p.live, &p.total, &p.goroutines, &p.cpu)
	return p, err
}

func mb(v uint64) float64 { return float64(v) / (1 << 20) }

// benchCluster 按部署手册场景 A 的模板起 Manager 和 Gateway。
func benchCluster(tag string, gateways int) (*cluster, string, error) {
	c, err := newCluster(tag)
	if err != nil {
		return nil, "", err
	}
	mhttp := fmt.Sprintf("127.0.0.1:%d", freePort())
	c.managerEnv = []string{"SC_MEMHTTP=" + mhttp}
	c.extraTop = "chanLens = 1000\nheartbeat = 30\nmaxConnections = 1000000\n"
	c.bucket = "\n[bucket]\nNum = 4\nCentralChanCount = 100000\nBuffChanCount = 1000\nConsumerNum = 1\n"
	if _, err := c.addManager("m1"); err != nil {
		return c, "", err
	}
	for i := 1; i <= gateways; i++ {
		if _, err := c.addGateway(i); err != nil {
			return c, "", err
		}
	}
	return c, mhttp, nil
}

// connectMany 并发建 n 条连接，第 i 条订阅 topicOf(i)。
func connectMany(gs []*gw, n int, topicOf func(i int) string) ([]*benchClient, error) {
	out := make([]*benchClient, n)
	sem := make(chan struct{}, 64)
	var wg sync.WaitGroup
	var firstErr atomic.Value
	for i := 0; i < n; i++ {
		if firstErr.Load() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			var bc *benchClient
			var err error
			for try := 0; try < 3; try++ {
				bc, err = dialBench(gs[i%len(gs)], fmt.Sprintf("b%d", i), topicOf(i))
				if err == nil {
					break
				}
				time.Sleep(200 * time.Millisecond)
			}
			if err != nil {
				firstErr.CompareAndSwap(nil, fmt.Errorf("第 %d 条连接: %v", i, err))
				return
			}
			out[i] = bc
		}(i)
	}
	wg.Wait()
	if e := firstErr.Load(); e != nil {
		for _, bc := range out {
			if bc != nil {
				bc.close()
			}
		}
		return nil, e.(error)
	}
	return out, nil
}

func closeAll(cs []*benchClient) {
	for _, c := range cs {
		if c != nil {
			c.close()
		}
	}
}

func stageBench(keep bool) {
	if n := raiseFileLimit(); n > 0 && uint64(benchConns)+512 > n {
		old := benchConns
		benchConns = int(n) - 512
		note("文件句柄上限 %d，B1 的连接数从 %d 降到 %d", n, old, benchConns)
	}
	benchIdle(keep)
	benchDeliver(keep)
	benchManager(keep)
}

// ---------- B1 空闲连接 ----------

func benchIdle(keep bool) {
	c, _, err := benchCluster("bench-idle", 1)
	if !must("B1", "起 Manager 和 Gateway", err) {
		if c != nil {
			finish(c, "B1-X", keep)
		}
		return
	}
	g := c.gateways[0]
	time.Sleep(2 * time.Second)
	base, err := readStats(g.http)
	if !must("B1", "读 Gateway 状态", err) {
		finish(c, "B1-X", keep)
		return
	}
	note("基线：存活堆 %.1fMB，Go 管理内存 %.1fMB，协程 %d", mb(base.live), mb(base.total), base.goroutines)

	const rooms = 100
	topicOf := func(i int) string { return fmt.Sprintf("idle-%d", i%rooms) }
	var all []*benchClient
	for _, target := range []int{benchConns / 2, benchConns} {
		start := time.Now()
		more, err := connectMany(c.gateways, target-len(all), func(i int) string { return topicOf(len(all) + i) })
		if !must("B1", fmt.Sprintf("建到 %d 条连接", target), err) {
			closeAll(all)
			finish(c, "B1-X", keep)
			return
		}
		all = append(all, more...)
		want := 0
		for i := 0; i < len(all); i++ {
			if i%rooms == 0 {
				want++
			}
		}
		if !must("B1", "订阅全部登记完", numIs(g, topicOf(0), want, 60*time.Second)) {
			closeAll(all)
			finish(c, "B1-X", keep)
			return
		}
		took := time.Since(start)
		time.Sleep(5 * time.Second)
		st, err := readStats(g.http)
		if !must("B1", "读 Gateway 状态", err) {
			closeAll(all)
			finish(c, "B1-X", keep)
			return
		}
		n := float64(len(all))
		note("BENCH B1 连接 %d：建连+订阅用时 %s；存活堆 %.1fMB（每连接 %.1fKB），Go 管理内存 %.1fMB（每连接 %.1fKB），协程 %d（每连接 %.2f）",
			len(all), took.Round(time.Millisecond),
			mb(st.live), float64(st.live-min64(st.live, base.live))/n/1024,
			mb(st.total), float64(st.total-min64(st.total, base.total))/n/1024,
			st.goroutines, float64(st.goroutines-base.goroutines)/n)
	}
	closeAll(all)
	// 不主动触发 GC，等 Go 自己回收。
	deadline := time.Now().Add(150 * time.Second)
	var after pstat
	for time.Now().Before(deadline) {
		after, _ = readStats(g.http)
		if after.goroutines <= base.goroutines+10 && after.live < base.live+(32<<20) {
			break
		}
		time.Sleep(2 * time.Second)
	}
	note("BENCH B1 全部断开后：协程 %d，存活堆 %.1fMB", after.goroutines, mb(after.live))
	rep.check("B1-01", fmt.Sprintf("%d 条空闲连接建立、登记并断开后协程回到基线", benchConns), func() error {
		if after.goroutines > base.goroutines+10 {
			return fmt.Errorf("断开后协程 %d，基线 %d", after.goroutines, base.goroutines)
		}
		return nil
	}())
	finish(c, "B1-02", keep)
}

func min64(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}

// ---------- 发布阶梯 ----------

type stepResult struct {
	rate       int
	sent       int64
	expect     int64
	got        int64
	p50, p99   int64 // 毫秒，上界
	gwCPU      float64
	selfCPU    float64
	mgrCPU     float64
	passed     bool
	actualRate float64
}

// ladder 按 rates 逐档发布，每档 benchStep。subs 是所有应收到的客户端。
func ladder(pub *benchClient, topic string, subs []*benchClient, rates []int, gwStats []string, mgrStats string) []stepResult {
	pad := strings.Repeat("x", benchSize)
	var out []stepResult
	for step, rate := range rates {
		if step >= benchMaxSteps {
			break
		}
		cpu0 := sumCPU(gwStats)
		m0, _ := readStats(mgrStats)
		self0 := osCPUSeconds()
		start := time.Now()
		var sent int64
		tick := time.NewTicker(5 * time.Millisecond)
		for now := range tick.C {
			el := now.Sub(start)
			if el >= benchStep {
				break
			}
			due := int64(float64(rate) * el.Seconds())
			for sent < due {
				data, _ := json.Marshal(map[string]any{"bs": step, "bt": time.Now().UnixNano(), "p": pad})
				frame, _ := json.Marshal(map[string]any{"type": "publish", "topic": topic, "data": json.RawMessage(data)})
				if err := pub.writeRaw(frame); err != nil {
					break
				}
				sent++
			}
		}
		tick.Stop()
		wall := time.Since(start)
		// 等这一档的消息落地：连续 1 秒没有新增就算完，最多 10 秒。
		expect := sent * int64(len(subs))
		var got, last int64
		stable := time.Now()
		for end := time.Now().Add(10 * time.Second); time.Now().Before(end); {
			got = 0
			for _, s := range subs {
				got += s.got[step].Load()
			}
			if got >= expect {
				break
			}
			if got != last {
				last, stable = got, time.Now()
			} else if time.Since(stable) > time.Second {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		cpu1 := sumCPU(gwStats)
		m1, _ := readStats(mgrStats)
		self1 := osCPUSeconds()
		var hist [14]int64
		for _, s := range subs {
			for i := range hist {
				hist[i] += s.hist[step][i].Load()
			}
		}
		r := stepResult{rate: rate, sent: sent, expect: expect, got: got,
			p50: pct(hist, 0.50), p99: pct(hist, 0.99),
			gwCPU: (cpu1 - cpu0) / wall.Seconds(), mgrCPU: (m1.cpu - m0.cpu) / wall.Seconds(), selfCPU: (self1 - self0) / wall.Seconds(),
			actualRate: float64(sent) / wall.Seconds()}
		loss := 1.0
		if expect > 0 {
			loss = 1 - float64(got)/float64(expect)
		}
		r.passed = loss <= benchLossMax && r.p99 <= benchP99Max.Milliseconds() && r.actualRate >= 0.9*float64(rate)
		out = append(out, r)
		note("BENCH 档 %d/s：实发 %.0f/s，应收 %d 实收 %d（丢 %.2f%%），延迟 p50≤%dms p99≤%dms，Gateway CPU %.2f 核，Manager CPU %.2f 核，压测客户端 CPU %.2f 核 %s",
			rate, r.actualRate, expect, got, loss*100, r.p50, r.p99, r.gwCPU, r.mgrCPU, r.selfCPU, map[bool]string{true: "通过", false: "未通过"}[r.passed])
		if !r.passed {
			break
		}
		time.Sleep(time.Second)
	}
	return out
}

func sumCPU(addrs []string) float64 {
	var t float64
	for _, a := range addrs {
		if st, err := readStats(a); err == nil {
			t += st.cpu
		}
	}
	return t
}

func pct(hist [14]int64, q float64) int64 {
	var total int64
	for _, v := range hist {
		total += v
	}
	if total == 0 {
		return 0
	}
	need := int64(float64(total) * q)
	var acc int64
	for i, v := range hist {
		acc += v
		if acc >= need {
			if i < len(latBounds) {
				return latBounds[i]
			}
			return 99999
		}
	}
	return 99999
}

func bestPassed(rs []stepResult) (stepResult, bool) {
	var best stepResult
	ok := false
	for _, r := range rs {
		if r.passed {
			best, ok = r, true
		}
	}
	return best, ok
}

// ---------- B2 一台 Gateway 的投递预算 ----------

func benchDeliver(keep bool) {
	c, mhttp, err := benchCluster("bench-deliver", 1)
	if !must("B2", "起 Manager 和 Gateway", err) {
		if c != nil {
			finish(c, "B2-X", keep)
		}
		return
	}
	g := c.gateways[0]
	const room = "bench-room"
	subs, err := connectMany(c.gateways, benchSubs, func(int) string { return room })
	if !must("B2", fmt.Sprintf("建 %d 个订阅者", benchSubs), err) {
		finish(c, "B2-X", keep)
		return
	}
	defer closeAll(subs)
	pub, err := dialBench(g, "pub")
	if !must("B2", "建发布者", err) {
		finish(c, "B2-X", keep)
		return
	}
	defer pub.close()
	if !must("B2", "订阅登记完", numIs(g, room, benchSubs, 60*time.Second)) {
		finish(c, "B2-X", keep)
		return
	}
	note("B2：一台 Gateway，房间 %d 人，每条 %d 字节，每档 %s", benchSubs, benchSize, benchStep)
	rs := ladder(pub, room, subs, []int{25, 50, 100, 200, 400, 800, 1600, 3200}, []string{g.http}, mhttp)
	if best, ok := bestPassed(rs); ok {
		note("BENCH B2 结论：这台机器上一台 Gateway 在 %d 人的房间里稳定发 %d 条/秒，即每秒投递 %d 条（%.1fMB/s），Gateway CPU %.2f 核",
			benchSubs, best.rate, int64(best.rate)*int64(benchSubs), float64(best.rate)*float64(benchSubs)*float64(benchSize)/(1<<20), best.gwCPU)
	} else {
		note("BENCH B2 结论：最低一档也没过")
	}
	rep.check("B2-01", "压测期间和之后 Gateway、Manager 都活着", func() error {
		for _, p := range c.procs() {
			if !p.alive() {
				return fmt.Errorf("%s 退出了", p.name)
			}
		}
		return nil
	}())
	finish(c, "B2-02", keep)
}

// ---------- B3 一台 Manager 的转发 ----------

func benchManager(keep bool) {
	c, mhttp, err := benchCluster("bench-manager", benchGWs)
	if !must("B3", fmt.Sprintf("起 1 台 Manager 和 %d 台 Gateway", benchGWs), err) {
		if c != nil {
			finish(c, "B3-X", keep)
		}
		return
	}
	const room = "bench-fan"
	per := 10
	subs, err := connectMany(c.gateways, benchGWs*per, func(int) string { return room })
	if !must("B3", "建订阅者", err) {
		finish(c, "B3-X", keep)
		return
	}
	defer closeAll(subs)
	pub, err := dialBench(c.gateways[0], "pub")
	if !must("B3", "建发布者", err) {
		finish(c, "B3-X", keep)
		return
	}
	defer pub.close()
	if !must("B3", "订阅登记完", numIs(c.gateways[0], room, benchGWs*per, 60*time.Second)) {
		finish(c, "B3-X", keep)
		return
	}
	var gws []string
	for _, g := range c.gateways {
		gws = append(gws, g.http)
	}
	note("B3：1 台 Manager、%d 台 Gateway（每台 %d 人订阅同一房间），每条 %d 字节，每档 %s。Manager 每收一条要写出 %d 帧",
		benchGWs, per, benchSize, benchStep, benchGWs)
	rs := ladder(pub, room, subs, []int{250, 500, 1000, 2000, 4000, 8000, 16000}, gws, mhttp)
	if best, ok := bestPassed(rs); ok {
		note("BENCH B3 结论：这台机器上一台 Manager 稳定转发 %d 条/秒 × %d 台 Gateway = %d 帧/秒（出口约 %.1fMB/s），Manager CPU %.2f 核",
			best.rate, benchGWs, best.rate*benchGWs, float64(best.rate*benchGWs)*float64(benchSize)/(1<<20), best.mgrCPU)
	} else {
		note("BENCH B3 结论：最低一档也没过")
	}
	rep.check("B3-01", "压测期间和之后各进程都活着", func() error {
		for _, p := range c.procs() {
			if !p.alive() {
				return fmt.Errorf("%s 退出了", p.name)
			}
		}
		return nil
	}())
	finish(c, "B3-02", keep)
}
