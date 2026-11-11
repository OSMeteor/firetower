package manager

import (
	"fmt"
	"runtime/metrics"
	"sync"
	"sync/atomic"
	"time"

	"github.com/OSMeteor/firetower/socket"
)

// 内存水位保护。默认开启：Manager 启动时按机器自动取上限（容器限制、GOMEMLIMIT、物理内存三者中最小的）。
// EnableMemoryGuard 可以改用指定的上限，DisableMemoryGuard 可以关掉。
//
// Manager 占内存的主要是发给各台 Gateway 的待发队列。这里不关 Gateway 连接：
// 关了 Gateway 会整表重放，内存反而更紧。只丢掉积压的待发消息（框架本来就是最多一次投递）。
//   - hard（默认 85%，按上一次 GC 后的存活对象；当前堆逼近 critical 时也算）：
//     积压超过队列 1/4 的 Gateway，把队列丢到 1/4。
//   - critical（默认 95%）：所有 Gateway 的待发队列清空。
// 检测每 100ms 一次，只读运行时指标，不暂停程序、不主动触发 GC。
// 内存紧张时，积压超过 1/4 的待发队列在入队处直接丢新帧，不让它在两次检测之间继续涨。

var (
	memLimit     atomic.Int64
	memHard      atomic.Int64
	memCritical  atomic.Int64
	memDropped   atomic.Int64
	memGuardOnce sync.Once
	memLastWarn  atomic.Int64
	memPressure  atomic.Bool
	memTick      = 100 * time.Millisecond
)

var memGuardOff atomic.Bool

// DisableMemoryGuard 关掉内存保护。要在 StartSocketService 之前调用。
func DisableMemoryGuard() { memGuardOff.Store(true) }

// MemoryGuardLimit 返回当前生效的上限（字节），没开启时为 0。
func MemoryGuardLimit() int64 { return memLimit.Load() }

// autoMemoryGuard 在 Manager 启动时调用：没有手动设置过上限，就按机器自动取
// （容器限制、GOMEMLIMIT、物理内存三者中最小的）。
func autoMemoryGuard() {
	if memGuardOff.Load() || memLimit.Load() > 0 {
		return
	}
	limit, source := socket.DetectMemoryLimit()
	if limit <= 0 {
		logManager("WARN", "memory guard: could not detect the memory limit, guard is off; call EnableMemoryGuard to turn it on")
		return
	}
	enableMemoryGuardBytes(limit, 0, 0)
	logManager("INFO", fmt.Sprintf("memory guard: limit %dMB from %s", limit>>20, source))
}

// EnableMemoryGuard 用指定的上限打开内存保护，覆盖自动取的值。limitMB 不大于 0 时什么都不做。
// hardPercent、criticalPercent 不在 1～100 之间时用默认的 85、95。
func EnableMemoryGuard(limitMB, hardPercent, criticalPercent int) {
	if limitMB <= 0 {
		return
	}
	limit := int64(limitMB) << 20
	if detected, from := socket.DetectMemoryLimit(); detected > 0 && limit > detected {
		logManager("WARN", fmt.Sprintf("memory guard: configured limit %dMB is above what this machine allows (%dMB from %s); using the configured value",
			limit>>20, detected>>20, from))
	}
	enableMemoryGuardBytes(limit, hardPercent, criticalPercent)
}

func enableMemoryGuardBytes(limit int64, hardPercent, criticalPercent int) {
	if memGuardOff.Load() || limit <= 0 {
		return
	}
	if hardPercent <= 0 || hardPercent > 100 {
		hardPercent = 85
	}
	if criticalPercent <= 0 || criticalPercent > 100 {
		criticalPercent = 95
	}
	memLimit.Store(limit)
	memHard.Store(limit * int64(hardPercent) / 100)
	memCritical.Store(limit * int64(criticalPercent) / 100)
	memGuardOnce.Do(func() { go memGuardLoop() })
}

// MemoryGuardDropped 返回因内存保护丢掉的待发消息数。
func MemoryGuardDropped() int64 { return memDropped.Load() }

func memGuardLoop() {
	samples := []metrics.Sample{
		{Name: "/gc/heap/live:bytes"},
		{Name: "/memory/classes/heap/objects:bytes"},
	}
	ticker := time.NewTicker(memTick)
	defer ticker.Stop()
	for range ticker.C {
		func() {
			defer func() {
				if err := recover(); err != nil {
					fmt.Printf("PANIC: manager memory guard recovered: %v\n", err)
				}
			}()
			metrics.Read(samples)
			var live, heap int64
			if samples[0].Value.Kind() == metrics.KindUint64 {
				live = int64(samples[0].Value.Uint64())
			}
			if samples[1].Value.Kind() == metrics.KindUint64 {
				heap = int64(samples[1].Value.Uint64())
			}
			// 按存活对象判断；当前堆含垃圾，平时会周期性涨到存活对象的两倍左右，只在逼近 critical 时才算上它。
			pressure := live >= memHard.Load() || heap >= memCritical.Load()
			memPressure.Store(pressure)
			if !pressure {
				return
			}
			critical := live >= memCritical.Load()
			dropped := memTrimOutboxes(critical)
			if dropped == 0 {
				return
			}
			memDropped.Add(int64(dropped))
			now := time.Now().UnixNano()
			if prev := memLastWarn.Load(); now-prev > int64(5*time.Second) && memLastWarn.CompareAndSwap(prev, now) {
				logManager("WARN", fmt.Sprintf("memory guard: dropped %d queued frames to gateways (live=%dMB limit=%dMB critical=%t)",
					dropped, live>>20, memLimit.Load()>>20, critical))
			}
		}()
	}
}

// memTrimOutboxes 丢掉各台 Gateway 待发队列里的旧帧。同一条连接可能挂在几个键下，只处理一次。
func memTrimOutboxes(critical bool) int {
	seen := map[*connectBucket]struct{}{}
	dropped := 0
	ConnIndexTable.Range(func(_, value any) bool {
		c, _ := value.(*connectBucket)
		if c == nil {
			return true
		}
		if _, dup := seen[c]; dup {
			return true
		}
		seen[c] = struct{}{}
		c.mu.Lock()
		ch := c.outbox
		c.mu.Unlock()
		if ch == nil {
			return true
		}
		keep := cap(ch) / 4
		if critical {
			keep = 0
		} else if len(ch) <= keep {
			return true
		}
		for len(ch) > keep {
			select {
			case <-ch:
				dropped++
				gatewayDrops.Add(1)
			default:
				return true
			}
		}
		return true
	})
	return dropped
}
