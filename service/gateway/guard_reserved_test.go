package gateway

import (
	"context"
	"strings"
	"sync"
	"testing"

	pb "github.com/OSMeteor/firetower/grpc/manager"
	"github.com/OSMeteor/firetower/socket"
)

// 握手和对账用的两个保留 Topic，客户端不能订阅也不能发布。
// 放过去的话，一条 WebSocket 连接发一帧就能把这台 Gateway 在 Manager 上的路由键改掉。
func TestGuard_ReservedTopicsAreRefused(t *testing.T) {
	auditEnv(t)
	var mu sync.Mutex
	var seen []string
	storeTopicGrpc(&scriptedTopic{
		sub: func(ctx context.Context, in *pb.SubscribeTopicRequest) (*pb.SubscribeTopicResponse, error) {
			mu.Lock()
			seen = append(seen, in.Topic...)
			mu.Unlock()
			return &pb.SubscribeTopicResponse{}, nil
		},
	})
	server, _, cleanup := wsPair(t)
	defer cleanup()
	tower := BuildTower(server, "guard-reserved")
	if tower == nil {
		t.Fatal("BuildTower returned nil")
	}
	defer tower.Close()
	added, err := tower.bindTopic([]string{socket.HandshakeTopic, "room", socket.EpochTopic})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if len(added) != 1 || added[0] != "room" {
		t.Fatalf("subscribed %v, want only room", added)
	}
	mu.Lock()
	joined := strings.Join(seen, ",")
	mu.Unlock()
	if strings.Contains(joined, "_firetower_") {
		t.Fatalf("a reserved topic was registered on the manager: %s", joined)
	}
	for _, name := range []string{socket.HandshakeTopic, socket.EpochTopic} {
		fire := NewFireInfo(tower, nil)
		fire.Message.Topic = name
		fire.Message.Data = []byte(`"1-x-1"`)
		if err := tower.Publish(fire); err == nil {
			t.Fatalf("publish to %s was accepted", name)
		}
	}
}
