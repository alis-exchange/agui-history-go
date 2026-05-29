// Package service provides [ThreadService], the built-in Google Cloud Spanner
// implementation for persisting and querying AG-UI thread metadata.
//
// Unlike the A2A history extension, this service does not store conversation
// events. ADK sessions are the event source of truth; this service stores only
// the thread-level metadata that ADK sessions lack: display names, agent
// identity, run counts, and per-user read/pin state.
//
// # ThreadService
//
// [NewThreadService] opens a Spanner client. IAM roles are registered in
// package [go.alis.build/agui/history/service/roles] via init:
//
//   - roles/open — ListThreads (per-row access still requires thread.viewer
//     binding on the thread's IAM policy).
//   - roles/thread.viewer — GetThread, GetUserThreadState,
//     UpdateUserThreadState.
//   - roles/thread.owner — all viewer permissions plus DeleteThread.
//
// [ThreadService.Register] wraps the generated gRPC registration helper so
// callers can mount the service without importing the generated protobuf
// package directly. gRPC servers must install iam/v3 identity interceptors
// so the caller's [iam.Identity] is available in the request context:
//
//	grpc.NewServer(
//	    grpc.UnaryInterceptor(iam.UnaryInterceptor),
//	    grpc.StreamInterceptor(iam.StreamInterceptor),
//	)
//
// In-process callers (e.g. the AG-UI launcher) propagate identity via
// [iam.Identity.Context] or incoming gRPC metadata (x-alis-identity).
//
// # Storage
//
// Two Spanner tables (names are configurable via [SpannerStoreConfig]):
//
//   - Threads table: stores Thread proto + IAM Policy proto, keyed by
//     "threads/{thread_id}".
//   - UserThreadStates table: stores UserThreadState proto, keyed by
//     "threads/{thread_id}/userStates/{user_id}".
//
// # CreateOrUpdateThread
//
// [ThreadService.CreateOrUpdateThread] is the entry point for the AG-UI
// launcher interceptor. On first call for a thread it creates the record
// with a Gemini-generated display name and grants roles/thread.owner to the
// caller. On subsequent calls it increments run_count and updates
// last_activity_time. Updates to existing threads require GetThread
// permission on the thread's IAM policy.
//
// # Authorization flow
//
// Each RPC enforces authorization in two layers:
//
//  1. RPC-level gate — checks the caller has the open-role permission for the
//     method (e.g. ListThreads is open to all authenticated callers).
//  2. Resource-level check — checks the caller's identity against the thread's
//     IAM policy for finer-grained permissions (GetThread, DeleteThread, etc.).
//
// [ThreadService.ListThreads] additionally applies a SQL member prefilter so
// Spanner only returns rows where the caller appears in at least one policy
// binding. Privileged identities (system, admin) bypass the prefilter and see
// all threads. A per-row [authz.Authorizer.HasPermission] check is kept as
// defense-in-depth.
//
// # Pagination
//
// [ThreadService.ListThreads] uses cursor-based pagination keyed on
// (last_activity_time DESC, thread name DESC). Page tokens are opaque
// base64-encoded cursors. The first page is requested with an empty
// page_token; subsequent pages use the next_page_token from the response.
//
// # Code flow
//
//	GetThread / DeleteThread:
//	    validate → read thread + policy → HasPermission(method, policy).
//	ListThreads:
//	    HasPermission(ListThreads) RPC gate → SQL member prefilter + cursor
//	    pagination → per-row HasPermission(GetThread, policy) → join caller
//	    user state → return ThreadView projections.
//	GetUserThreadState / UpdateUserThreadState:
//	    read parent thread + policy → HasPermission(method, policy) →
//	    caller-scoped state checks.
//	CreateOrUpdateThread:
//	    create grants roles/thread.owner; updates require
//	    HasPermission(GetThread, policy) on existing thread.
package service
