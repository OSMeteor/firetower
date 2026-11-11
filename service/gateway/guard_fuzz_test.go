package gateway

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// FuzzClientFrame 把任意字节当成客户端的一帧发给一条真实的 WebSocket 连接。
// 要求：不 panic（读写协程里的 panic 会被接住，但这里会把它记成失败），也不卡住关闭。
func FuzzClientFrame(f *testing.F) {
	for _, seed := range []string{
		`{"type":"subscribe","topic":"a,b","data":{}}`,
		`{"type":"unSubscribe","topic":"a","data":{}}`,
		`{"type":"publish","topic":"a","data":{"x":1}}`,
		`{"type":"publish","topic":"_firetower_handshake","data":"1-x-1"}`,
		`{"type":"subscribe","topic":",,,","data":null}`,
		`{"type":"offline_user","topic":"a","data":"u"}`,
		`{"type":7,"topic":[1],"data":"x"}`,
		`null`, `[]`, `"x"`, `{`, ``,
	} {
		f.Add([]byte(seed), uint8(websocket.TextMessage))
	}
	f.Fuzz(func(t *testing.T, data []byte, kind uint8) {
		auditEnv(t)
		var panics atomic.Int32
		storeTowerLogger(func(_ *FireTower, level, _ string) {
			if level == "PANIC" {
				panics.Add(1)
			}
		})
		server, client, cleanup := wsPair(t)
		defer cleanup()
		tower := BuildTower(server, "fuzz")
		if tower == nil {
			t.Skip("no tower")
		}
		tower.UserId = "fuzz-user"
		tower.SetReadHandler(func(fire *FireInfo) bool {
			_ = tower.Publish(fire)
			return true
		})
		tower.SetSubscribeHandler(func(ctx *FireLife, topics []string) bool {
			for _, topic := range topics {
				_ = tower.GetConnectNum(topic)
			}
			return true
		})
		done := make(chan struct{})
		go func() {
			defer close(done)
			tower.Run()
		}()
		mt := websocket.TextMessage
		if kind%2 == 1 {
			mt = websocket.BinaryMessage
		}
		_ = client.SetWriteDeadline(time.Now().Add(time.Second))
		_ = client.WriteMessage(mt, data)
		// 同一条连接上再发一帧正常的，确认坏帧之后连接还能用（或者已经被干净地关掉）。
		_ = client.WriteMessage(websocket.TextMessage, []byte(`{"type":"subscribe","topic":"after","data":{}}`))
		time.Sleep(5 * time.Millisecond)
		_ = client.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("Run did not return after the client went away; frame %q", data)
		}
		if n := panics.Load(); n > 0 {
			t.Fatalf("frame %q caused %d recovered panic(s)", data, n)
		}
	})
}
