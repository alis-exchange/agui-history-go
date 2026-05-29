// Package roles registers IAM role-to-permission mappings for
// [service.ThreadService]. Importing this package (directly or via the service
// package) causes the [init] function to register roles with [authz], so they
// are available for all subsequent authorization checks.
//
// Three roles are defined:
//
//   - [Open] — granted to all authenticated callers. Permits ListThreads.
//   - [ThreadViewer] — granted per-thread via IAM policy bindings. Permits
//     GetThread, GetUserThreadState, and UpdateUserThreadState.
//   - [ThreadOwner] — inherits all viewer permissions and adds DeleteThread.
//     Automatically granted to the thread creator.
package roles

import (
	pb "go.alis.build/common/alis/agui/history/v1"
	"go.alis.build/iam/v3/authz"
)

const (
	// Open is the open role granted to all authenticated callers.
	Open = "roles/open"
	// ThreadViewer grants read access to a specific thread and its user state.
	ThreadViewer = "roles/thread.viewer"
	// ThreadOwner grants full access to a thread, including deletion.
	ThreadOwner = "roles/thread.owner"
)

func init() {
	authz.AddOpenRolePermissions(Open, []string{
		pb.ThreadService_ListThreads_FullMethodName,
	})
	viewerPerms := authz.AddRolePermissions(ThreadViewer, []string{
		pb.ThreadService_GetThread_FullMethodName,
		pb.ThreadService_GetUserThreadState_FullMethodName,
		pb.ThreadService_UpdateUserThreadState_FullMethodName,
	})
	authz.AddRolePermissions(ThreadOwner, append(viewerPerms,
		pb.ThreadService_DeleteThread_FullMethodName,
	))
}
