package gateway

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/OSMeteor/firetower/grpc/manager"
	"github.com/OSMeteor/firetower/socket"
)

// Manager 刚拉起时 TCP 先通、gRPC 后通，第一次重放会失败。
// 失败后必须自己重试到成功，不能等 3 分钟一轮的对账。
func TestGuard_FailedReplayIsRetried(t *testing.T) {
	auditEnv(t)
	prev := setRetryDelays([]time.Duration{10 * time.Millisecond, 10 * time.Millisecond, 10 * time.Millisecond, 10 * time.Millisecond})
	t.Cleanup(func() { setRetryDelays(prev) })

	const topic = "guard-replay-room"
	noteSubscribe([]string{topic})
	t.Cleanup(func() { revertSubscribe([]string{topic}) })

	var calls atomic.Int32
	var mu sync.Mutex
	var finished bool
	rpc := &scriptedTopic{
		sub: func(ctx context.Context, in *pb.SubscribeTopicRequest) (*pb.SubscribeTopicResponse, error) {
			if calls.Add(1) <= 2 {
				return nil, errors.New("unavailable")
			}
			mu.Lock()
			if in.RoundEnd {
				finished = true
			}
			mu.Unlock()
			return &pb.SubscribeTopicResponse{}, nil
		},
	}
	tcp := &socket.TcpClient{}
	tcp.SetRouteKey("9-guardreplay-1")
	link := &managerLink{name: "guard", tcp: tcp, rpc: rpc, register: true, publish: true}
	setPool([]*managerLink{link})
	t.Cleanup(func() { setPool(nil); replaySlots.Delete(tcp) })

	replayLink(link)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		ok := finished
		mu.Unlock()
		if ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("replay was not retried to completion after %d call(s)", calls.Load())
}

// 正在重放时又来一次重放请求（比如刚好又重连了一次），这一次不能丢。
func TestGuard_ReplayRequestDuringReplayIsNotLost(t *testing.T) {
	auditEnv(t)
	const topic = "guard-replay-room-2"
	noteSubscribe([]string{topic})
	t.Cleanup(func() { revertSubscribe([]string{topic}) })

	var rounds atomic.Int32
	gate := make(chan struct{})
	entered := make(chan struct{}, 8)
	rpc := &scriptedTopic{
		sub: func(ctx context.Context, in *pb.SubscribeTopicRequest) (*pb.SubscribeTopicResponse, error) {
			if in.RoundBegin {
				select {
				case entered <- struct{}{}:
				default:
				}
				<-gate
			}
			if in.RoundEnd {
				rounds.Add(1)
			}
			return &pb.SubscribeTopicResponse{}, nil
		},
	}
	tcp := &socket.TcpClient{}
	tcp.SetRouteKey("9-guardreplay-2")
	link := &managerLink{name: "guard2", tcp: tcp, rpc: rpc, register: true, publish: true}
	t.Cleanup(func() { replaySlots.Delete(tcp) })

	done := make(chan struct{})
	go func() {
		defer close(done)
		replayLink(link)
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first replay never started")
	}
	replayLink(link) // 第一轮还卡在里面，这次请求只能留个标记
	close(gate)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("replay did not finish")
	}
	if got := rounds.Load(); got < 2 {
		t.Fatalf("the replay requested during a running replay was dropped: %d round(s) completed", got)
	}
}
