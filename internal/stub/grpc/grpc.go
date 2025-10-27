package grpc

import (
	"context"
	"net"
)

// SupportPackageIsVersion4 keeps generated protobuf code compatible with the
// stub implementation.
const SupportPackageIsVersion4 = 4

// DialOption mirrors the upstream type.
type DialOption interface{}

// CallOption mirrors the upstream type.
type CallOption interface{}

// ServerOption mirrors the upstream type.
type ServerOption interface{}

// UnaryHandler is used by UnaryServerInterceptor.
type UnaryHandler func(ctx context.Context, req interface{}) (interface{}, error)

// UnaryServerInfo carries metadata about the RPC being invoked.
type UnaryServerInfo struct {
	Server     interface{}
	FullMethod string
}

// UnaryServerInterceptor matches the upstream signature.
type UnaryServerInterceptor func(ctx context.Context, req interface{}, info *UnaryServerInfo, handler UnaryHandler) (interface{}, error)

// MethodDesc is part of a service definition.
type MethodDesc struct {
	MethodName string
	Handler    func(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor UnaryServerInterceptor) (interface{}, error)
}

// StreamDesc exists for API compatibility.
type StreamDesc struct{}

// ServiceDesc aggregates all handlers for a gRPC service.
type ServiceDesc struct {
	ServiceName string
	HandlerType interface{}
	Methods     []MethodDesc
	Streams     []StreamDesc
	Metadata    interface{}
}

// ClientConn is a lightweight stand-in for the real gRPC client connection.
type ClientConn struct{}

// Dial returns a stub client connection.
func Dial(target string, opts ...DialOption) (*ClientConn, error) {
	return &ClientConn{}, nil
}

// WithInsecure mirrors the upstream helper that configures plaintext
// connections.
func WithInsecure() DialOption { return struct{}{} }

// Invoke performs a unary RPC call. The stub does not issue network traffic and
// simply returns nil.
func (c *ClientConn) Invoke(ctx context.Context, method string, args interface{}, reply interface{}, opts ...CallOption) error {
	return nil
}

// Server is a placeholder for the real gRPC server implementation.
type Server struct{}

// NewServer creates a stub server instance.
func NewServer(opts ...ServerOption) *Server { return &Server{} }

// RegisterService is a no-op retained for API compatibility.
func (s *Server) RegisterService(desc *ServiceDesc, impl interface{}) {}

// Serve is a no-op retained for API compatibility.
func (s *Server) Serve(lis net.Listener) error { return nil }
