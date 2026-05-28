package jsonrpc

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"go.alis.build/alog"
	pb "go.alis.build/common/alis/agui/history/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const (
	version = "2.0"

	methodListThreads          = "ListThreads"
	methodGetThread            = "GetThread"
	methodDeleteThread         = "DeleteThread"
	methodGetUserThreadState   = "GetUserThreadState"
	methodUpdateUserThreadState = "UpdateUserThreadState"

	// JSONRPCPath is the default HTTP path for mounting [NewJSONRPCHandler].
	JSONRPCPath = "/alis.agui.history.v1.ThreadService"
)

var (
	jsonrpcMarshaler = protojson.MarshalOptions{
		UseProtoNames:   false,
		EmitUnpopulated: true,
	}
	jsonrpcUnmarshaler = protojson.UnmarshalOptions{
		DiscardUnknown: true,
	}
)

type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	ID      any             `json:"id"`
}

type jsonrpcResponse struct {
	JSONRPC string              `json:"jsonrpc"`
	ID      any                 `json:"id"`
	Result  any                 `json:"result,omitempty"`
	Error   *jsonrpcErrorObject `json:"error,omitempty"`
}

type jsonrpcHandler struct {
	service pb.ThreadServiceServer
	cors    *corsConfig
}

type jsonrpcStream struct {
	method string
}

func (s *jsonrpcStream) Method() string                  { return s.method }
func (s *jsonrpcStream) SetHeader(_ metadata.MD) error   { return nil }
func (s *jsonrpcStream) SendHeader(_ metadata.MD) error  { return nil }
func (s *jsonrpcStream) SetTrailer(_ metadata.MD) error  { return nil }

// JSONRPCHandlerOption configures [NewJSONRPCHandler].
type JSONRPCHandlerOption func(*jsonrpcHandler)

// NewJSONRPCHandler returns an [http.Handler] that implements JSON-RPC 2.0 for
// the AG-UI thread history API. Request params and response results use protojson.
// gRPC status errors from the service are mapped to JSON-RPC errors.
func NewJSONRPCHandler(service pb.ThreadServiceServer, opts ...JSONRPCHandlerOption) http.Handler {
	h := &jsonrpcHandler{service: service}
	for _, o := range opts {
		o(h)
	}
	return h
}

func (h *jsonrpcHandler) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	md := metadata.MD{}
	for k, vs := range req.Header {
		md[strings.ToLower(k)] = vs
	}
	ctx = metadata.NewIncomingContext(ctx, md)

	if h.cors != nil {
		h.cors.writeHeaders(rw)
		if req.Method == http.MethodOptions {
			rw.WriteHeader(http.StatusOK)
			return
		}
	}

	if req.Method != http.MethodPost {
		h.writeJSONRPCError(ctx, rw, ErrInvalidRequest{err: errors.New("method not allowed")}, nil)
		return
	}

	defer req.Body.Close()

	var payload jsonrpcRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		h.writeJSONRPCError(ctx, rw, ErrParseError{err: err}, payload.ID)
		return
	}
	if payload.ID == nil || payload.ID == "" {
		h.writeJSONRPCError(ctx, rw, ErrInvalidRequest{err: errors.New("missing request id")}, nil)
		return
	}
	if payload.JSONRPC != version {
		h.writeJSONRPCError(ctx, rw, ErrInvalidRequest{err: errors.New("invalid JSON-RPC version")}, payload.ID)
		return
	}

	h.handleRequest(ctx, rw, &payload)
}

func (h *jsonrpcHandler) handleRequest(ctx context.Context, rw http.ResponseWriter, req *jsonrpcRequest) {
	var result proto.Message
	var err error

	switch req.Method {
	case methodListThreads:
		result, err = h.onListThreads(ctx, req.Params)
	case methodGetThread:
		result, err = h.onGetThread(ctx, req.Params)
	case methodDeleteThread:
		result, err = h.onDeleteThread(ctx, req.Params)
	case methodGetUserThreadState:
		result, err = h.onGetUserThreadState(ctx, req.Params)
	case methodUpdateUserThreadState:
		result, err = h.onUpdateUserThreadState(ctx, req.Params)
	case "":
		err = ErrInvalidRequest{err: errors.New("method not found")}
	default:
		err = ErrMethodNotFound{err: errors.New("method not found")}
	}
	if err != nil {
		h.writeJSONRPCError(ctx, rw, err, req.ID)
		return
	}

	if result != nil {
		resultJSON, marshalErr := jsonrpcMarshaler.Marshal(result)
		if marshalErr != nil {
			h.writeJSONRPCError(ctx, rw, ErrInternalError{err: marshalErr}, req.ID)
			return
		}
		resp := jsonrpcResponse{JSONRPC: version, ID: req.ID, Result: json.RawMessage(resultJSON)}
		if encErr := json.NewEncoder(rw).Encode(resp); encErr != nil {
			alog.Alertf(ctx, "failed to encode response: %v", encErr)
		}
	}
}

func (h *jsonrpcHandler) onListThreads(ctx context.Context, raw json.RawMessage) (*pb.ListThreadsResponse, error) {
	query := &pb.ListThreadsRequest{}
	if err := jsonrpcUnmarshaler.Unmarshal(raw, query); err != nil {
		return nil, ErrInvalidParams{err: err}
	}
	ctx = grpc.NewContextWithServerTransportStream(ctx, &jsonrpcStream{method: pb.ThreadService_ListThreads_FullMethodName})
	return h.service.ListThreads(ctx, query)
}

func (h *jsonrpcHandler) onGetThread(ctx context.Context, raw json.RawMessage) (*pb.Thread, error) {
	query := &pb.GetThreadRequest{}
	if err := jsonrpcUnmarshaler.Unmarshal(raw, query); err != nil {
		return nil, ErrInvalidParams{err: err}
	}
	ctx = grpc.NewContextWithServerTransportStream(ctx, &jsonrpcStream{method: pb.ThreadService_GetThread_FullMethodName})
	return h.service.GetThread(ctx, query)
}

func (h *jsonrpcHandler) onDeleteThread(ctx context.Context, raw json.RawMessage) (proto.Message, error) {
	query := &pb.DeleteThreadRequest{}
	if err := jsonrpcUnmarshaler.Unmarshal(raw, query); err != nil {
		return nil, ErrInvalidParams{err: err}
	}
	ctx = grpc.NewContextWithServerTransportStream(ctx, &jsonrpcStream{method: pb.ThreadService_DeleteThread_FullMethodName})
	return h.service.DeleteThread(ctx, query)
}

func (h *jsonrpcHandler) onGetUserThreadState(ctx context.Context, raw json.RawMessage) (*pb.UserThreadState, error) {
	query := &pb.GetUserThreadStateRequest{}
	if err := jsonrpcUnmarshaler.Unmarshal(raw, query); err != nil {
		return nil, ErrInvalidParams{err: err}
	}
	ctx = grpc.NewContextWithServerTransportStream(ctx, &jsonrpcStream{method: pb.ThreadService_GetUserThreadState_FullMethodName})
	return h.service.GetUserThreadState(ctx, query)
}

func (h *jsonrpcHandler) onUpdateUserThreadState(ctx context.Context, raw json.RawMessage) (*pb.UserThreadState, error) {
	query := &pb.UpdateUserThreadStateRequest{}
	if err := jsonrpcUnmarshaler.Unmarshal(raw, query); err != nil {
		return nil, ErrInvalidParams{err: err}
	}
	ctx = grpc.NewContextWithServerTransportStream(ctx, &jsonrpcStream{method: pb.ThreadService_UpdateUserThreadState_FullMethodName})
	return h.service.UpdateUserThreadState(ctx, query)
}

func (h *jsonrpcHandler) writeJSONRPCError(ctx context.Context, rw http.ResponseWriter, err error, reqID any) {
	if err == nil {
		return
	}
	var jsonrpcError JSONRPCError
	if st, ok := status.FromError(err); ok {
		jsonrpcError = grpcToJSONRPCError(st)
	} else if errors.As(err, &jsonrpcError) {
		// errors.As filled jsonrpcError
	} else {
		jsonrpcError = ErrInternalError{err: err}
	}
	resp := jsonrpcResponse{JSONRPC: version, Error: jsonrpcError.JSONRPCErrorObject(), ID: reqID}
	if encErr := json.NewEncoder(rw).Encode(resp); encErr != nil {
		alog.Alertf(ctx, "failed to send error response: %v", encErr)
	}
}

func grpcToJSONRPCError(st *status.Status) JSONRPCError {
	switch st.Code() {
	case codes.InvalidArgument:
		return ErrInvalidParams{err: st.Err()}
	case codes.NotFound:
		return ErrNotFound{err: st.Err()}
	case codes.Unauthenticated:
		return ErrUnauthenticated{err: st.Err()}
	case codes.PermissionDenied:
		return ErrPermissionDenied{err: st.Err()}
	case codes.Unimplemented:
		return ErrMethodNotFound{err: st.Err()}
	default:
		return ErrInternalError{err: st.Err()}
	}
}
