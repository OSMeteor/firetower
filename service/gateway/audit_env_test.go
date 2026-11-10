package gateway

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	pb "github.com/OSMeteor/firetower/grpc/manager"
	"github.com/OSMeteor/firetower/socket"
	"github.com/gorilla/websocket"
	"github.com/holdno/snowFlakeByGo"
	"github.com/pelletier/go-toml"
	"google.golang.org/grpc"
)

// 验收辅助。这些用例各自带时限：死锁必须由用例失败退出，不能靠 go test 的进程超时发现。

func saveAuditGlobals(t *testing.T) {
	t.Helper()
	saved := snapshotGW()
	groups := groupsSnapshot()
	t.Cleanup(func() {
		restoreGW(saved)
		restoreGroups(groups)
	})
}

func loadAuditConfig(t *testing.T, heartbeat, buff, consumers, buckets int) {
	t.Helper()
	tree, err := toml.LoadBytes([]byte(strings.TrimSpace(`
chanLens = 8
heartbeat = ` + itoa(heartbeat) + `
topicServiceAddr = "127.0.0.1:1"

[grpc]
address = "127.0.0.1:1"

[bucket]
Num = ` + itoa(buckets) + `
CentralChanCount = 8
BuffChanCount = ` + itoa(buff) + `
ConsumerNum = ` + itoa(consumers) + `
`)))
	if err != nil {
		t.Fatalf("load audit config: %v", err)
	}
	storeConfigTree(tree)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [16]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func noopAuditLogs() {
	storeTowerLogger(func(*FireTower, string, string) {})
	storeFireLogger(func(*FireInfo, string, string) {})
}

func ensureIDWorker(t *testing.T) {
	t.Helper()
	if loadIDWorker() != nil {
		return
	}
	worker, err := snowFlakeByGo.NewWorker(1)
	if err != nil {
		t.Fatalf("snowflake worker: %v", err)
	}
	storeIDWorker(worker)
}

func installAuditPipe(t *testing.T) {
	t.Helper()
	left, right := net.Pipe()
	t.Cleanup(func() {
		_ = left.Close()
		_ = right.Close()
	})
	storeTopicManage(&socket.TcpClient{Conn: left})
}

func installAuditBucket(t *testing.T) *Bucket {
	t.Helper()
	bucket := &Bucket{
		topicRelevance: make(map[string]map[string]*FireTower),
		BuffChan:       make(chan *socket.SendMessage, 4),
	}
	storeTM(&TowerManager{bucket: []*Bucket{bucket}})
	return bucket
}

// auditEnv 准备 Close / 退订路径会读到的全局依赖。gRPC 默认成功返回。
func auditEnv(t *testing.T) *Bucket {
	t.Helper()
	saveAuditGlobals(t)
	loadAuditConfig(t, 3600, 4, 1, 1)
	noopAuditLogs()
	ensureIDWorker(t)
	installAuditPipe(t)
	storeTopicGrpc(&scriptedTopic{})
	return installAuditBucket(t)
}

func finishWithin(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s: did not finish within %s", what, d)
	}
}

func wsPair(t *testing.T) (server, client *websocket.Conn, cleanup func()) {
	t.Helper()
	srvConn := make(chan *websocket.Conn, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		srvConn <- conn
	}))
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	cli, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		srv.Close()
		t.Fatalf("dial websocket: %v", err)
	}
	select {
	case server = <-srvConn:
	case <-time.After(2 * time.Second):
		_ = cli.Close()
		srv.Close()
		t.Fatal("websocket server connection was not established")
	}
	cleanup = func() {
		_ = cli.Close()
		_ = server.Close()
		srv.Close()
	}
	return server, cli, cleanup
}

func auditTower(t *testing.T, clientID, userID string) *FireTower {
	t.Helper()
	server, _, cleanup := wsPair(t)
	t.Cleanup(cleanup)
	return &FireTower{
		ClientId:  clientID,
		UserId:    userID,
		startTime: time.Now(),
		readIn:    make(chan *FireInfo, 4),
		sendOut:   make(chan *socket.SendMessage, 4),
		ws:        server,
		topic:     map[string]bool{"room": true},
		closeChan: make(chan struct{}),
	}
}

type scriptedTopic struct {
	unsub func(ctx context.Context, in *pb.UnSubscribeTopicRequest) (*pb.UnSubscribeTopicResponse, error)
	sub   func(ctx context.Context, in *pb.SubscribeTopicRequest) (*pb.SubscribeTopicResponse, error)
	check func(ctx context.Context, in *pb.CheckTopicExistRequest) (*pb.CheckTopicExistResponse, error)
	num   func(ctx context.Context, in *pb.GetConnectNumRequest) (*pb.GetConnectNumResponse, error)
}

func (s *scriptedTopic) GetConnectNum(ctx context.Context, in *pb.GetConnectNumRequest, opts ...grpc.CallOption) (*pb.GetConnectNumResponse, error) {
	if s != nil && s.num != nil {
		return s.num(ctx, in)
	}
	return &pb.GetConnectNumResponse{}, nil
}

func (s *scriptedTopic) SubscribeTopic(ctx context.Context, in *pb.SubscribeTopicRequest, opts ...grpc.CallOption) (*pb.SubscribeTopicResponse, error) {
	if s.sub != nil {
		return s.sub(ctx, in)
	}
	return &pb.SubscribeTopicResponse{}, nil
}

func (s *scriptedTopic) UnSubscribeTopic(ctx context.Context, in *pb.UnSubscribeTopicRequest, opts ...grpc.CallOption) (*pb.UnSubscribeTopicResponse, error) {
	if s.unsub != nil {
		return s.unsub(ctx, in)
	}
	return &pb.UnSubscribeTopicResponse{}, nil
}

func (s *scriptedTopic) Publish(ctx context.Context, in *pb.PublishRequest, opts ...grpc.CallOption) (*pb.PublishResponse, error) {
	return &pb.PublishResponse{}, nil
}

func (s *scriptedTopic) CheckTopicExist(ctx context.Context, in *pb.CheckTopicExistRequest, opts ...grpc.CallOption) (*pb.CheckTopicExistResponse, error) {
	if s != nil && s.check != nil {
		return s.check(ctx, in)
	}
	return &pb.CheckTopicExistResponse{}, nil
}
