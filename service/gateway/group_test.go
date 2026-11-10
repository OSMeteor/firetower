package gateway

import (
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	pb "github.com/OSMeteor/firetower/grpc/manager"
	"github.com/OSMeteor/firetower/socket"
	"github.com/pelletier/go-toml"
)

func TestAudit_UngroupedTopicIsOneKey(t *testing.T) {
	auditEnv(t)
	if registerKey("room", "u") != "room" {
		t.Fatal("default grouping changed the topic")
	}
	keys := publishKeys("room")
	if len(keys) != 1 || keys[0] != "room" {
		t.Fatalf("publish keys %v", keys)
	}
}

func TestAudit_GroupRegistersOneShard(t *testing.T) {
	bucket := auditEnv(t)
	SetTopicShards("live-group", 4)
	var subscribed, unsubscribed []string
	storeTopicGrpc(&scriptedTopic{
		sub: func(_ context.Context, in *pb.SubscribeTopicRequest) (*pb.SubscribeTopicResponse, error) {
			subscribed = append(subscribed, in.Topic...)
			return &pb.SubscribeTopicResponse{}, nil
		},
		unsub: func(_ context.Context, in *pb.UnSubscribeTopicRequest) (*pb.UnSubscribeTopicResponse, error) {
			unsubscribed = append(unsubscribed, in.Topic...)
			return &pb.UnSubscribeTopicResponse{}, nil
		},
	})
	user := userInShard(t, "live-group", 0)
	otherID := userInShard(t, "live-group", 1)
	ft := &FireTower{ClientId: user, UserId: user, sendOut: make(chan *socket.SendMessage, 1)}
	added, err := ft.bindTopic([]string{"live-group"})
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 1 || added[0] != "live-group" || !ft.topic["live-group"] {
		t.Fatalf("client topic = %v %v", added, ft.topic)
	}
	key := registerKey("live-group", user)
	if len(subscribed) != 1 || subscribed[0] != key {
		t.Fatalf("registered %v, want %s", subscribed, key)
	}
	if _, ok := bucket.topicRelevance["live-group"]; ok {
		t.Fatal("parent topic was stored in the bucket")
	}
	if bucket.topicRelevance[key][user] != ft {
		t.Fatal("shard subscription was not stored")
	}

	other := &FireTower{ClientId: otherID, UserId: otherID, sendOut: make(chan *socket.SendMessage, 1)}
	if _, err := other.bindTopic([]string{"live-group"}); err != nil {
		t.Fatal(err)
	}
	msg := socket.GetSendMessage("g", "audit")
	msg.Type = socket.PublishKey
	msg.Topic = key
	msg.Data = []byte("once")
	if err := bucket.push(msg); err != nil {
		t.Fatal(err)
	}
	if len(ft.sendOut) != 1 || len(other.sendOut) != 0 {
		t.Fatalf("delivery first=%d other=%d", len(ft.sendOut), len(other.sendOut))
	}
	del, err := ft.unbindTopic([]string{"live-group"})
	if err != nil {
		t.Fatal(err)
	}
	if len(del) != 1 || del[0] != "live-group" || len(unsubscribed) != 1 || unsubscribed[0] != key {
		t.Fatalf("unbind client %v route %v", del, unsubscribed)
	}
}

func TestAudit_GroupPublishWritesEveryShard(t *testing.T) {
	auditEnv(t)
	SetTopicShards("live-pub", 4)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	tcp := socket.NewClient(ln.Addr().String())
	if err := tcp.Connect(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tcp.Shutdown() })
	var server net.Conn
	select {
	case server = <-accepted:
	case <-time.After(time.Second):
		t.Fatal("manager side did not accept")
	}
	t.Cleanup(func() { _ = server.Close() })
	setPool([]*managerLink{{
		name: "only", tcp: tcp, rpc: &scriptedTopic{}, register: true, publish: true, forceUp: true,
	}})
	t.Cleanup(func() { setPool(nil) })
	if err := (&FireTower{}).Publish(&FireInfo{
		Context: &FireLife{id: "m1"},
		Message: &TopicMessage{Topic: "live-pub", Data: []byte("hello")},
	}); err != nil {
		t.Fatal(err)
	}
	_ = server.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 8192)
	var got []byte
	for len(got) < 8192 {
		n, err := server.Read(buf)
		got = append(got, buf[:n]...)
		found := 0
		for i := 0; i < 4; i++ {
			key := "live-pub" + groupSep + strconv.Itoa(i)
			if strings.Contains(string(got), key) && strings.Contains(string(got), "hello") {
				found++
			}
		}
		if found == 4 {
			return
		}
		if err != nil {
			t.Fatalf("frames missing after %d bytes: %v", len(got), err)
		}
	}
	t.Fatal("did not see four shard frames")
}

func TestAudit_GroupShardsSpreadAcrossManagers(t *testing.T) {
	auditEnv(t)
	SetTopicShards("live-spread", 16)
	var links []*managerLink
	for _, name := range []string{"a", "b", "c"} {
		links = append(links, &managerLink{
			name: name, tcp: linkedPipe(t), rpc: &scriptedTopic{}, register: true, publish: true,
		})
	}
	setPool(links)
	t.Cleanup(func() { setPool(nil) })
	seen := map[string]int{}
	for _, key := range publishKeys("live-spread") {
		got := pickPublisher(key)
		if got == nil {
			t.Fatal("no publisher")
		}
		seen[got.name]++
	}
	if len(seen) < 2 {
		t.Fatalf("shards stayed on one manager: %v", seen)
	}
}

func TestAudit_GroupConnectNumSumsShards(t *testing.T) {
	auditEnv(t)
	SetTopicShards("live-num", 2)
	storeTopicGrpc(&scriptedTopic{
		num: func(_ context.Context, in *pb.GetConnectNumRequest) (*pb.GetConnectNumResponse, error) {
			if strings.Contains(in.Topic, groupSep) {
				return &pb.GetConnectNumResponse{Number: 3}, nil
			}
			return &pb.GetConnectNumResponse{Number: 100}, nil
		},
	})
	ft := &FireTower{}
	if got := ft.GetConnectNum("live-num"); got != 6 {
		t.Fatalf("grouped count = %d", got)
	}
	if got := ft.GetConnectNum("plain"); got != 100 {
		t.Fatalf("plain count = %d", got)
	}
}

func TestAudit_GroupConfigLoads(t *testing.T) {
	auditEnv(t)
	tree, err := toml.LoadBytes([]byte(`
[[group]]
topic = "live_stream"
shards = 8
`))
	if err != nil {
		t.Fatal(err)
	}
	loadGroups(tree)
	if topicShards("live_stream") != 8 || len(publishKeys("live_stream")) != 8 {
		t.Fatalf("shards = %d", topicShards("live_stream"))
	}
	SetTopicShards("live_stream", 1)
	if topicShards("live_stream") != 0 {
		t.Fatal("shards stayed on after disable")
	}
}

func userInShard(t *testing.T, topic string, want int) string {
	t.Helper()
	suffix := groupSep + strconv.Itoa(want)
	for i := 0; i < 4000; i++ {
		id := "user-" + strconv.Itoa(i)
		if registerKey(topic, id) == topic+suffix {
			return id
		}
	}
	t.Fatalf("no user landed in shard %d", want)
	return ""
}
