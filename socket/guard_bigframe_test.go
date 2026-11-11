package socket

import (
	"bytes"
	"testing"
)

// 正文大于 bufio 默认缓冲（4096）时以前会被截断。正文必须原样完整还原。
func TestGuard_DepackKeepsLargeBodies(t *testing.T) {
	for _, size := range []int{0, 1, 4000, 4095, 4096, 4097, 8192, 65536, MaxFrameLength - 200} {
		body := bytes.Repeat([]byte("abcdefgh\n"), size/9+1)[:size]
		frame, err := Enpack(PublishKey, "id1", "src", "room", body)
		if err != nil {
			t.Fatal(err)
		}
		out := make(chan *SendMessage, 4)
		// 一次给完，以及按 16KB 一段分开给，两种都要对。
		for _, chunk := range []int{len(frame), 16 * 1024} {
			var rest []byte
			for off := 0; off < len(frame); off += chunk {
				end := off + chunk
				if end > len(frame) {
					end = len(frame)
				}
				rest, err = Depack(append(rest, frame[off:end]...), out)
				if err != nil {
					t.Fatalf("size %d: %v", size, err)
				}
			}
			select {
			case msg := <-out:
				if msg.Type != PublishKey || msg.Topic != "room" || msg.Context.Id != "id1" || msg.Context.Source != "src" {
					t.Fatalf("size %d: header fields wrong: %+v", size, msg)
				}
				if !bytes.Equal(msg.Data, body) {
					t.Fatalf("size %d (chunk %d): body has %d bytes, want %d", size, chunk, len(msg.Data), len(body))
				}
			default:
				t.Fatalf("size %d (chunk %d): no message", size, chunk)
			}
		}
	}
}

// 两帧挨在一起时，第一帧的正文不能吃掉第二帧，也不能和下一帧共用底层数组。
func TestGuard_DepackBackToBackFramesDoNotAlias(t *testing.T) {
	a, _ := Enpack(PublishKey, "1", "s", "ra", bytes.Repeat([]byte("A"), 5000))
	b, _ := Enpack(PublishKey, "2", "s", "rb", []byte("B"))
	buf := append(append([]byte{}, a...), b...)
	out := make(chan *SendMessage, 4)
	if _, err := Depack(buf, out); err != nil {
		t.Fatal(err)
	}
	first, second := <-out, <-out
	for i := range buf {
		buf[i] = 'Z'
	}
	if len(first.Data) != 5000 || first.Data[0] != 'A' || first.Data[4999] != 'A' || string(second.Data) != "B" || second.Topic != "rb" {
		t.Fatalf("frames were mixed up or alias the input buffer: %d %q %q", len(first.Data), second.Topic, second.Data)
	}
}
