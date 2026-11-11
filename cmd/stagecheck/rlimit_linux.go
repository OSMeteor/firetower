//go:build linux

package main

import "syscall"

// raiseFileLimit 把本进程的文件句柄软上限提到硬上限，子进程会继承。返回提高后的值。
func raiseFileLimit() uint64 {
	var r syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &r); err != nil {
		return 0
	}
	if r.Cur < r.Max {
		r.Cur = r.Max
		_ = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &r)
		_ = syscall.Getrlimit(syscall.RLIMIT_NOFILE, &r)
	}
	return r.Cur
}
