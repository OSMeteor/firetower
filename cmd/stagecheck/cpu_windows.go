//go:build windows

package main

import "syscall"

// osCPUSeconds 用 GetProcessTimes 取本进程的用户态加内核态 CPU 秒数。读不到返回 -1。
func osCPUSeconds() float64 {
	var c, e, k, u syscall.Filetime
	h, err := syscall.GetCurrentProcess()
	if err != nil {
		return -1
	}
	if err := syscall.GetProcessTimes(h, &c, &e, &k, &u); err != nil {
		return -1
	}
	ticks := func(f syscall.Filetime) float64 { return float64(uint64(f.HighDateTime)<<32 | uint64(f.LowDateTime)) }
	return (ticks(k) + ticks(u)) / 1e7 // 100ns 为单位
}
