//go:build linux

package main

import (
	"os"
	"strconv"
	"strings"
)

// osCPUSeconds 读 /proc/self/stat 里本进程的用户态加内核态 CPU 秒数。读不到返回 -1。
func osCPUSeconds() float64 {
	b, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return -1
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return -1
	}
	f := strings.Fields(s[i+1:])
	// ) 之后第 1 个字段是 state（原第 3 个），utime、stime 是原第 14、15 个。
	if len(f) < 13 {
		return -1
	}
	ut, err1 := strconv.ParseFloat(f[11], 64)
	st, err2 := strconv.ParseFloat(f[12], 64)
	if err1 != nil || err2 != nil {
		return -1
	}
	return (ut + st) / 100 // USER_HZ 在 Linux 上几乎总是 100
}
