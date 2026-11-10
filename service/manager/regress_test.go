package manager

import (
	"context"
	"fmt"
	"runtime"
	"testing"

	pb "github.com/OSMeteor/firetower/grpc/manager"
	"github.com/OSMeteor/firetower/socket"
)

func TestAudit_R03a_UnfinishedRoundKeepsTheMissingChunk(t *testing.T) {
	prev := currentLogger()
	setLogger(func(string, string) {})
	t.Cleanup(func() { setLogger(prev) })

	svc := &topicGrpcService{}
	const ip = "10.3.3.3:3"
	topics := []string{"r3a-a", "r3a-b", "r3a-c"}
	if _, err := svc.SubscribeTopic(context.Background(), &pb.SubscribeTopicRequest{
		Topic: topics, Ip: ip, Count: []int64{1, 1, 1}, Version: []uint64{1, 1, 1},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		topicMu.Lock()
		forgetIPLocked(ip)
		topicMu.Unlock()
	})
	if _, err := svc.SubscribeTopic(context.Background(), &pb.SubscribeTopicRequest{
		Topic: []string{"r3a-a"}, Ip: ip, Count: []int64{1}, Version: []uint64{1},
		Round: 1, RoundBegin: true,
	}); err != nil {
		t.Fatal(err)
	}
	for _, topic := range topics {
		if got := countTopic(t, svc, topic); got != 1 {
			t.Fatalf("%s count=%d after an unfinished round", topic, got)
		}
	}
}

func TestAudit_R03b_SubscribeBetweenSnapshotAndEndStays(t *testing.T) {
	prev := currentLogger()
	setLogger(func(string, string) {})
	t.Cleanup(func() { setLogger(prev) })

	svc := &topicGrpcService{}
	const ip = "10.3.3.4:4"
	if _, err := svc.SubscribeTopic(context.Background(), &pb.SubscribeTopicRequest{
		Topic: []string{"r3b-old"}, Ip: ip, Count: []int64{1}, Version: []uint64{1},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		topicMu.Lock()
		forgetIPLocked(ip)
		topicMu.Unlock()
	})
	if _, err := svc.SubscribeTopic(context.Background(), &pb.SubscribeTopicRequest{
		Topic: []string{"r3b-old"}, Ip: ip, Count: []int64{1}, Version: []uint64{1},
		Round: 7, RoundBegin: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SubscribeTopic(context.Background(), &pb.SubscribeTopicRequest{
		Topic: []string{"r3b-new"}, Ip: ip, Count: []int64{1}, Version: []uint64{5},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SubscribeTopic(context.Background(), &pb.SubscribeTopicRequest{
		Topic: []string{socket.EpochTopic}, Ip: ip, Count: []int64{0}, Version: []uint64{3},
		Round: 7, RoundEnd: true,
	}); err != nil {
		t.Fatal(err)
	}
	if got := countTopic(t, svc, "r3b-old"); got != 1 {
		t.Fatalf("snapshot topic count=%d", got)
	}
	if got := countTopic(t, svc, "r3b-new"); got != 1 {
		t.Fatalf("topic subscribed after the snapshot was deleted, count=%d", got)
	}
}

func TestAudit_R05_TopicChurnDoesNotKeepRoutes(t *testing.T) {
	prev := currentLogger()
	setLogger(func(string, string) {})
	t.Cleanup(func() { setLogger(prev) })

	const ip = "10.5.5.5:5"
	const total = 1000000
	svc := &topicGrpcService{}
	bucket := &connectBucket{routeKey: ip}
	t.Cleanup(func() { bucket.delRelation() })

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	for i := 0; i < total; i++ {
		topic := fmt.Sprintf("r5m-%d", i)
		ver := uint64(i + 1)
		if _, err := svc.SubscribeTopic(context.Background(), &pb.SubscribeTopicRequest{
			Topic: []string{topic}, Ip: ip, Count: []int64{1}, Version: []uint64{ver},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.UnSubscribeTopic(context.Background(), &pb.UnSubscribeTopicRequest{
			Topic: []string{topic}, Ip: ip, Count: []int64{0}, Version: []uint64{ver + total},
		}); err != nil {
			t.Fatal(err)
		}
		if (i+1)%100000 == 0 {
			bucket.delRelation()
		}
	}
	bucket.delRelation()

	left := 0
	topicRelevance.Range(func(key, _ any) bool {
		if topic, ok := key.(string); ok && len(topic) >= 4 && topic[:4] == "r5m-" {
			left++
		}
		return true
	})
	if left != 0 {
		t.Fatalf("manager still has %d routes", left)
	}
	topicMu.Lock()
	versions := len(subVersion[ip])
	topicMu.Unlock()
	if versions != 0 {
		t.Fatalf("manager still has %d versions for a closed connection", versions)
	}
	runtime.GC()
	var after runtime.MemStats
	if after.HeapAlloc > before.HeapAlloc+64<<20 {
		t.Fatalf("manager heap grew from %d to %d", before.HeapAlloc, after.HeapAlloc)
	}
}

func TestAudit_R08_UnsubscribeRemovesTheRoute(t *testing.T) {
	prev := currentLogger()
	setLogger(func(string, string) {})
	t.Cleanup(func() { setLogger(prev) })

	svc := &topicGrpcService{}
	const ip = "10.8.8.1:8"
	const topic = "a"
	if _, err := svc.SubscribeTopic(context.Background(), &pb.SubscribeTopicRequest{
		Topic: []string{topic}, Ip: ip, Count: []int64{1}, Version: []uint64{1},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		topicMu.Lock()
		forgetIPLocked(ip)
		topicMu.Unlock()
	})
	if _, err := svc.UnSubscribeTopic(context.Background(), &pb.UnSubscribeTopicRequest{
		Topic: []string{topic}, Ip: ip, Count: []int64{0}, Version: []uint64{2},
	}); err != nil {
		t.Fatal(err)
	}
	if got := countTopic(t, svc, topic); got != 0 {
		t.Fatalf("count=%d", got)
	}
	if _, ok := topicRelevance.Load(topic); ok {
		t.Fatal("manager still has a route for a")
	}
}
