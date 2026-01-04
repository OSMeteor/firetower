package gateway

import (
	"testing"
	"time"

	"github.com/OSMeteor/firetower/socket"
)

// Mock FireTower for testing
func newMockTower(clientId string, sendChanSize int) *FireTower {
	t := &FireTower{
		ClientId: clientId,
		sendOut:  make(chan *socket.SendMessage, sendChanSize),
		isClose:  false,
	}
	// Important: Initialize closeChan to avoid nil pointer
	t.closeChan = make(chan struct{}) 
	return t
}

func TestBucketPush(t *testing.T) {
	// Initialize bucket
	b := &Bucket{
		topicRelevance: make(map[string]map[string]*FireTower),
		BuffChan:       make(chan *socket.SendMessage, 100),
	}

	topic := "bench_topic"
	
	// Case 1: Normal Push
	client1 := newMockTower("c1", 10)
	b.AddSubscribe(topic, client1)
	
	msg := &socket.SendMessage{
		Topic: topic,
		Data:  []byte("hello"),
	}
	
	err := b.push(msg)
	if err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
	
	select {
	case <-client1.sendOut:
		// success
	default:
		t.Error("expected message in client1 sendOut")
	}

	// Case 2: Slow Consumer (Buffer Full)
	// We fill client1's buffer
	for i := 0; i < 10; i++ {
		client1.sendOut <- msg
	}
	
	// Send one more, should fail non-blocking or blocking?
	// Our 'Send' implementation in tower.go is non-blocking (select default).
	// But here 'v.Send' is called. We need to Mock v.Send? 
	// FireTower.Send is a method on *FireTower. We cannot mock it easily unless we use an interface.
	// But we can test the behavior of FireTower.Send separately, or integration test here.
	
	// Since we are using valid FireTower struct, calling b.push calls FireTower.Send.
	// FireTower.Send has the select logic.
	
	err = b.push(msg) // This attempts to send to a full channel
	
	// The push method itself returns nil because it iterates. 
	// Individual Send failures are logged but not returned by b.push for the aggregate.
	// (Wait, look at bucket.go:153: v.Send(message) return value is ignored!)
	
	// Ideally we want to ensure it DOES NOT BLOCK.
	// If it blocked, this test would hang.
	
	done := make(chan bool)
	go func() {
		b.push(msg)
		done <- true
	}()
	
	select {
	case <-done:
		// Success, didn't block
	case <-time.After(100 * time.Millisecond):
		t.Error("push blocked on full buffer")
	}
}

func BenchmarkBucketPush(b *testing.B) {
	bucket := &Bucket{
		topicRelevance: make(map[string]map[string]*FireTower),
		BuffChan:       make(chan *socket.SendMessage, 1000),
	}
	topic := "bench"
	
	// Simulate 1000 subscribers
	for i := 0; i < 1000; i++ {
		// Only 1 buffer size to simulate pressure easily if we wanted
		client := newMockTower(string(rune(i)), 100) 
		bucket.AddSubscribe(topic, client)
		
		// Drainer
		go func(c *FireTower) {
			for range c.sendOut {
			}
		}(client)
	}
	
	msg := &socket.SendMessage{
		Topic: topic,
		Data:  []byte("benchmark payload"),
	}
	
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bucket.push(msg)
	}
}
