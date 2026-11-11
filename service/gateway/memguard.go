package gateway

import (
	"fmt"
	"runtime/metrics"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/OSMeteor/firetower/socket"
)

// 内存水位保护。默认开启，上限按机器自动取（容器限制、GOMEMLIMIT、物理内存三者中最小的）；
// 配了 memory.limitMB 就用配置的，配 memory.disable = true 可以关掉。
//
// 检测：每 100ms 读一次 Go 运行时自带的两个指标，不暂停程序、不主动触发 GC，读取复用同一块缓冲。
//   - live：上一次 GC 之后还活着的对象，代表真正被占住的内存。各道水位都按它判断。
//   - heap：当前堆上的对象，含还没回收的垃圾。平时就会周期性涨到 live 的两倍左右，
//     所以只在它逼近 critical（真快撑爆了）时才参与判断，否则会误拒、误丢。
//
// 连接的状态，每轮现算：
//   - 存活：最后活跃（收到 Pong 或任何客户端消息）在 1.5 个心跳周期以内。客户端一直回心跳就一直续命，
//     存活的连接任何水位下都不会被关。
//   - 积压：发送队列里堆着的消息数达到队列容量的 1/8。
//
// 水位（都是相对 limitMB 的百分比）：
//   - soft（默认 70）：拒绝新连接。
//   - hard（默认 85）：每 100ms 更新一次标记，标记亮着时在入队处拦住已经积压的队列，不让它继续涨；
//     同时每 500ms 做一轮清理：
//     不存活的连接按「沉默最久 → 积压最多 → 建立最早」排序，每轮最多关 shedPercent%（至少 16 条）；
//     存活但积压的连接不关，把队列里的旧消息丢到容量的 1/4，打开了缺失提示的连接会收到 gap。
//   - critical（默认 95，按 live）：不存活的连接这一轮全部关掉；存活但积压的连接队列清空。
//
// 关连接走正常的 Close：退订、人数减掉、下线回调照常触发，和客户端自己断开一样。

type memState struct {
	limit    int64
	soft     int64
	hard     int64
	critical int64
	shedPct  int64
}

var (
	memCfg       atomic.Pointer[memState]
	memRejecting atomic.Bool
	memPressure  atomic.Bool // 每 100ms 更新：到了 hard 水位
	memLive      atomic.Uint64
	memHeap      atomic.Uint64
	memClosed    atomic.Int64 // 因内存关掉的连接数
	memTrimmed   atomic.Int64 // 因内存丢掉的排队消息数
	memGuardOnce sync.Once
	memLastWarn  atomic.Int64
	memClosing   sync.Map // connId -> struct{}，避免同一条连接被重复安排关闭
	memCloseSem  = make(chan struct{}, 64)
	memRejected  atomic.Int64 // 因内存拒绝的新连接数
	memRejectLog atomic.Int64
	memTick      = 100 * time.Millisecond // 读指标、更新标记的间隔
	memShedEvery = 5                      // 每几次检测做一轮清理（500ms）
)

// MemoryGuardStats 返回内存保护的状态，供监控使用。没开启时 limit 为 0。
func MemoryGuardStats() (limit int64, liveHeap, heap uint64, rejecting bool, closed, trimmed int64) {
	if st := memCfg.Load(); st != nil {
		limit = st.limit
	}
	return limit, memLive.Load(), memHeap.Load(), memRejecting.Load(), memClosed.Load(), memTrimmed.Load()
}

func startMemoryGuard() {
	if tree := loadConfigTree(); tree != nil {
		if off, ok := tree.Get("memory.disable").(bool); ok && off {
			callTowerLogger(nil, "INFO", "memory guard: disabled by memory.disable")
			return
		}
	}
	// 上限默认按机器自动取：容器限制、GOMEMLIMIT、物理内存三者中最小的。配了 memory.limitMB 就用配置的。
	// 优先级：配置 > 自动检测。
	detected, detectedFrom := socket.DetectMemoryLimit()
	limit, source := int64(configInt("memory.limitMB", 0))<<20, "memory.limitMB"
	if limit <= 0 {
		limit, source = detected, detectedFrom
	} else if detected > 0 && limit > detected {
		callTowerLogger(nil, "WARN", fmt.Sprintf("memory guard: memory.limitMB=%dMB is above what this machine allows (%dMB from %s); using the configured value",
			limit>>20, detected>>20, detectedFrom))
	}
	if limit <= 0 {
		callTowerLogger(nil, "WARN", "memory guard: could not detect the memory limit, guard is off; set memory.limitMB to turn it on")
		return
	}
	pct := func(key string, def int) int64 {
		v := int64(configInt(key, def))
		if v <= 0 || v > 100 {
			v = int64(def)
		}
		return v
	}
	st := &memState{
		limit:    limit,
		soft:     limit * pct("memory.softPercent", 70) / 100,
		hard:     limit * pct("memory.hardPercent", 85) / 100,
		critical: limit * pct("memory.criticalPercent", 95) / 100,
		shedPct:  pct("memory.shedPercent", 5),
	}
	memCfg.Store(st)
	callTowerLogger(nil, "INFO", fmt.Sprintf("memory guard: limit %dMB from %s (reject new connections at %dMB, clean up at %dMB)",
		st.limit>>20, source, st.soft>>20, st.hard>>20))
	memGuardOnce.Do(func() { go memGuardLoop() })
}

func memGuardLoop() {
	samples := []metrics.Sample{
		{Name: "/gc/heap/live:bytes"},
		{Name: "/memory/classes/heap/objects:bytes"},
	}
	var buf []memCandidate
	ticker := time.NewTicker(memTick)
	defer ticker.Stop()
	n := 0
	for range ticker.C {
		n++
		func() {
			defer func() {
				if err := recover(); err != nil {
					fmt.Printf("PANIC: memory guard recovered: %v\n", err)
				}
			}()
			buf = memGuardOnceTick(samples, buf[:0], n%memShedEvery == 0)
		}()
	}
}

func readSample(s metrics.Sample) uint64 {
	if s.Value.Kind() == metrics.KindUint64 {
		return s.Value.Uint64()
	}
	return 0
}

type memCandidate struct {
	t       *FireTower
	silent  time.Duration
	backlog int
	start   time.Time
}

func memGuardOnceTick(samples []metrics.Sample, buf []memCandidate, shedRound bool) []memCandidate {
	st := memCfg.Load()
	if st == nil {
		return buf
	}
	metrics.Read(samples)
	live, heap := readSample(samples[0]), readSample(samples[1])
	memLive.Store(live)
	memHeap.Store(heap)

	// 按存活对象判断。当前堆含没回收的垃圾，平时就会周期性涨到存活对象的两倍左右，拿它判断会误拒；
	// 只有当前堆逼近最后一道水位（真快撑爆了）时才算上它。
	rejecting := int64(live) >= st.soft || int64(heap) >= st.critical
	if memRejecting.Swap(rejecting) != rejecting {
		callTowerLogger(nil, "WARN", fmt.Sprintf("memory guard: rejecting new connections=%t live=%dMB heap=%dMB limit=%dMB",
			rejecting, live>>20, heap>>20, st.limit>>20))
	}
	shed := int64(live) >= st.hard || int64(heap) >= st.critical
	memPressure.Store(shed)
	if !shed || !shedRound {
		return buf
	}
	critical := int64(live) >= st.critical
	return memShed(st, critical, buf)
}

func memShed(st *memState, critical bool, buf []memCandidate) []memCandidate {
	now := time.Now()
	alive := heartbeatInterval() * 3 / 2
	var dead, slow []memCandidate
	buf = buf[:0]
	liveTowers.Range(func(_, value any) bool {
		t, _ := value.(*FireTower)
		if t == nil || t.sendOut == nil {
			return true
		}
		c := memCandidate{
			t:       t,
			silent:  now.Sub(time.Unix(0, t.lastActive.Load())),
			backlog: len(t.sendOut),
			start:   t.startTime,
		}
		buf = append(buf, c)
		return true
	})
	for i := range buf {
		c := buf[i]
		switch {
		case c.silent > alive:
			dead = append(dead, c)
		case c.backlog >= memBacklogMin(cap(c.t.sendOut)):
			slow = append(slow, c)
		}
	}

	dead = memOrder(dead)
	quota := len(dead)
	if !critical {
		quota = int(int64(len(buf)) * st.shedPct / 100)
		if quota < 16 {
			quota = 16
		}
	}
	closed := 0
	for _, c := range dead {
		if closed >= quota {
			break
		}
		if memScheduleClose(c.t) {
			closed++
		}
	}

	// 存活但积压的连接：不关，丢掉队列里的旧消息。
	sort.Slice(slow, func(i, j int) bool { return slow[i].backlog > slow[j].backlog })
	trimmed := 0
	for _, c := range slow {
		keep := cap(c.t.sendOut) / 4
		if critical {
			keep = 0
		}
		trimmed += trimQueue(c.t, keep)
	}

	if closed > 0 || trimmed > 0 {
		memClosed.Add(int64(closed))
		memTrimmed.Add(int64(trimmed))
		nowNs := now.UnixNano()
		if prev := memLastWarn.Load(); nowNs-prev > int64(5*time.Second) && memLastWarn.CompareAndSwap(prev, nowNs) {
			callTowerLogger(nil, "WARN", fmt.Sprintf("memory guard: closed %d silent connections, dropped %d queued messages on %d slow connections (live=%dMB limit=%dMB critical=%t)",
				closed, trimmed, len(slow), memLive.Load()>>20, st.limit>>20, critical))
		}
	}
	// 不把连接指针留在复用的缓冲里，免得关掉的连接要等下一轮才能被回收。
	for i := range buf {
		buf[i] = memCandidate{}
	}
	return buf[:0]
}

// memBacklogMin 发送队列里堆到多少条算积压：容量的 1/8，至少 1 条。
func memBacklogMin(capacity int) int {
	if n := capacity / 8; n > 1 {
		return n
	}
	return 1
}

// memOrder 不存活的连接的关闭顺序：沉默最久 → 积压最多 → 建立最早。
func memOrder(c []memCandidate) []memCandidate {
	sort.SliceStable(c, func(i, j int) bool {
		a, b := c[i], c[j]
		if a.silent != b.silent {
			return a.silent > b.silent
		}
		if a.backlog != b.backlog {
			return a.backlog > b.backlog
		}
		return a.start.Before(b.start)
	})
	return c
}

// trimQueue 从发送队列头部丢掉旧消息，直到只剩 keep 条。返回丢掉的条数。
// 发送协程同时也在取，这里用非阻塞读，取空就停。
func trimQueue(t *FireTower, keep int) int {
	if t == nil || t.sendOut == nil {
		return 0
	}
	n := 0
	for len(t.sendOut) > keep {
		select {
		case msg, ok := <-t.sendOut:
			if !ok {
				return n
			}
			n++
			noteSendDrop(t)
			if msg != nil {
				t.noteGap(parentTopic(msg.Topic))
			}
		default:
			return n
		}
	}
	return n
}

func memScheduleClose(t *FireTower) bool {
	if t == nil {
		return false
	}
	if _, busy := memClosing.LoadOrStore(t.connId, struct{}{}); busy {
		return false
	}
	go func() {
		defer memClosing.Delete(t.connId)
		defer func() { _ = recover() }()
		memCloseSem <- struct{}{}
		defer func() { <-memCloseSem }()
		safeClose(t)
	}()
	return true
}

// noteMemReject 记一次因内存拒绝的新连接。日志每 5 秒最多一条，带这段时间的次数。
func noteMemReject() {
	n := memRejected.Add(1)
	now := time.Now().UnixNano()
	prev := memRejectLog.Load()
	if now-prev < int64(5*time.Second) || !memRejectLog.CompareAndSwap(prev, now) {
		return
	}
	callTowerLogger(nil, "WARN", fmt.Sprintf("memory guard: rejecting new connections (total rejected %d, live=%dMB)", n, memLive.Load()>>20))
}

// touch 记下这条连接最后一次有动静的时间：收到 Pong 或任何客户端消息。
func (t *FireTower) touch() {
	if t != nil {
		t.lastActive.Store(time.Now().UnixNano())
	}
}
