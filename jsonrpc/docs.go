// Package jsonrpc provides a JSON-RPC 2.0 HTTP handler for the AG-UI thread
// history [service.ThreadService].
//
// Supported methods: ListThreads, GetThread, DeleteThread, GetUserThreadState,
// UpdateUserThreadState. Request params and response results use protojson
// (camelCase JSON names). gRPC status errors from the service are mapped to
// JSON-RPC error codes.
//
// For browser clients, pass [WithCORS] (and [CORSAllowOrigin] /
// [CORSAllowHeaders] / [CORSAllowMethods]) to enable CORS headers and OPTIONS
// preflight handling.
package jsonrpc
