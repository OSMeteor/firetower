//go:build !linux

package main

// raiseFileLimit 在 Linux 以外不做事。返回 0 表示不限制连接数。
func raiseFileLimit() uint64 { return 0 }
