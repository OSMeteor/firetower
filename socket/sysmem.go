package socket

import (
	"math"
	"runtime/debug"
)

// DetectMemoryLimit 自动确定这个进程能用的内存上限（字节），以及它是从哪里来的。
// 取以下几项里最小的那个：
//   - 进程的 GOMEMLIMIT（只读取，不修改，不影响 GC）；
//   - 容器的内存限制（Linux cgroup v2 的 memory.max、v1 的 memory.limit_in_bytes）；
//   - 机器的物理内存。
//
// 都取不到时返回 0。
func DetectMemoryLimit() (int64, string) {
	best, source := int64(0), ""
	consider := func(v int64, from string) {
		if v <= 0 {
			return
		}
		if best == 0 || v < best {
			best, source = v, from
		}
	}
	// SetMemoryLimit 传负数只是读取当前值，不做任何修改。
	if v := debug.SetMemoryLimit(-1); v > 0 && v < math.MaxInt64 {
		consider(v, "GOMEMLIMIT")
	}
	consider(containerMemoryLimit(), "cgroup")
	consider(physicalMemory(), "physical")
	return best, source
}
