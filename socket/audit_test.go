package socket

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"runtime/debug"
	"sync/atomic"
	"testing"
	"time"
)

func TestAudit_CO03_PoolGetClearsPayload(t *testing.T) {
	prev := debug.SetGCPercent(-1)
	t.Cleanup(func() { debug.SetGCPercent(prev) })

	for i := 0; i < 20; i++ {
		msg := GetSendMessage("id", "user")
		msg.Type = "publish"
		msg.Topic = "room"
		msg.Data = []byte("secret")
		msg.MessageType = 2
		msg.Recycling()

		next := GetSendMessage("next", "platform")
		if next.Type != "" || next.Topic != "" || len(next.Data) != 0 || next.MessageType != 0 {
			t.Fatalf("CO03: reused message kept Type=%q Topic=%q Data=%q MessageType=%d", next.Type, next.Topic, next.Data, next.MessageType)
		}
		if next.Context == nil || next.Context.Id != "next" || next.Context.Source != "platform" {
			t.Fatalf("CO03: context was not reset: %+v", next.Context)
		}
	}
}

func TestAudit_PR01_NegativeLengthDoesNotPanic(t *testing.T) {
	packet := append([]byte(ConstHeader), IntToBytes(-1)...)
	ch := make(chan *SendMessage, 1)
	defer func() {
		if recover() != nil {
			t.Fatal("PR01: Depack panicked on a negative length")
		}
	}()
	if _, err := Depack(packet, ch); err == nil {
		t.Fatal("PR01: negative length was accepted")
	}
}

func TestAudit_PR01_OversizedDeclaredLengthIsRejected(t *testing.T) {
	const maxFrame = 1 << 20
	packet := append([]byte(ConstHeader), IntToBytes(maxFrame+1)...)
	ch := make(chan *SendMessage, 1)
	if _, err := Depack(packet, ch); err == nil {
		t.Fatal("PR01: declared length above 1MiB was treated as an incomplete frame")
	}
}

func TestAudit_PR01_DepackDoesNotBlockWhenConsumerIsGone(t *testing.T) {
	packet, err := Enpack("push", "1", "src", "topic", []byte("hi"))
	if err != nil {
		t.Fatal(err)
	}
	ch := make(chan *SendMessage) // 无人接收
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = Depack(packet, ch)
	}()
	select {
	case <-done:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("PR01: Depack blocked with no way to cancel when the result channel cannot accept")
	}
}

func TestAudit_PR02_GarbagePrefixStickyAndHalfPacket(t *testing.T) {
	packet, err := Enpack("push", "9", "src", "topic", []byte("body"))
	if err != nil {
		t.Fatal(err)
	}
	var stream []byte
	stream = append(stream, []byte("junk-prefix")...)
	stream = append(stream, packet...)
	stream = append(stream, packet...)
	stream = append(stream, packet[:len(packet)-3]...)

	ch := make(chan *SendMessage, 4)
	overflow, err := Depack(stream, ch)
	if err != nil {
		t.Fatalf("PR02: %v", err)
	}
	if len(ch) != 2 {
		t.Fatalf("PR02: got %d messages, want 2", len(ch))
	}
	if !bytes.Equal(overflow, packet[:len(packet)-3]) {
		t.Fatalf("PR02: overflow = %q", overflow)
	}
	msg := <-ch
	if msg.Type != "push" || msg.Topic != "topic" || string(msg.Data) != "body" {
		t.Fatalf("PR02: decoded %+v data=%q", msg, msg.Data)
	}
}

func FuzzDepack(f *testing.F) {
	valid, err := Enpack("push", "1", "src", "topic", []byte("body"))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(valid)
	f.Add([]byte(ConstHeader))
	f.Add(append([]byte(ConstHeader), IntToBytes(-1)...))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<16 {
			data = data[:1<<16]
		}
		ch := make(chan *SendMessage, 8)
		stop := make(chan struct{})
		go func() {
			for {
				select {
				case <-ch:
				case <-stop:
					return
				}
			}
		}()
		defer close(stop)
		_, _ = Depack(data, ch)
	})
}

func TestAudit_TC01_ShutdownUnblocksRead(t *testing.T) {
	left, right := net.Pipe()
	t.Cleanup(func() {
		_ = left.Close()
		_ = right.Close()
	})
	cli := NewClient("127.0.0.1:1")
	cli.Conn = left
	cli.closeChan = make(chan struct{})

	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- errPanic(r)
			}
		}()
		_, err := cli.Read()
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	cli.Shutdown()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("TC01: Read returned nil error after Shutdown")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("TC01: Read was still blocked 500ms after Shutdown")
	}
}

func TestAudit_TC01_ShutdownStopsOnPush(t *testing.T) {
	left, right := net.Pipe()
	t.Cleanup(func() {
		_ = left.Close()
		_ = right.Close()
	})
	cli := NewClient("127.0.0.1:1")
	cli.Conn = left
	cli.closeChan = make(chan struct{})
	cli.OnPush(func(*SendMessage) {})
	time.Sleep(50 * time.Millisecond)
	running := runtime.NumGoroutine()
	cli.Shutdown()

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() < running {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("TC01: OnPush was still running after Shutdown")
}

func TestAudit_TC02_StaleReadDoesNotCloseNewConn(t *testing.T) {
	previous := dialTCP
	t.Cleanup(func() { dialTCP = previous })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	accepted := make(chan net.Conn, 4)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- conn
		}
	}()

	releaseOldRead := make(chan struct{})
	var dials atomic.Int32
	dialTCP = func(network, address string) (net.Conn, error) {
		conn, err := net.Dial(network, address)
		if err != nil {
			return nil, err
		}
		if dials.Add(1) == 1 {
			return holdConn{Conn: conn, release: releaseOldRead}, nil
		}
		return conn, nil
	}

	cli := NewClient(ln.Addr().String())
	if err := cli.Connect(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cli.Shutdown() })
	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("TC02: first connection was not accepted")
	}

	go cli.Close()
	var fresh net.Conn
	select {
	case fresh = <-accepted:
	case <-time.After(3 * time.Second):
		t.Fatal("TC02: reconnect did not produce a new connection")
	}
	waitUntil(t, 2*time.Second, "replacement connection became current", func() bool {
		cli.mutex.Lock()
		defer cli.mutex.Unlock()
		return !cli.isClose && cli.Conn != nil
	})
	close(releaseOldRead)
	time.Sleep(150 * time.Millisecond)

	_ = fresh.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	_, err = fresh.Read(make([]byte, 1))
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return
	}
	t.Fatalf("TC02: the previous read loop closed the new connection: %v", err)
}

func TestAudit_TC02_FlapDoesNotMultiplyGoroutines(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	cli := NewClient(ln.Addr().String())
	if err := cli.Connect(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cli.Shutdown() })
	time.Sleep(100 * time.Millisecond)
	base := runtime.NumGoroutine()
	time.Sleep(2 * time.Second)
	grown := runtime.NumGoroutine() - base
	cli.Shutdown()
	if grown > 15 {
		t.Fatalf("TC02: reconnect flap grew goroutines by %d", grown)
	}
}

func TestAudit_TC02_ShutdownStopsReconnect(t *testing.T) {
	previous := dialTCP
	var dials atomic.Int32
	dialTCP = func(network, address string) (net.Conn, error) {
		dials.Add(1)
		return nil, errors.New("offline")
	}
	t.Cleanup(func() { dialTCP = previous })

	cli := NewClient("127.0.0.1:1")
	go cli.Close()
	time.Sleep(50 * time.Millisecond)
	cli.Shutdown()
	marked := dials.Load()
	time.Sleep(1500 * time.Millisecond)
	if got := dials.Load(); got != marked {
		t.Fatalf("TC02: reconnect dialed after Shutdown: before=%d after=%d", marked, got)
	}
}

func TestAudit_NilClientAndFrameDoNotPanic(t *testing.T) {
	defer func() {
		if recover() != nil {
			t.Fatal("nil path panicked")
		}
	}()
	var cli *TcpClient
	cli.Close()
	cli.Shutdown()
	if _, err := cli.Read(); err == nil {
		t.Fatal("nil client Read returned success")
	}
	if err := cli.Connect(); err == nil {
		t.Fatal("nil client Connect returned success")
	}
	if _, err := Depack(nil, nil); err != nil {
		t.Fatal(err)
	}
	packet := append([]byte(ConstHeader), IntToBytes(-5)...)
	if _, err := Depack(packet, nil); err == nil {
		t.Fatal("negative frame was accepted")
	}
}

type holdConn struct {
	net.Conn
	release <-chan struct{}
}

func (c holdConn) Read([]byte) (int, error) {
	<-c.release
	return 0, io.EOF
}

func waitUntil(t *testing.T, d time.Duration, what string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("TC02: timed out waiting until %s", what)
}

type panicError struct{ value any }

func (e panicError) Error() string { return fmt.Sprintf("panic: %v", e.value) }

func errPanic(v any) error { return panicError{value: v} }
