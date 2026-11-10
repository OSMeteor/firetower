package gateway

import (
	"fmt"
	"sync"
	"time"
)

// warnHold 是水位要连续越过这么久才打日志。测试可以把这个值改成 0。
var warnHold = 10 * time.Second

type warnState struct {
	since time.Time
	last  time.Time
}

var (
	warnMu     sync.Mutex
	warnStates = map[string]warnState{}
)

func warnCapacity(name string, value, line int64, action string) {
	if name == "" || line <= 0 {
		return
	}
	now := time.Now()
	warnMu.Lock()
	st := warnStates[name]
	if value < line {
		st.since = time.Time{}
		warnStates[name] = st
		warnMu.Unlock()
		return
	}
	if st.since.IsZero() {
		st.since = now
	}
	due := now.Sub(st.since) >= warnHold && (st.last.IsZero() || now.Sub(st.last) >= time.Minute)
	if due {
		st.last = now
	}
	warnStates[name] = st
	warnMu.Unlock()
	if !due {
		return
	}
	callTowerLogger(nil, "WARN", fmt.Sprintf("%s value=%d line=%d action=%s", name, value, line, action))
}
