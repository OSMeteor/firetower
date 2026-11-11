package manager

import (
	"context"
	"testing"

	pb "github.com/OSMeteor/firetower/grpc/manager"
	"github.com/OSMeteor/firetower/socket"
)

func guardNum(t *testing.T, svc *topicGrpcService, topic string) int64 {
	t.Helper()
	res, err := svc.GetConnectNum(context.Background(), &pb.GetConnectNumRequest{Topic: topic})
	if err != nil {
		t.Fatal(err)
	}
	return res.Number
}

func guardSub(t *testing.T, svc *topicGrpcService, ip, topic string, count int64, ver uint64) {
	t.Helper()
	if _, err := svc.SubscribeTopic(context.Background(), &pb.SubscribeTopicRequest{
		Ip: ip, Topic: []string{topic}, Count: []int64{count}, Version: []uint64{ver},
	}); err != nil {
		t.Fatal(err)
	}
}

// 握手确认前 Gateway 用本地地址登记。这样的请求在 rebind 之后才到时，
// 人数要记在路由键上，不能留在一个没有连接的地址键下面。
func TestGuard_LateAddressRegistrationFollowsTheHandshakeKey(t *testing.T) {
	const remote = "10.77.1.1:1001"
	const key = "77-latekey-1"
	const topic = "guard-late-room"
	svc := &topicGrpcService{}
	bucket := &connectBucket{conn: &recordConn{fakeConn: fakeConn{addr: fakeAddr(remote)}}, closeChan: make(chan struct{})}
	bucket.relation()
	t.Cleanup(func() {
		bucket.close()
		identityMu.Lock()
		delete(identities, "77")
		identityMu.Unlock()
		topicMu.Lock()
		delete(deadKeys, remote)
		delete(deadKeys, key)
		topicMu.Unlock()
	})
	bucket.acceptHandshake(&socket.SendMessage{Type: "handshake", Topic: socket.HandshakeTopic, Data: []byte(key)})

	// 握手之后才到的、仍然写着地址的登记。
	guardSub(t, svc, remote, topic, 4, 10)
	if got := guardNum(t, svc, topic); got != 4 {
		t.Fatalf("late registration: want 4, got %d", got)
	}
	// 随后的重放用路由键盖写同一份人数，不能变成两份。
	guardSub(t, svc, key, topic, 4, 10)
	if got := guardNum(t, svc, topic); got != 4 {
		t.Fatalf("replay under the handshake key doubled the count: got %d", got)
	}
	targets, ok := snapshotTargets(topic)
	if !ok || len(targets) != 1 || targets[0] != bucket {
		t.Fatalf("publish targets = %v, want the live connection", targets)
	}
	// 退订也可能还写着地址。
	if _, err := svc.UnSubscribeTopic(context.Background(), &pb.UnSubscribeTopicRequest{
		Ip: remote, Topic: []string{topic}, Count: []int64{0}, Version: []uint64{11},
	}); err != nil {
		t.Fatal(err)
	}
	if got := guardNum(t, svc, topic); got != 0 {
		t.Fatalf("late unsubscribe under the address was lost: got %d", got)
	}
}

// 连接断开之后才到的登记不能写回去，否则这个键下的人数没有人会再清。
func TestGuard_RegistrationAfterDisconnectIsDropped(t *testing.T) {
	const remote = "10.77.1.2:1002"
	const key = "78-latekey-1"
	const topic = "guard-dead-room"
	svc := &topicGrpcService{}
	bucket := &connectBucket{conn: &recordConn{fakeConn: fakeConn{addr: fakeAddr(remote)}}, closeChan: make(chan struct{})}
	bucket.relation()
	t.Cleanup(func() {
		identityMu.Lock()
		delete(identities, "78")
		identityMu.Unlock()
		topicMu.Lock()
		delete(deadKeys, remote)
		delete(deadKeys, key)
		topicMu.Unlock()
		topicRelevance.Delete(topic)
	})
	bucket.acceptHandshake(&socket.SendMessage{Type: "handshake", Topic: socket.HandshakeTopic, Data: []byte(key)})
	guardSub(t, svc, key, topic, 3, 5)
	if got := guardNum(t, svc, topic); got != 3 {
		t.Fatalf("want 3 before disconnect, got %d", got)
	}
	bucket.close()
	if got := guardNum(t, svc, topic); got != 0 {
		t.Fatalf("disconnect did not clear the count: got %d", got)
	}
	guardSub(t, svc, key, topic, 3, 6)
	guardSub(t, svc, remote, topic, 3, 7)
	if got := guardNum(t, svc, topic); got != 0 {
		t.Fatalf("registration that arrived after the disconnect came back: got %d", got)
	}

	// 同一个地址被新连接复用后，新连接的登记必须照常生效。
	again := &connectBucket{conn: &recordConn{fakeConn: fakeConn{addr: fakeAddr(remote)}}, closeChan: make(chan struct{})}
	again.relation()
	defer again.close()
	guardSub(t, svc, remote, topic, 2, 8)
	if got := guardNum(t, svc, topic); got != 2 {
		t.Fatalf("a new connection on a reused address was treated as dead: got %d", got)
	}
}

// 地址被新连接接手、旧连接随后才关闭时，不能把新连接的键标成已断开。
func TestGuard_ReplacedConnectionDoesNotBuryTheNewOwner(t *testing.T) {
	const remote = "10.77.1.3:1003"
	const topic = "guard-reuse-room"
	svc := &topicGrpcService{}
	old := &connectBucket{conn: &recordConn{fakeConn: fakeConn{addr: fakeAddr(remote)}}, closeChan: make(chan struct{})}
	old.relation()
	fresh := &connectBucket{conn: &recordConn{fakeConn: fakeConn{addr: fakeAddr(remote)}}, closeChan: make(chan struct{})}
	fresh.relation() // 会关掉 old
	t.Cleanup(func() {
		fresh.close()
		topicMu.Lock()
		delete(deadKeys, remote)
		topicMu.Unlock()
		topicRelevance.Delete(topic)
	})
	guardSub(t, svc, remote, topic, 1, 3)
	if got := guardNum(t, svc, topic); got != 1 {
		t.Fatalf("the new owner of a reused address cannot register: got %d", got)
	}
}
