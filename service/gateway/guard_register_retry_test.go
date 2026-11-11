package gateway

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/OSMeteor/firetower/grpc/manager"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Manager 一时忙（登记在排队）导致超时时，订阅不能被悄悄丢掉：要重发，直到成功。
func TestGuard_SlowManagerDoesNotLoseTheSubscription(t *testing.T) {
	bucket := auditEnv(t)
	t.Cleanup(func() { setPool(nil) })
	var calls atomic.Int32
	slow := &scriptedTopic{sub: func(ctx context.Context, in *pb.SubscribeTopicRequest) (*pb.SubscribeTopicResponse, error) {
		switch calls.Add(1) {
		case 1:
			return nil, status.Error(codes.DeadlineExceeded, "queued")
		case 2:
			return nil, status.Error(codes.Unavailable, "reconnecting")
		}
		return &pb.SubscribeTopicResponse{}, nil
	}}
	setPool([]*managerLink{{name: "slow", tcp: linkedPipe(t), rpc: slow, register: true, publish: true}})
	ft := auditTower(t, "retry", "user")
	ft.topic = map[string]bool{}
	var failed bool
	ft.SetSubscribeFailHandler(func([]string, error) { failed = true })
	if _, err := ft.bindTopic([]string{"retry-room"}); err != nil {
		t.Fatalf("subscribe failed although the manager answered on the third try: %v", err)
	}
	if failed || calls.Load() != 3 {
		t.Fatalf("calls=%d failHandler=%t, want 3 calls and no failure", calls.Load(), failed)
	}
	bucket.mu.RLock()
	kept := len(bucket.topicRelevance["retry-room"])
	bucket.mu.RUnlock()
	if kept != 1 {
		t.Fatalf("local subscription = %d, want 1", kept)
	}
}

// Manager 明确拒绝时不重试，仍按原来的规则整次失败。
func TestGuard_RejectedRegistrationIsNotRetried(t *testing.T) {
	auditEnv(t)
	t.Cleanup(func() { setPool(nil) })
	var calls atomic.Int32
	bad := &scriptedTopic{sub: func(context.Context, *pb.SubscribeTopicRequest) (*pb.SubscribeTopicResponse, error) {
		calls.Add(1)
		return nil, errors.New("manager down")
	}}
	setPool([]*managerLink{{name: "bad", tcp: linkedPipe(t), rpc: bad, register: true, publish: true}})
	ft := auditTower(t, "noretry", "user")
	ft.topic = map[string]bool{}
	if _, err := ft.bindTopic([]string{"noretry-room"}); err == nil {
		t.Fatal("subscribe succeeded after the manager rejected it")
	}
	if calls.Load() != 1 {
		t.Fatalf("rejected registration was sent %d times", calls.Load())
	}
}

// 一直超时：重发有上限，不能把这条连接的读循环卡死；订阅留在本机，后台重放一直补到 Manager 收下为止。
func TestGuard_RegisterTimeoutKeepsSubscriptionAndReplays(t *testing.T) {
	bucket := auditEnv(t)
	t.Cleanup(func() { setPool(nil) })
	prevDelays := setRetryDelays([]time.Duration{10 * time.Millisecond, 10 * time.Millisecond})
	t.Cleanup(func() { setRetryDelays(prevDelays) })

	var calls, replayed atomic.Int32
	var answer atomic.Bool
	slow := &scriptedTopic{sub: func(ctx context.Context, in *pb.SubscribeTopicRequest) (*pb.SubscribeTopicResponse, error) {
		calls.Add(1)
		if !answer.Load() {
			return nil, status.Error(codes.DeadlineExceeded, "queued")
		}
		if in.Round > 0 && in.RoundEnd {
			replayed.Add(1)
		}
		return &pb.SubscribeTopicResponse{}, nil
	}}
	link := &managerLink{name: "slow", tcp: linkedPipe(t), rpc: slow, register: true, publish: true}
	setPool([]*managerLink{link})
	t.Cleanup(func() { replaySlots.Delete(link.tcp) })
	ft := auditTower(t, "kept", "user")
	ft.topic = map[string]bool{}
	start := time.Now()
	if _, err := ft.bindTopic([]string{"kept-room"}); err != nil {
		t.Fatalf("subscription was dropped because the manager was slow: %v", err)
	}
	if took := time.Since(start); took > registerBudget()+time.Second {
		t.Fatalf("subscribe blocked for %s", took)
	}
	if got := int(calls.Load()); got < len(registerTimeouts) {
		t.Fatalf("sent %d times before handing over, want %d", got, len(registerTimeouts))
	}
	bucket.mu.RLock()
	kept := len(bucket.topicRelevance["kept-room"])
	bucket.mu.RUnlock()
	if kept != 1 {
		t.Fatalf("local subscription = %d, want 1", kept)
	}
	// 一段时间后 Manager 恢复：后台重放要把整表补上，不能因为前面失败太多次就放弃。
	time.Sleep(200 * time.Millisecond)
	answer.Store(true)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && replayed.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if replayed.Load() == 0 {
		t.Fatal("background replay gave up before the manager recovered")
	}
}

// 退订超时：Close 不能因此变慢，Manager 上多出来的人数要很快被重放盖回去。
func TestGuard_FailedUnsubscribeIsCorrectedByReplay(t *testing.T) {
	auditEnv(t)
	t.Cleanup(func() { setPool(nil) })
	prev := setRetryDelays([]time.Duration{20 * time.Millisecond, 20 * time.Millisecond})
	t.Cleanup(func() { setRetryDelays(prev) })

	var mu sync.Mutex
	manager := map[string]int64{}
	var round map[string]bool // 和真的 Manager 一样：一轮对账里没出现的 Topic 清零
	var unsubCalls atomic.Int32
	rpc := &scriptedTopic{
		sub: func(ctx context.Context, in *pb.SubscribeTopicRequest) (*pb.SubscribeTopicResponse, error) {
			mu.Lock()
			defer mu.Unlock()
			if in.Round > 0 && in.RoundBegin {
				round = map[string]bool{}
			}
			for i, name := range in.Topic {
				if i < len(in.Count) {
					manager[name] = in.Count[i]
				}
				if round != nil {
					round[name] = true
				}
			}
			if in.Round > 0 && in.RoundEnd && round != nil {
				for name := range manager {
					if !round[name] {
						manager[name] = 0
					}
				}
				round = nil
			}
			return &pb.SubscribeTopicResponse{}, nil
		},
		unsub: func(ctx context.Context, in *pb.UnSubscribeTopicRequest) (*pb.UnSubscribeTopicResponse, error) {
			unsubCalls.Add(1)
			return nil, status.Error(codes.DeadlineExceeded, "slow")
		},
	}
	link := &managerLink{name: "m", tcp: linkedPipe(t), rpc: rpc, register: true, publish: true}
	setPool([]*managerLink{link})
	t.Cleanup(func() { replaySlots.Delete(link.tcp) })
	ft := auditTower(t, "unsub-replay", "user")
	ft.topic = map[string]bool{}
	if _, err := ft.bindTopic([]string{"unsub-room"}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	ft.Close()
	if took := time.Since(start); took > 2*managerRPCTimeout {
		t.Fatalf("Close took %s with a slow unsubscribe", took)
	}
	if unsubCalls.Load() != 1 {
		t.Fatalf("unsubscribe sent %d times, want 1", unsubCalls.Load())
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n, seen := manager["unsub-room"]
		mu.Unlock()
		if seen && n == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Fatalf("manager still counts %d for a connection that has gone", manager["unsub-room"])
}
