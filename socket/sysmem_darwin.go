package socket

import (
	"encoding/binary"
	"syscall"
)

func physicalMemory() int64 {
	s, err := syscall.Sysctl("hw.memsize")
	if err != nil {
		return 0
	}
	b := []byte(s)
	// syscall.Sysctl 会去掉末尾的 0 字节，这里补回 8 字节再按小端读。
	for len(b) < 8 {
		b = append(b, 0)
	}
	v := binary.LittleEndian.Uint64(b[:8])
	if v == 0 || v > 1<<62 {
		return 0
	}
	return int64(v)
}

func containerMemoryLimit() int64 { return 0 }
