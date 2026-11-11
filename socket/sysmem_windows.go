package socket

import (
	"syscall"
	"unsafe"
)

type memoryStatusEx struct {
	length               uint32
	memoryLoad           uint32
	totalPhys            uint64
	availPhys            uint64
	totalPageFile        uint64
	availPageFile        uint64
	totalVirtual         uint64
	availVirtual         uint64
	availExtendedVirtual uint64
}

var procGlobalMemoryStatusEx = syscall.NewLazyDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx")

func physicalMemory() int64 {
	if procGlobalMemoryStatusEx.Find() != nil {
		return 0
	}
	var st memoryStatusEx
	st.length = uint32(unsafe.Sizeof(st))
	ok, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&st)))
	if ok == 0 || st.totalPhys == 0 || st.totalPhys > 1<<62 {
		return 0
	}
	return int64(st.totalPhys)
}

func containerMemoryLimit() int64 { return 0 }
