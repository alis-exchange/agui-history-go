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
// [NewThreadService] opens a Spanner client and configures an IAM authorizer
// with three roles:
//
//   - roles/open — ListThreads, GetUserThreadState, UpdateUserThreadState.
//   - roles/thread.viewer — GetThread when the caller is bound on the thread.
//   - roles/thread.admin — GetThread and DeleteThread for threads where the
//     caller is admin.
//
// [ThreadService.Register] wraps the generated gRPC registration helper so
// callers can mount the service without importing the generated protobuf
// package directly.
//
// # Storage
//
// Two Spanner tables:
//
//   - Threads table: stores Thread proto + IAM Policy proto, keyed by
//     "threads/{thread_id}".
//   - UserThreadStates table: stores UserThreadState proto, keyed by
//     "threads/{thread_id}/userStates/{user_id}".
//
// # CreateOrUpdateThread
//
// [ThreadService.CreateOrUpdateThread] is the entry point for the AG-UI
// launcher interceptor. On first call for a thread, it creates the thread
// record with a Gemini-generated display name and grants roles/thread.admin
// to the caller. On subsequent calls, it increments run_count and updates
// last_activity_time.
//
// # Code flow
//
//	GetThread / DeleteThread: authorize → validate → read thread policy → check RPC permission.
//	ListThreads: authorize open RPC → query threads (optionally filter by policy member) → join caller user state → return ThreadView projections.
//	GetUserThreadState / UpdateUserThreadState: require authenticated caller → authorize access to parent thread → read/write caller-scoped UserThreadState.
//	CreateOrUpdateThread: atomically load/create thread + increment run_count.
package service
