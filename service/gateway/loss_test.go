package gateway

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	pb "github.com/OSMeteor/firetower/grpc/manager"
	"github.com/OSMeteor/firetower/socket"
)

func TestAudit_OnlyWaitingManagerStillPublishes(t *testing.T) {
	auditEnv(t)
	t.Cleanup(func() { setPool(nil) })
	only := linkedPipe(t)
	only.ApplyHeartbeat("wait")
	setPool([]*managerLink{{name: "only", tcp: only, rpc: &scriptedTopic{}, register: true, publish: true}})
	if pickPublisher("room") == nil {
		t.Fatal("the only manager was skipped while it was waiting")
	}
	ready := linkedPipe(t)
	ready.ApplyHeartbeat("ready")
	waiting := linkedPipe(t)
	waiting.ApplyHeartbeat("wait")
	setPool([]*managerLink{
		{name: "wait", tcp: waiting, rpc: &scriptedTopic{}, register: true, publish: true},
		{name: "ready", tcp: ready, rpc: &scriptedTopic{}, register: true, publish: true},
	})
	if got := pickPublisher("room"); got == nil || got.name != "ready" {
		t.Fatalf("picked %#v", got)
	}
}

func TestAudit_DuplicateTopicCountsOnce(t *testing.T) {
	auditEnv(t)
	t.Cleanup(func() {
		censusMu.Lock()
		delete(census, "dup-room")
		censusMu.Unlock()
	})
	var calls int
	storeTopicGrpc(&scriptedTopic{
		sub: func(_ context.Context, in *pb.SubscribeTopicRequest) (*pb.SubscribeTopicResponse, error) {
			calls++
			if len(in.Topic) != 1 || in.Topic[0] != "dup-room" || len(in.Count) != 1 || in.Count[0] != 1 {
				return nil, fmt.Errorf("registered %v counts %v", in.Topic, in.Count)
			}
			return &pb.SubscribeTopicResponse{}, nil
		},
	})
	added, err := (&FireTower{ClientId: "dup", UserId: "dup"}).bindTopic([]string{"dup-room", "dup-room"})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(added) != 1 || added[0] != "dup-room" {
		t.Fatalf("calls=%d added=%v", calls, added)
	}
}

func TestAudit_HolderFollowsTheBucketLock(t *testing.T) {
	const topic = "holder-race"
	b := &Bucket{}
	t.Cleanup(func() {
		holderMu.Lock()
		delete(holders, topic)
		holderMu.Unlock()
	})
	for i := 0; i < 200; i++ {
		old := &FireTower{ClientId: fmt.Sprintf("old-%d", i)}
		neu := &FireTower{ClientId: fmt.Sprintf("new-%d", i)}
		b.AddSubscribe(topic, old)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			b.DelSubscribe(topic, old)
		}()
		go func() {
			defer wg.Done()
			b.AddSubscribe(topic, neu)
		}()
		wg.Wait()
		b.mu.RLock()
		has := b.topicRelevance[topic][neu.ClientId] == neu
		b.mu.RUnlock()
		listed := false
		for _, item := range bucketsFor(topic) {
			if item == b {
				listed = true
			}
		}
		if has != listed {
			t.Fatalf("membership %v holder %v", has, listed)
		}
		b.DelSubscribe(topic, old)
		b.DelSubscribe(topic, neu)
	}
}

func TestAudit_ReplayStopsBeforeTheEndWhenAChunkFails(t *testing.T) {
	auditEnv(t)
	noteSubscribe([]string{"keep-replay", "boom-replay"})
	t.Cleanup(func() {
		noteUnsubscribe([]string{"keep-replay", "boom-replay"})
	})
	var ends int
	storeTopicGrpc(&scriptedTopic{
		sub: func(_ context.Context, in *pb.SubscribeTopicRequest) (*pb.SubscribeTopicResponse, error) {
			for _, topic := range in.Topic {
				if topic == "boom-replay" {
					return nil, errors.New("boom")
				}
			}
			if in.RoundEnd {
				ends++
			}
			return &pb.SubscribeTopicResponse{}, nil
		},
	})
	replayLocalTopics()
	if ends != 0 {
		t.Fatal("a failed replay still closed the round")
	}
}

func TestAudit_EmptyCensusDoesNotStay(t *testing.T) {
	auditEnv(t)
	noteSubscribe([]string{"temp-census"})
	noteUnsubscribe([]string{"temp-census"})
	censusMu.Lock()
	_, ok := census["temp-census"]
	censusMu.Unlock()
	if ok {
		t.Fatal("a topic with nobody left stayed in the census")
	}
}

func TestAudit_FullCentralChanIsCounted(t *testing.T) {
	auditEnv(t)
	storeTM(&TowerManager{centralChan: make(chan *socket.SendMessage, 1)})
	forwardPush(&socket.SendMessage{Topic: "a"})
	before := centralDrops.Load()
	forwardPush(&socket.SendMessage{Topic: "b"})
	if centralDrops.Load() != before+1 {
		t.Fatalf("drops %d, before %d", centralDrops.Load(), before)
	}
}
