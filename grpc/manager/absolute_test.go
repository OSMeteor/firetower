package topicproto

import (
	"testing"

	"github.com/golang/protobuf/proto"
)

func TestAbsoluteFieldsRoundTrip(t *testing.T) {
	in := &SubscribeTopicRequest{
		Topic:      []string{"room"},
		Ip:         "10.0.0.1:1",
		Count:      []int64{4},
		Version:    []uint64{7},
		Round:      3,
		RoundBegin: true,
		RoundEnd:   true,
	}
	wire, err := proto.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out SubscribeTopicRequest
	if err := proto.Unmarshal(wire, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Count) != 1 || out.Count[0] != 4 || len(out.Version) != 1 || out.Version[0] != 7 || out.Topic[0] != "room" || out.Round != 3 || !out.RoundBegin || !out.RoundEnd {
		t.Fatalf("round trip = %+v", out)
	}
}
