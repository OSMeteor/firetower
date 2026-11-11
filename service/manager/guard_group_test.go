package manager

import (
	"context"
	"testing"

	pb "github.com/OSMeteor/firetower/grpc/manager"
)

// 房间分片后 Manager 上只有片键。业务后端和管理命令按房间名来查、来发时，要展开到各片，
// 并且同一台 Gateway 只收一帧。
func TestGuard_RoomNameReachesEveryShard(t *testing.T) {
	const room = "guard-group-room"
	svc := &topicGrpcService{}
	a := &connectBucket{conn: &recordConn{fakeConn: fakeConn{addr: fakeAddr("10.77.3.1:3001")}}, closeChan: make(chan struct{})}
	b := &connectBucket{conn: &recordConn{fakeConn: fakeConn{addr: fakeAddr("10.77.3.2:3002")}}, closeChan: make(chan struct{})}
	a.relation()
	b.relation()
	t.Cleanup(func() {
		a.close()
		b.close()
		topicMu.Lock()
		delete(deadKeys, "10.77.3.1:3001")
		delete(deadKeys, "10.77.3.2:3002")
		topicMu.Unlock()
	})
	if res, _ := svc.CheckTopicExist(context.Background(), &pb.CheckTopicExistRequest{Topic: room}); res.Ok {
		t.Fatal("empty room reported as existing")
	}
	guardSub(t, svc, "10.77.3.1:3001", room+groupSep+"0", 2, 1)
	guardSub(t, svc, "10.77.3.1:3001", room+groupSep+"5", 1, 2)
	guardSub(t, svc, "10.77.3.2:3002", room+groupSep+"5", 4, 1)
	guardSub(t, svc, "10.77.3.2:3002", room+groupSep+"255", 3, 2)

	if got := guardNum(t, svc, room); got != 10 {
		t.Fatalf("room count = %d, want the sum of its shards 10", got)
	}
	if res, _ := svc.CheckTopicExist(context.Background(), &pb.CheckTopicExistRequest{Topic: room}); !res.Ok {
		t.Fatal("grouped room reported as missing")
	}
	targets, ok := snapshotTargets(room)
	if !ok || len(targets) != 2 {
		t.Fatalf("room targets = %d (ok=%t), want each gateway exactly once", len(targets), ok)
	}
	if res, err := svc.Publish(context.Background(), &pb.PublishRequest{Topic: room, Data: []byte("x"), MessageId: "1", Source: "backend"}); err != nil || !res.Ok {
		t.Fatalf("publish by room name: %v", err)
	}
	// 片键本身和别的房间不受影响。
	if got := guardNum(t, svc, room+groupSep+"5"); got != 5 {
		t.Fatalf("shard count = %d, want 5", got)
	}
	if got := guardNum(t, svc, room+"x"); got != 0 {
		t.Fatalf("another room picked up the shards: %d", got)
	}
}
