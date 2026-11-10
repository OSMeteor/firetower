package gateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	pb "github.com/OSMeteor/firetower/grpc/manager"
	"github.com/OSMeteor/firetower/socket"
	"github.com/pelletier/go-toml"
)

func linkedPipe(t *testing.T) *socket.TcpClient {
	t.Helper()
	left, right := net.Pipe()
	t.Cleanup(func() {
		_ = left.Close()
		_ = right.Close()
	})
	return &socket.TcpClient{Conn: left}
}

func TestAudit_OneManagerIsAlwaysPicked(t *testing.T) {
	auditEnv(t)
	t.Cleanup(func() { setPool(nil) })
	only := &managerLink{name: "only", tcp: linkedPipe(t), rpc: &scriptedTopic{}, register: true, publish: true}
	setPool([]*managerLink{only})
	for _, topic := range []string{"a", "b", "room"} {
		if got := pickPublisher(topic); got != only {
			t.Fatalf("topic %s picked %#v", topic, got)
		}
	}
}

func TestAudit_HashStaysUntilThatManagerLeaves(t *testing.T) {
	auditEnv(t)
	t.Cleanup(func() { setPool(nil) })
	var links []*managerLink
	for _, name := range []string{"a", "b", "c"} {
		links = append(links, &managerLink{
			name: name, tcp: linkedPipe(t), rpc: &scriptedTopic{}, register: true, publish: true,
		})
	}
	setPool(links)
	first := map[string]string{}
	seen := map[string]int{}
	for i := 0; i < 40; i++ {
		topic := fmt.Sprintf("topic-%d", i)
		got := pickPublisher(topic)
		if got == nil {
			t.Fatal("no publisher")
		}
		first[topic] = got.name
		seen[got.name]++
		if again := pickPublisher(topic); again.name != got.name {
			t.Fatalf("topic %s moved from %s to %s while the set was unchanged", topic, got.name, again.name)
		}
	}
	if len(seen) < 2 {
		t.Fatalf("hash did not spread topics: %v", seen)
	}
	var drop *managerLink
	for _, link := range links {
		if seen[link.name] > 0 {
			drop = link
			break
		}
	}
	before := ManagerFlaps.Load()
	drop.publish = false
	var stayed int
	for topic, name := range first {
		got := pickPublisher(topic)
		if got == nil || got == drop {
			t.Fatalf("topic %s still targets a manager that left the publish set", topic)
		}
		if name != drop.name && got.name == name {
			stayed++
		}
	}
	if stayed == 0 {
		t.Fatal("every topic moved when one manager left")
	}
	if ManagerFlaps.Load() <= before {
		t.Fatal("a manager leaving the publish set did not count as a placement change")
	}
}

func TestAudit_RegisterOnlyManagerDoesNotReceivePublish(t *testing.T) {
	auditEnv(t)
	t.Cleanup(func() { setPool(nil) })
	reg := &managerLink{name: "reg", tcp: linkedPipe(t), rpc: &scriptedTopic{}, register: true, publish: false}
	pub := &managerLink{name: "pub", tcp: linkedPipe(t), rpc: &scriptedTopic{}, register: true, publish: true}
	setPool([]*managerLink{reg, pub})
	if got := pickPublisher("room"); got != pub {
		t.Fatalf("picked %v, want the publish member", got)
	}
}

func TestAudit_RegisterIsAllOrNothing(t *testing.T) {
	bucket := auditEnv(t)
	t.Cleanup(func() { setPool(nil) })
	var calls []string
	good := &scriptedTopic{sub: func(context.Context, *pb.SubscribeTopicRequest) (*pb.SubscribeTopicResponse, error) {
		calls = append(calls, "good")
		return &pb.SubscribeTopicResponse{}, nil
	}}
	bad := &scriptedTopic{sub: func(context.Context, *pb.SubscribeTopicRequest) (*pb.SubscribeTopicResponse, error) {
		calls = append(calls, "bad")
		return nil, errors.New("manager down")
	}}
	setPool([]*managerLink{
		{name: "good", tcp: linkedPipe(t), rpc: good, register: true, publish: true},
		{name: "bad", tcp: linkedPipe(t), rpc: bad, register: true, publish: true},
	})
	ft := auditTower(t, "pool", "user")
	ft.topic = map[string]bool{}
	if _, err := ft.bindTopic([]string{"pool-room"}); err == nil {
		t.Fatal("subscribe succeeded after one manager rejected it")
	}
	if len(calls) != 2 {
		t.Fatalf("register calls = %v, want both managers", calls)
	}
	bucket.mu.RLock()
	left := len(bucket.topicRelevance["pool-room"])
	bucket.mu.RUnlock()
	if left != 0 {
		t.Fatalf("local subscription remained: %d", left)
	}
}

func TestAudit_GetConnectNumUsesThePublishTarget(t *testing.T) {
	auditEnv(t)
	t.Cleanup(func() { setPool(nil) })
	nums := map[string]int64{"a": 2, "b": 9}
	var links []*managerLink
	for _, name := range []string{"a", "b"} {
		n := name
		links = append(links, &managerLink{
			name: n, tcp: linkedPipe(t), register: true, publish: true,
			rpc: &scriptedTopic{num: func(context.Context, *pb.GetConnectNumRequest) (*pb.GetConnectNumResponse, error) {
				return &pb.GetConnectNumResponse{Number: nums[n]}, nil
			}},
		})
	}
	setPool(links)
	ft := auditTower(t, "num", "user")
	got := ft.GetConnectNum("room")
	picked := pickPublisher("room")
	if picked == nil || got != nums[picked.name] {
		t.Fatalf("count = %d, publisher %v", got, picked)
	}
}

func TestAudit_OldConfigIsOneManagerAndListCanSplitRoles(t *testing.T) {
	oldTree, err := toml.Load(`
topicServiceAddr = "127.0.0.1:6666"
[grpc]
address = "127.0.0.1:6667"
`)
	if err != nil {
		t.Fatal(err)
	}
	old := managerSpecs(oldTree)
	if len(old) != 1 || old[0].name != "default" || !old[0].register || !old[0].publish {
		t.Fatalf("old keys = %+v", old)
	}
	listTree, err := toml.Load(`
[[manager]]
name = "new"
tcp = "127.0.0.1:7001"
grpc = "127.0.0.1:7002"
register = true
publish = false
`)
	if err != nil {
		t.Fatal(err)
	}
	list := managerSpecs(listTree)
	if len(list) != 1 || list[0].name != "new" || !list[0].register || list[0].publish {
		t.Fatalf("list = %+v", list)
	}
}
