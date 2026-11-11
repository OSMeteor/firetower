package manager

import "testing"

// Logger 可以被业务替换，recover 里也会调用它。它自己 panic 时不能把进程带走。
func TestGuard_PanickingLoggerDoesNotEscape(t *testing.T) {
	prev := currentLogger()
	setLogger(func(string, string) { panic("manager logger bug") })
	t.Cleanup(func() { setLogger(prev) })

	var escaped interface{}
	func() {
		defer func() { escaped = recover() }()
		logManager("ERROR", "x")
		logPanic("guard", "boom")
		func() {
			defer recoverTopic("guard")
			panic("inner")
		}()
	}()
	if escaped != nil {
		t.Fatalf("logger panic escaped: %v", escaped)
	}
}
