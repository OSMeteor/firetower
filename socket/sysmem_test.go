package socket

import (
	"runtime"
	"testing"
)

func TestGuard_DetectMemoryLimitFindsSomething(t *testing.T) {
	v, src := DetectMemoryLimit()
	switch runtime.GOOS {
	case "linux", "windows", "darwin":
		if v <= 0 {
			t.Fatalf("could not detect a memory limit on %s", runtime.GOOS)
		}
	}
	t.Logf("limit=%dMB from %s", v>>20, src)
	if phys := physicalMemory(); phys > 0 && v > phys {
		t.Fatalf("limit %d is above physical memory %d", v, phys)
	}
}
