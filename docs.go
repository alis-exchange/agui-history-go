// Package aguihistory provides a Go library for managing AG-UI conversation
// thread metadata backed by Google Cloud Spanner.
//
// This library stores thread-level metadata (display names, agent identity,
// run counts, per-user read/pin state) that ADK sessions do not provide.
// Conversation events (messages, tool calls) are NOT stored here; ADK sessions
// are the source of truth for conversation history.
//
// Use [service.NewThreadService] to create a Spanner-backed implementation,
// then register it with the AG-UI launcher via WithThreadService so threads
// are created automatically on each /run_sse request.
package aguihistory
