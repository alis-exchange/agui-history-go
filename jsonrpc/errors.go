package jsonrpc

import (
	"errors"
	"fmt"
)

// JSONRPCError is a JSON-RPC 2.0 error.
type JSONRPCError interface {
	Error() string
	Is(target error) bool
	JSONRPCErrorObject() *jsonrpcErrorObject
}

type jsonrpcErrorObject struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// ErrInvalidRequest represents an invalid request error.
type ErrInvalidRequest struct{ err error }

func (e ErrInvalidRequest) Error() string                        { return fmt.Sprintf("invalid request: %v", e.err) }
func (e ErrInvalidRequest) Is(target error) bool                 { var t ErrInvalidRequest; return errors.As(target, &t) || errors.Is(e.err, target) }
func (e ErrInvalidRequest) JSONRPCErrorObject() *jsonrpcErrorObject { return &jsonrpcErrorObject{Code: -32600, Message: e.Error()} }

// ErrMethodNotFound represents a method not found error.
type ErrMethodNotFound struct{ err error }

func (e ErrMethodNotFound) Error() string                        { return fmt.Sprintf("method not found: %v", e.err) }
func (e ErrMethodNotFound) Is(target error) bool                 { var t ErrMethodNotFound; return errors.As(target, &t) || errors.Is(e.err, target) }
func (e ErrMethodNotFound) JSONRPCErrorObject() *jsonrpcErrorObject { return &jsonrpcErrorObject{Code: -32601, Message: e.Error()} }

// ErrInvalidParams represents an invalid params error.
type ErrInvalidParams struct{ err error }

func (e ErrInvalidParams) Error() string                        { return fmt.Sprintf("invalid params: %v", e.err) }
func (e ErrInvalidParams) Is(target error) bool                 { var t ErrInvalidParams; return errors.As(target, &t) || errors.Is(e.err, target) }
func (e ErrInvalidParams) JSONRPCErrorObject() *jsonrpcErrorObject { return &jsonrpcErrorObject{Code: -32602, Message: e.Error()} }

// ErrInternalError represents an internal JSON-RPC error.
type ErrInternalError struct{ err error }

func (e ErrInternalError) Error() string                        { return fmt.Sprintf("internal error: %v", e.err) }
func (e ErrInternalError) Is(target error) bool                 { var t ErrInternalError; return errors.As(target, &t) || errors.Is(e.err, target) }
func (e ErrInternalError) JSONRPCErrorObject() *jsonrpcErrorObject { return &jsonrpcErrorObject{Code: -32603, Message: e.Error()} }

// ErrParseError represents a parse error.
type ErrParseError struct{ err error }

func (e ErrParseError) Error() string                        { return fmt.Sprintf("parse error: %v", e.err) }
func (e ErrParseError) Is(target error) bool                 { var t ErrParseError; return errors.As(target, &t) || errors.Is(e.err, target) }
func (e ErrParseError) JSONRPCErrorObject() *jsonrpcErrorObject { return &jsonrpcErrorObject{Code: -32700, Message: e.Error()} }

// ErrNotFound represents a not found error.
type ErrNotFound struct{ err error }

func (e ErrNotFound) Error() string                        { return fmt.Sprintf("not found: %v", e.err) }
func (e ErrNotFound) Is(target error) bool                 { return errors.Is(e.err, target) }
func (e ErrNotFound) JSONRPCErrorObject() *jsonrpcErrorObject { return &jsonrpcErrorObject{Code: -32004, Message: e.Error()} }

// ErrUnauthenticated represents an unauthenticated error.
type ErrUnauthenticated struct{ err error }

func (e ErrUnauthenticated) Error() string                        { return fmt.Sprintf("unauthenticated: %v", e.err) }
func (e ErrUnauthenticated) Is(target error) bool                 { return errors.Is(e.err, target) }
func (e ErrUnauthenticated) JSONRPCErrorObject() *jsonrpcErrorObject { return &jsonrpcErrorObject{Code: -32001, Message: e.Error()} }

// ErrPermissionDenied represents a permission denied error.
type ErrPermissionDenied struct{ err error }

func (e ErrPermissionDenied) Error() string                        { return fmt.Sprintf("permission denied: %v", e.err) }
func (e ErrPermissionDenied) Is(target error) bool                 { return errors.Is(e.err, target) }
func (e ErrPermissionDenied) JSONRPCErrorObject() *jsonrpcErrorObject { return &jsonrpcErrorObject{Code: -32003, Message: e.Error()} }
