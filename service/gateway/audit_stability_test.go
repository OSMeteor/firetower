package gateway

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	pb "github.com/OSMeteor/firetower/grpc/manager"
	"github.com/OSMeteor/firetower/socket"
	"github.com/pelletier/go-toml"
)

func TestAudit_ToSelfNilHasNoResidue(t *testing.T) {
	dirty := socket.GetSendMessage("1", "user")
	dirty.Type = "publish"
	dirty.Topic = "room"
	dirty.Data = []byte("secret")
	dirty.Recycling()

	ft := &FireTower{sendOut: make(chan *socket.SendMessage, 1)}
	if err := ft.ToSelf(nil); err != nil {
		t.Fatal(err)
	}
	got := <-ft.sendOut
	if len(got.Data) != 0 || got.Type != "" || got.Topic != "" {
		t.Fatalf("ToSelf(nil) queued residue type=%q topic=%q data=%q", got.Type, got.Topic, got.Data)
	}
}

func TestAudit_TopicFrameAndConnLimits(t *testing.T) {
	if _, ok := splitTopics("a,b", 2); !ok {
		t.Fatal("two topics were rejected")
	}
	if _, ok := splitTopics(strings.Repeat("t,", 200000), 8); ok {
		t.Fatal("huge topic frame was accepted")
	}

	auditEnv(t)
	tree, err := toml.LoadBytes([]byte("maxTopicsPerConn = 1\nmaxTopicsPerFrame = 8\nchanLens = 4\nheartbeat = 3600\n"))
	if err != nil {
		t.Fatal(err)
	}
	storeConfigTree(tree)
	ft := &FireTower{ClientId: "limit", topic: map[string]bool{}}
	if _, err := ft.bindTopic([]string{"a", "b"}); err == nil {
		t.Fatal("connection accepted more topics than its limit")
	}
	if len(ft.topic) != 0 {
		t.Fatalf("rejected subscribe recorded %v", ft.topic)
	}
	if _, err := ft.bindTopic([]string{"a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ft.bindTopic([]string{"b"}); err == nil {
		t.Fatal("second topic exceeded the per-connection limit")
	}
}

func TestAudit_ConnectionLimit(t *testing.T) {
	saveAuditGlobals(t)
	prev := liveConns.Load()
	t.Cleanup(func() { liveConns.Store(prev) })
	tree, err := toml.LoadBytes([]byte("maxConnections = 1\nchanLens = 4\n"))
	if err != nil {
		t.Fatal(err)
	}
	storeConfigTree(tree)
	storeTopicGrpc(&scriptedTopic{})
	server, _, cleanup := wsPair(t)
	defer cleanup()
	first := BuildTower(server, "a")
	if first == nil {
		t.Fatal("first connection was rejected")
	}
	t.Cleanup(func() { first.Close() })
	if second := BuildTower(server, "b"); second != nil {
		second.Close()
		t.Fatal("connection limit was not enforced")
	}
}

func TestAudit_ManagerRPCDoesNotHang(t *testing.T) {
	saveAuditGlobals(t)
	storeTopicGrpc(&scriptedTopic{
		check: func(ctx context.Context, in *pb.CheckTopicExistRequest) (*pb.CheckTopicExistResponse, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
		num: func(ctx context.Context, in *pb.GetConnectNumRequest) (*pb.GetConnectNumResponse, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})
	ft := &FireTower{}
	finishWithin(t, 2*time.Second, "CheckTopicExist", func() { _ = ft.CheckTopicExist("room") })
	finishWithin(t, 2*time.Second, "GetConnectNum", func() { _ = ft.GetConnectNum("room") })
}

func TestAudit_ManagerIPNilConn(t *testing.T) {
	saveAuditGlobals(t)
	storeTopicGrpc(&scriptedTopic{})
	storeTopicManage(&socket.TcpClient{})
	if _, err := managerIP(); err == nil {
		t.Fatal("nil connection returned an address")
	}
}

func TestAudit_DropLogRespectsLevel(t *testing.T) {
	saveAuditGlobals(t)
	prevLevel := LogLevel
	prevWriter := DefaultErrorWriter
	t.Cleanup(func() {
		LogLevel = prevLevel
		DefaultErrorWriter = prevWriter
	})
	var buf bytes.Buffer
	DefaultErrorWriter = &buf
	storeTowerLogger(towerLog)
	ft := &FireTower{sendOut: make(chan *socket.SendMessage)}

	LogLevel = "ERROR"
	lastDropLog.Store(0)
	for i := 0; i < 20; i++ {
		_ = ft.Send(socket.GetSendMessage("1", "user"))
	}
	if buf.Len() != 0 {
		t.Fatalf("drops were logged at ERROR level: %s", buf.String())
	}

	LogLevel = "INFO"
	lastDropLog.Store(0)
	_ = ft.Send(socket.GetSendMessage("1", "user"))
	_ = ft.Send(socket.GetSendMessage("1", "user"))
	if strings.Count(buf.String(), "send buffer full") != 1 {
		t.Fatalf("drop log = %q", buf.String())
	}
}
