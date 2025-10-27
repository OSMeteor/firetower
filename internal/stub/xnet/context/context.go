package context

import (
	stdctx "context"
	"time"
)

// Re-export the standard library context helpers to satisfy dependencies on the
// legacy golang.org/x/net/context package.
type Context = stdctx.Context

type CancelFunc = stdctx.CancelFunc

func Background() Context { return stdctx.Background() }
func TODO() Context       { return stdctx.TODO() }

func WithCancel(parent Context) (Context, CancelFunc) {
	return stdctx.WithCancel(parent)
}

func WithTimeout(parent Context, timeout time.Duration) (Context, CancelFunc) {
	return stdctx.WithTimeout(parent, timeout)
}

func WithDeadline(parent Context, d time.Time) (Context, CancelFunc) {
	return stdctx.WithDeadline(parent, d)
}

func WithValue(parent Context, key interface{}, val interface{}) Context {
	return stdctx.WithValue(parent, key, val)
}
