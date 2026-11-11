package manager

import (
	"testing"
	"time"

	"github.com/OSMeteor/firetower/socket"
)

// 只有握手帧才能改路由键。经 Gateway 转来的普通发布即使 Topic 写成握手 Topic，也不能当握手处理。
func TestGuard_PublishToHandshakeTopicDoesNotRebind(t *testing.T) {
	const remote = "10.77.2.1:2001"
	const key = "79-reserved-1"
	bucket := &connectBucket{
		conn:       &recordConn{fakeConn: fakeConn{addr: fakeAddr(remote)}},
		closeChan:  make(chan struct{}),
		packetChan: make(chan *socket.SendMessage, 4),
	}
	bucket.relation()
	t.Cleanup(func() {
		bucket.close()
		identityMu.Lock()
		delete(identities, "79")
		identityMu.Unlock()
		topicMu.Lock()
		delete(deadKeys, remote)
		delete(deadKeys, key)
		delete(deadKeys, "evil-key")
		topicMu.Unlock()
		ConnIndexTable.Delete("evil-key")
	})
	bucket.acceptHandshake(&socket.SendMessage{Type: "handshake", Topic: socket.HandshakeTopic, Data: []byte(key)})
	go bucket.sendLoop()

	for _, kind := range []string{socket.PublishKey, "user", ""} {
		msg := socket.GetSendMessage("1", "user")
		msg.Type = kind
		msg.Topic = socket.HandshakeTopic
		msg.Data = []byte("evil-key")
		bucket.packetChan <- msg
	}
	wrong := socket.GetSendMessage("1", "user")
	wrong.Type = "handshake"
	wrong.Topic = "some-room"
	wrong.Data = []byte("evil-key")
	bucket.packetChan <- wrong

	deadline := time.Now().Add(2 * time.Second)
	for len(bucket.packetChan) > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if got := bucket.indexKey(); got != key {
		t.Fatalf("route key changed to %q by a non-handshake frame", got)
	}
	if _, ok := ConnIndexTable.Load("evil-key"); ok {
		t.Fatal("a client-supplied key was indexed")
	}
	if got, ok := ConnIndexTable.Load(key); !ok || got != bucket {
		t.Fatal("the real handshake key was lost")
	}
}
