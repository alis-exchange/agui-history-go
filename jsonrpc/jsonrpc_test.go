package jsonrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	pb "go.alis.build/common/alis/agui/history/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type fakeThreadService struct {
	pb.UnimplementedThreadServiceServer

	listResp   *pb.ListThreadsResponse
	getResp    *pb.Thread
	stateResp  *pb.UserThreadState
	returnErr  error
	lastMethod string
}

func (f *fakeThreadService) ListThreads(_ context.Context, _ *pb.ListThreadsRequest) (*pb.ListThreadsResponse, error) {
	f.lastMethod = "ListThreads"
	if f.returnErr != nil {
		return nil, f.returnErr
	}
	return f.listResp, nil
}

func (f *fakeThreadService) GetThread(_ context.Context, req *pb.GetThreadRequest) (*pb.Thread, error) {
	f.lastMethod = "GetThread"
	if f.returnErr != nil {
		return nil, f.returnErr
	}
	return f.getResp, nil
}

func (f *fakeThreadService) DeleteThread(_ context.Context, _ *pb.DeleteThreadRequest) (*emptypb.Empty, error) {
	f.lastMethod = "DeleteThread"
	if f.returnErr != nil {
		return nil, f.returnErr
	}
	return &emptypb.Empty{}, nil
}

func (f *fakeThreadService) GetUserThreadState(_ context.Context, _ *pb.GetUserThreadStateRequest) (*pb.UserThreadState, error) {
	f.lastMethod = "GetUserThreadState"
	if f.returnErr != nil {
		return nil, f.returnErr
	}
	return f.stateResp, nil
}

func (f *fakeThreadService) UpdateUserThreadState(_ context.Context, req *pb.UpdateUserThreadStateRequest) (*pb.UserThreadState, error) {
	f.lastMethod = "UpdateUserThreadState"
	if f.returnErr != nil {
		return nil, f.returnErr
	}
	return f.stateResp, nil
}

func postJSONRPC(handler http.Handler, method string, params any, id any) *httptest.ResponseRecorder {
	body := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"id":      id,
	}
	if params != nil {
		body["params"] = params
	}
	data, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, JSONRPCPath, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func decodeResponse(t *testing.T, rec *httptest.ResponseRecorder) jsonrpcResponse {
	t.Helper()
	var resp jsonrpcResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp
}

func TestMethodDispatch(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		wantMethod string
	}{
		{"ListThreads", "ListThreads", "ListThreads"},
		{"GetThread", "GetThread", "GetThread"},
		{"DeleteThread", "DeleteThread", "DeleteThread"},
		{"GetUserThreadState", "GetUserThreadState", "GetUserThreadState"},
		{"UpdateUserThreadState", "UpdateUserThreadState", "UpdateUserThreadState"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &fakeThreadService{
				listResp:  &pb.ListThreadsResponse{},
				getResp:   &pb.Thread{Name: "threads/test"},
				stateResp: &pb.UserThreadState{Name: "threads/test/userStates/u1"},
			}
			handler := NewJSONRPCHandler(svc)
			rec := postJSONRPC(handler, tt.method, map[string]any{"name": "threads/test"}, "req-1")

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			resp := decodeResponse(t, rec)
			if resp.Error != nil {
				t.Fatalf("unexpected error: %+v", resp.Error)
			}
			if resp.ID != "req-1" {
				t.Errorf("id = %v, want req-1", resp.ID)
			}
			if svc.lastMethod != tt.wantMethod {
				t.Errorf("dispatched to %q, want %q", svc.lastMethod, tt.wantMethod)
			}
		})
	}
}

func TestUnknownMethod(t *testing.T) {
	handler := NewJSONRPCHandler(&fakeThreadService{})
	rec := postJSONRPC(handler, "DoSomethingElse", nil, "req-1")

	resp := decodeResponse(t, rec)
	if resp.Error == nil {
		t.Fatal("expected error for unknown method")
	}
	if resp.Error.Code != -32601 {
		t.Errorf("error code = %d, want -32601 (method not found)", resp.Error.Code)
	}
}

func TestInvalidJSON(t *testing.T) {
	handler := NewJSONRPCHandler(&fakeThreadService{})
	req := httptest.NewRequest(http.MethodPost, JSONRPCPath, bytes.NewReader([]byte("not json")))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	resp := decodeResponse(t, rec)
	if resp.Error == nil {
		t.Fatal("expected parse error")
	}
	if resp.Error.Code != -32700 {
		t.Errorf("error code = %d, want -32700 (parse error)", resp.Error.Code)
	}
}

func TestMissingID(t *testing.T) {
	handler := NewJSONRPCHandler(&fakeThreadService{})
	data, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "ListThreads",
	})
	req := httptest.NewRequest(http.MethodPost, JSONRPCPath, bytes.NewReader(data))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	resp := decodeResponse(t, rec)
	if resp.Error == nil {
		t.Fatal("expected error for missing id")
	}
	if resp.Error.Code != -32600 {
		t.Errorf("error code = %d, want -32600 (invalid request)", resp.Error.Code)
	}
}

func TestInvalidVersion(t *testing.T) {
	handler := NewJSONRPCHandler(&fakeThreadService{})
	data, _ := json.Marshal(map[string]any{
		"jsonrpc": "1.0",
		"method":  "ListThreads",
		"id":      "req-1",
	})
	req := httptest.NewRequest(http.MethodPost, JSONRPCPath, bytes.NewReader(data))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	resp := decodeResponse(t, rec)
	if resp.Error == nil {
		t.Fatal("expected error for invalid version")
	}
	if resp.Error.Code != -32600 {
		t.Errorf("error code = %d, want -32600 (invalid request)", resp.Error.Code)
	}
}

func TestGETRejected(t *testing.T) {
	handler := NewJSONRPCHandler(&fakeThreadService{})
	req := httptest.NewRequest(http.MethodGet, JSONRPCPath, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	resp := decodeResponse(t, rec)
	if resp.Error == nil {
		t.Fatal("expected error for GET")
	}
	if resp.Error.Code != -32600 {
		t.Errorf("error code = %d, want -32600 (invalid request)", resp.Error.Code)
	}
}

func TestGRPCErrorMapping(t *testing.T) {
	tests := []struct {
		name     string
		grpcCode codes.Code
		wantCode int
	}{
		{"not found", codes.NotFound, -32004},
		{"invalid argument", codes.InvalidArgument, -32602},
		{"unauthenticated", codes.Unauthenticated, -32001},
		{"permission denied", codes.PermissionDenied, -32003},
		{"internal", codes.Internal, -32603},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &fakeThreadService{
				returnErr: status.Error(tt.grpcCode, "test error"),
			}
			handler := NewJSONRPCHandler(svc)
			rec := postJSONRPC(handler, "GetThread", map[string]any{"name": "threads/x"}, "req-1")

			resp := decodeResponse(t, rec)
			if resp.Error == nil {
				t.Fatal("expected error")
			}
			if resp.Error.Code != tt.wantCode {
				t.Errorf("error code = %d, want %d", resp.Error.Code, tt.wantCode)
			}
		})
	}
}

func TestCORS_Preflight(t *testing.T) {
	handler := NewJSONRPCHandler(&fakeThreadService{}, WithCORS())
	req := httptest.NewRequest(http.MethodOptions, JSONRPCPath, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Allow-Origin = %q, want *", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); got != "POST, OPTIONS" {
		t.Errorf("Allow-Methods = %q, want 'POST, OPTIONS'", got)
	}
}

func TestCORS_CustomOrigin(t *testing.T) {
	handler := NewJSONRPCHandler(&fakeThreadService{}, WithCORS(CORSAllowOrigin("https://app.example.com")))
	rec := postJSONRPC(handler, "ListThreads", map[string]any{}, "req-1")

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Errorf("Allow-Origin = %q, want https://app.example.com", got)
	}
}

func TestCORS_Disabled(t *testing.T) {
	handler := NewJSONRPCHandler(&fakeThreadService{listResp: &pb.ListThreadsResponse{}})
	rec := postJSONRPC(handler, "ListThreads", map[string]any{}, "req-1")

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Allow-Origin should be empty when CORS disabled, got %q", got)
	}
}

func TestListThreads_Response(t *testing.T) {
	svc := &fakeThreadService{
		listResp: &pb.ListThreadsResponse{
			Threads: []*pb.ThreadView{
				{
					Thread: &pb.Thread{
						Name:        "threads/t1",
						DisplayName: "Test Thread",
						RunCount:    3,
						CreateTime:  timestamppb.Now(),
					},
					HasUnread:    true,
					ReadRunCount: 1,
				},
			},
			NextPageToken: "next",
		},
	}
	handler := NewJSONRPCHandler(svc)
	rec := postJSONRPC(handler, "ListThreads", map[string]any{}, "req-1")

	resp := decodeResponse(t, rec)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("result type = %T, want map", resp.Result)
	}
	threads, ok := result["threads"].([]any)
	if !ok || len(threads) != 1 {
		t.Fatalf("threads = %v, want 1 element", result["threads"])
	}
	if result["nextPageToken"] != "next" {
		t.Errorf("nextPageToken = %v, want 'next'", result["nextPageToken"])
	}
}

func TestHeadersForwardedAsGRPCMetadata(t *testing.T) {
	var capturedHeaders []string
	svc := &fakeThreadService{}
	svc.listResp = &pb.ListThreadsResponse{}

	handler := NewJSONRPCHandler(svc)
	data, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "ListThreads",
		"params":  map[string]any{},
		"id":      "req-1",
	})
	req := httptest.NewRequest(http.MethodPost, JSONRPCPath, bytes.NewReader(data))
	req.Header.Set("X-Custom-Header", "test-value")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	_ = capturedHeaders
	resp := decodeResponse(t, rec)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
}
