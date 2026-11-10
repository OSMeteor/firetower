package socket

import (
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 阶段 0：协议和连接入口不能把进程打崩。功能对错由 audit 用例负责。

func TestStage0_FrameEdgesDoNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("frame edge panicked: %v", r)
		}
	}()

	valid, err := Enpack("push", "1", "src", "topic", []byte("hi"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Enpack("", "1", "src", "topic", []byte("hi")); err == nil {
		t.Fatal("empty type was accepted")
	}
	if _, err = Enpack("push", "1", "src", "", []byte("hi")); err == nil {
		t.Fatal("empty topic was accepted")
	}
	if _, err = Enpack("push", "1", "src", "topic", nil); err == nil {
		t.Fatal("nil content was accepted")
	}

	cases := [][]byte{
		nil,
		{},
		[]byte("FireHeader"),
		append([]byte(ConstHeader), IntToBytes(0)...),
		append([]byte(ConstHeader), IntToBytes(-1)...),
		append([]byte(ConstHeader), IntToBytes(MaxFrameLength+1)...),
		append(append([]byte(ConstHeader), IntToBytes(len("onlytype\n"))...), []byte("onlytype\n")...),
		append(append([]byte(nil), valid...), valid...),
	}
	full := make(chan *SendMessage, 1)
	if _, err = Depack(append(append([]byte(nil), valid...), valid...), full); err == nil && len(full) == 1 {
		// 第二条因队列满被拒绝。这里只要求返回，不要求把第二条留下。
	}
	for _, packet := range cases {
		ch := make(chan *SendMessage, 2)
		if _, err = Depack(packet, ch); err != nil && len(ch) != 0 {
			t.Fatalf("rejected frame still queued a message: %v", err)
		}
		if _, err = Depack(packet, nil); err != nil && len(packet) == 0 {
			continue
		}
	}
	if _, err = Depack(valid, nil); err == nil {
		t.Fatal("nil result channel accepted a frame")
	}

	var cli *TcpClient
	if err = cli.Publish("1", "user", "topic", []byte("hi")); err == nil {
		t.Fatal("nil client Publish returned success")
	}
	bare := &TcpClient{}
	if err = bare.Publish("1", "user", "topic", []byte("hi")); err == nil {
		t.Fatal("client without a send queue returned success")
	}
}

func TestStage0_ConcurrentDepackDoesNotPanic(t *testing.T) {
	valid, err := Enpack("push", "1", "src", "topic", []byte("body"))
	if err != nil {
		t.Fatal(err)
	}
	var failed atomic.Bool
	var wg sync.WaitGroup
	seeds := [][]byte{valid, []byte(ConstHeader), append([]byte(ConstHeader), IntToBytes(-3)...)}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			defer func() {
				if recover() != nil {
					failed.Store(true)
				}
			}()
			local := rand.New(rand.NewSource(int64(seed)))
			for n := 0; n < 40; n++ {
				buf := append([]byte(nil), seeds[local.Intn(len(seeds))]...)
				if local.Intn(2) == 0 {
					buf = append(buf, byte(local.Intn(256)))
				}
				ch := make(chan *SendMessage, 2)
				_, _ = Depack(buf, ch)
			}
		}(i + 1)
	}
	wg.Wait()
	if failed.Load() {
		t.Fatal("concurrent Depack panicked")
	}
}

func TestStage0_PoolConcurrentDoesNotPanic(t *testing.T) {
	prev := SendLogger
	SendLogger = func(*SendMessage, string, string) {}
	t.Cleanup(func() { SendLogger = prev })

	var failed atomic.Bool
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if recover() != nil {
					failed.Store(true)
				}
			}()
			for n := 0; n < 200; n++ {
				msg := GetSendMessage("id", "src")
				msg.Type = "publish"
				msg.Topic = "room"
				msg.Data = []byte("payload")
				msg.Info("stage0")
				msg.Error("stage0")
				msg.Recycling()
			}
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pool workers did not finish")
	}
	if failed.Load() {
		t.Fatal("pooled SendMessage panicked")
	}
}
