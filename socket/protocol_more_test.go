package socket

import "testing"

func TestIntToBytesAndBack(t *testing.T) {
	tests := []int{0, 1, 255, 256, 65535, 2147483647}
	for _, v := range tests {
		b := IntToBytes(v)
		if len(b) != 4 {
			t.Errorf("IntToBytes(%d) length = %d; want 4", v, len(b))
		}
		got := BytesToInt(b)
		if got != v {
			t.Errorf("BytesToInt(IntToBytes(%d)) = %d; want %d", v, got, v)
		}
	}
}

func TestEnpack(t *testing.T) {
	_, err := Enpack("", "1", "s", "t", []byte("d"))
	if err == nil {
		t.Error("Enpack should fail with empty type")
	}
	_, err = Enpack("type", "1", "s", "", []byte("d"))
	if err == nil {
		t.Error("Enpack should fail with empty topic")
	}
	_, err = Enpack("type", "1", "s", "t", nil)
	if err == nil {
		t.Error("Enpack should fail with nil content")
	}
}
