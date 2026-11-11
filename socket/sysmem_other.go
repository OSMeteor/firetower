//go:build !linux && !windows && !darwin

package socket

func physicalMemory() int64       { return 0 }
func containerMemoryLimit() int64 { return 0 }
