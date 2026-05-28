package jsonrpc

import (
	"net/http"

	pb "go.alis.build/common/alis/agui/history/v1"
)

// HTTPRegistrar is implemented by routers that support net/http-style handler
// registration with method-aware patterns (e.g. Go 1.22+ ServeMux).
type HTTPRegistrar interface {
	Handle(pattern string, handler http.Handler)
}

// Register mounts the thread history JSON-RPC handler at [JSONRPCPath] for POST
// and OPTIONS requests on method-aware muxes.
func Register(mux HTTPRegistrar, service pb.ThreadServiceServer, opts ...JSONRPCHandlerOption) {
	handler := NewJSONRPCHandler(service, opts...)
	mux.Handle("POST "+JSONRPCPath, handler)
	mux.Handle("OPTIONS "+JSONRPCPath, handler)
}
