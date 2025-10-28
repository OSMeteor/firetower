package proto

import "fmt"

// Message is the interface expected by generated protobuf code.
type Message interface {
	ProtoMessage()
}

// InternalMessageInfo provides the helper methods referenced by generated
// protobuf code. The stub implementations are no-ops that satisfy the interface
// expectations during compilation.
type InternalMessageInfo struct{}

func (InternalMessageInfo) Unmarshal(m interface{}, b []byte) error { return nil }
func (InternalMessageInfo) Marshal(b []byte, m interface{}, deterministic bool) ([]byte, error) {
	return nil, nil
}
func (InternalMessageInfo) Merge(dst, src interface{}) {}
func (InternalMessageInfo) Size(interface{}) int       { return 0 }
func (InternalMessageInfo) DiscardUnknown(interface{}) {}

// Marshal encodes the provided message. The stub returns an empty slice to keep
// the generated code operational in a test environment.
func Marshal(m Message) ([]byte, error) { return nil, nil }

// Unmarshal decodes data into the provided message.
func Unmarshal(b []byte, m Message) error { return nil }

// CompactTextString creates a printable representation of a message, similar to
// the behaviour of the upstream implementation.
func CompactTextString(m Message) string { return fmt.Sprintf("%T", m) }

// RegisterType and RegisterFile are no-ops retained for API compatibility.
func RegisterType(msg interface{}, name string)       {}
func RegisterFile(filename string, descriptor []byte) {}

// ProtoPackageIsVersion2 mirrors the constant exposed by the upstream module.
const ProtoPackageIsVersion2 = 2
