package gateway

import (
	"context"
	"sync"
	"testing"
	"time"

	pb "github.com/OSMeteor/firetower/grpc/manager"
)

// 订阅的登记还在路上时连接关掉了。Close 那一刻看不到这次订阅，不会替它退订。
// bindTopic 回来后必须自己把 Manager 上的人数改回去，否则会多到下一轮对账。
func TestGuard_SubscribeRacingCloseCorrectsTheManagerCount(t *testing.T) {
	auditEnv(t)
	const topic = "guard-revert-room"
	entered := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	manager := map[string]int64{} // 模拟 Manager：每个 Topic 最后一次盖写的人数
	var once sync.Once
	storeTopicGrpc(&scriptedTopic{
		sub: func(ctx context.Context, in *pb.SubscribeTopicRequest) (*pb.SubscribeTopicResponse, error) {
			mu.Lock()
			for i, name := range in.Topic {
				if i < len(in.Count) {
					manager[name] = in.Count[i]
				}
			}
			mu.Unlock()
			once.Do(func() { close(entered) })
			<-release
			return &pb.SubscribeTopicResponse{}, nil
		},
		unsub: func(ctx context.Context, in *pb.UnSubscribeTopicRequest) (*pb.UnSubscribeTopicResponse, error) {
			mu.Lock()
			for i, name := range in.Topic {
				if i < len(in.Count) {
					manager[name] = in.Count[i]
				}
			}
			mu.Unlock()
			return &pb.UnSubscribeTopicResponse{}, nil
		},
	})
	server, _, cleanup := wsPair(t)
	defer cleanup()
	tower := BuildTower(server, "guard-revert")
	if tower == nil {
		t.Fatal("BuildTower returned nil")
	}
	done := make(chan error, 1)
	go func() {
		_, err := tower.bindTopic([]string{topic})
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("subscribe never reached the manager")
	}
	mu.Lock()
	during := manager[topic]
	mu.Unlock()
	if during != 1 {
		t.Fatalf("manager saw %d during the subscribe, want 1", during)
	}
	tower.Close() // 这时 t.topic 里还没有这个 Topic
	close(release)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("subscribe on a closed tower reported success")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("bindTopic did not return")
	}
	mu.Lock()
	after := manager[topic]
	mu.Unlock()
	if after != 0 {
		t.Fatalf("manager still counts %d subscriber(s) for a connection that is gone", after)
	}
	censusMu.Lock()
	left := census[topic].n
	censusMu.Unlock()
	if left != 0 {
		t.Fatalf("local census still counts %d", left)
	}
}
