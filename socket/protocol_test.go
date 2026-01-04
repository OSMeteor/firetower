package socket

import (
	"bytes"
	"testing"
)

func TestDepack(t *testing.T) {
	// Construct a valid packet
	pushType := "test"
	messageId := "1"
	source := "unit_test"
	topic := "demo"
	content := []byte("hello world")

	packet, err := Enpack(pushType, messageId, source, topic, content)
	if err != nil {
		t.Fatalf("Enpack failed: %v", err)
	}

	// Case 1: Multiple packets in one buffer
	var buffer []byte
	buffer = append(buffer, packet...)
	buffer = append(buffer, packet...)
	
	ch := make(chan *SendMessage, 10)
	
	// Process
	overflow, err := Depack(buffer, ch)
	if err != nil {
		t.Fatalf("Depack error: %v", err)
	}

	if len(overflow) != 0 {
		t.Errorf("Expected empty overflow, got length %d", len(overflow))
	}

	if len(ch) != 2 {
		t.Errorf("Expected 2 messages, got %d", len(ch))
	}

	// Case 2: Partial packet
	buffer = append(buffer, packet...)
	buffer = append(buffer, packet[:5]...) // Partial packet
	
	overflow, err = Depack(buffer, ch)
	if err != nil {
		t.Fatalf("Depack error: %v", err)
	}
	
	// We consumed 2 full packets (previously pushed to channel? No, channel had 2, now should have 2+1=3?)
	// Wait, Depack processes the WHOLE buffer.
	// In Case 1 we cleared `buffer` implicitly by passing it by value? No, slice is reference-ish.
	// But `Depack` does not modify the underlying array of the input slice, it returns a new slice view.
	
	// Let's reset for Case 2
	close(ch)
	ch = make(chan *SendMessage, 10)
	buffer = []byte{}
	buffer = append(buffer, packet...) // Full packet
	buffer = append(buffer, packet[:len(packet)-2]...) // Partial packet
	
	overflow, err = Depack(buffer, ch)
	if err != nil {
		t.Fatalf("Depack error: %v", err)
	}
	
	if len(ch) != 1 {
		t.Errorf("Expected 1 message, got %d", len(ch))
	}
	
	if len(overflow) != len(packet)-2 {
		t.Errorf("Expected overflow length %d, got %d", len(packet)-2, len(overflow))
	}
	
	if !bytes.Equal(overflow, packet[:len(packet)-2]) {
		t.Errorf("Overflow content mismatch")
	}
}

func BenchmarkDepack(b *testing.B) {
	pushType := "test"
	messageId := "1"
	source := "unit_test"
	topic := "demo"
	content := []byte("hello world")
	packet, _ := Enpack(pushType, messageId, source, topic, content)
	
	// Make a buffer with 100 packets
	var buffer []byte
	for i := 0; i < 100; i++ {
		buffer = append(buffer, packet...)
	}
	
	ch := make(chan *SendMessage, 1000)
	
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Depack(buffer, ch)
		// Drain channel
		for len(ch) > 0 {
			<-ch
		}
	}
}
