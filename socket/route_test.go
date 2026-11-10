package socket

import "testing"

func TestAudit_DisconnectClearsTheRouteKey(t *testing.T) {
	client := NewClient("127.0.0.1:1")
	client.SetRouteKey("1-abcd-1")
	client.ApplyHeartbeat("wait")
	if client.Publishable() {
		t.Fatal("wait heartbeat stayed publishable")
	}
	client.Shutdown()
	if client.RouteKey() != "" {
		t.Fatal("disconnected client kept its route key")
	}
	if !client.Publishable() {
		t.Fatal("disconnected client stayed in the wait state")
	}
}
