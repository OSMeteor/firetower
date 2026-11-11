package socket

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func physicalMemory() int64 {
	return meminfoTotal("/proc/meminfo")
}

func meminfoTotal(path string) int64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kb, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil || kb <= 0 {
				return 0
			}
			return kb * 1024
		}
	}
	return 0
}

func containerMemoryLimit() int64 {
	return cgroupMemoryLimit("/proc/self/cgroup", "/sys/fs/cgroup")
}

// cgroupMemoryLimit 按 /proc/self/cgroup 找到本进程所在的 cgroup，读它的内存上限。
// 没有限制（"max"，或者 v1 里接近 int64 最大值的数）时返回 0。
func cgroupMemoryLimit(selfCgroup, root string) int64 {
	data, err := os.ReadFile(selfCgroup)
	if err != nil {
		return 0
	}
	var candidates []string
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), ":", 3)
		if len(parts) != 3 {
			continue
		}
		path := parts[2]
		switch {
		case parts[0] == "0" && parts[1] == "": // v2
			candidates = append(candidates,
				filepath.Join(root, path, "memory.max"),
				filepath.Join(root, "memory.max"))
		case hasController(parts[1], "memory"): // v1
			candidates = append(candidates,
				filepath.Join(root, "memory", path, "memory.limit_in_bytes"),
				filepath.Join(root, "memory", "memory.limit_in_bytes"))
		}
	}
	for _, p := range candidates {
		if v := readCgroupValue(p); v > 0 {
			return v
		}
	}
	return 0
}

func hasController(list, name string) bool {
	for _, c := range strings.Split(list, ",") {
		if c == name {
			return true
		}
	}
	return false
}

func readCgroupValue(path string) int64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	s := strings.TrimSpace(string(data))
	if s == "" || s == "max" {
		return 0
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v <= 0 {
		return 0
	}
	// v1 没有限制时是一个接近 int64 最大值、按页对齐的数。
	if v >= 1<<62 {
		return 0
	}
	return v
}
