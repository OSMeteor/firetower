package manager

import (
	"context"
	"testing"

	pb "github.com/OSMeteor/firetower/grpc/manager"
	"github.com/OSMeteor/firetower/socket"
)

func TestAudit_ReconcileKeepsVersionsNewerThanTheSnapshot(t *testing.T) {
	svc := &topicGrpcService{}
	const ip = "10.8.8.8:9"
	if _, err := svc.SubscribeTopic(context.Background(), &pb.SubscribeTopicRequest{
		Topic: []string{"old-ep", "new-ep"}, Ip: ip, Count: []int64{1, 1}, Version: []uint64{1, 5},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		topicMu.Lock()
		forgetIPLocked(ip)
		topicMu.Unlock()
	})
	if _, err := svc.SubscribeTopic(context.Background(), &pb.SubscribeTopicRequest{
		Topic: []string{socket.EpochTopic}, Ip: ip, Count: []int64{0}, Version: []uint64{3},
		Round: 9, RoundBegin: true, RoundEnd: true,
	}); err != nil {
		t.Fatal(err)
	}
	if got := countTopic(t, svc, "old-ep"); got != 0 {
		t.Fatalf("topic older than the snapshot stayed at %d", got)
	}
	if got := countTopic(t, svc, "new-ep"); got != 1 {
		t.Fatalf("topic written after the snapshot was deleted, count %d", got)
	}
	topicMu.Lock()
	oldVer := subVersion[ip]["old-ep"]
	newVer := subVersion[ip]["new-ep"]
	topicMu.Unlock()
	if oldVer != 0 {
		t.Fatalf("version for the dropped topic stayed at %d", oldVer)
	}
	if newVer != 5 {
		t.Fatalf("version newer than the snapshot = %d", newVer)
	}
}

func TestAudit_FullOutboxIsCounted(t *testing.T) {
	bucket := &connectBucket{outbox: make(chan []byte, 1), closeChan: make(chan struct{})}
	before := gatewayDrops.Load()
	bucket.deliver([]byte("a"))
	bucket.deliver([]byte("b"))
	if gatewayDrops.Load() != before+1 {
		t.Fatalf("drops %d before %d", gatewayDrops.Load(), before)
	}
}
