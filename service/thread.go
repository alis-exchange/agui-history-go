package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/spanner"
	pb "go.alis.build/common/alis/agui/history/v1"
	"go.alis.build/agui/history/service/roles"
	auth "go.alis.build/iam/v3"
	"go.alis.build/iam/v3/authz"
	"go.alis.build/validation"
	"google.golang.org/api/iterator"
	"google.golang.org/genai"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	threadRegex          = `^threads/[a-z0-9-]{2,50}$`
	defaultTitleModel    = "gemini-2.5-flash-lite"
	defaultTitleLocation = "global"
)

// SpannerStoreConfig selects the Spanner database and table names used by [ThreadService].
type SpannerStoreConfig struct {
	Project               string
	Instance              string
	Database              string
	DatabaseRole          string
	ThreadsTable          string
	UserThreadStatesTable string
}

// Option configures optional [ThreadService] settings applied in [NewThreadService].
type Option func(*options)

type options struct {
	titleModel    string
	titleLocation string
}

// WithTitleModel sets the Gemini model used to generate thread display names.
// Defaults to "gemini-2.5-flash-lite".
func WithTitleModel(model string) Option {
	return func(o *options) { o.titleModel = model }
}

// WithTitleLocation sets the Vertex AI location for the Gemini client.
// Defaults to "global".
func WithTitleLocation(location string) Option {
	return func(o *options) { o.titleLocation = location }
}

// ThreadService manages AG-UI thread metadata and per-user state via Google
// Cloud Spanner. It implements the generated ThreadServiceServer interface and
// adds [CreateOrUpdateThread] for the AG-UI launcher interceptor.
type ThreadService struct {
	db            *spanner.Client
	threadsTbl    string
	userStatesTbl string
	geminiClient  *genai.Client
	titleModel    string
	pb.UnimplementedThreadServiceServer
}

// NewThreadService constructs a [ThreadService] backed by the Spanner database
// described in config. It opens a Spanner client and, when ALIS_OS_PROJECT (or
// config.Project) is set, a Vertex AI Gemini client for generating thread
// display names. Use [WithTitleModel] and [WithTitleLocation] to override
// the Gemini defaults.
func NewThreadService(ctx context.Context, config *SpannerStoreConfig, opts ...Option) (*ThreadService, error) {
	dbName := fmt.Sprintf("projects/%s/instances/%s/databases/%s", config.Project, config.Instance, config.Database)

	db, err := spanner.NewClientWithConfig(ctx, dbName, spanner.ClientConfig{
		DisableNativeMetrics: true,
		DatabaseRole:         config.DatabaseRole,
	})
	if err != nil {
		return nil, err
	}

	o := &options{
		titleModel:    defaultTitleModel,
		titleLocation: defaultTitleLocation,
	}
	for _, opt := range opts {
		opt(o)
	}

	var geminiClient *genai.Client
	projectID := strings.TrimSpace(os.Getenv("ALIS_OS_PROJECT"))
	if projectID == "" {
		projectID = strings.TrimSpace(config.Project)
	}
	if projectID != "" {
		geminiClient, err = genai.NewClient(ctx, &genai.ClientConfig{
			Backend:  genai.BackendVertexAI,
			Project:  projectID,
			Location: o.titleLocation,
		})
		if err != nil {
			return nil, err
		}
	}

	return &ThreadService{
		db:            db,
		threadsTbl:    config.ThreadsTable,
		userStatesTbl: config.UserThreadStatesTable,
		geminiClient:  geminiClient,
		titleModel:    o.titleModel,
	}, nil
}

// GetThread returns a single thread by name. The caller must have GetThread
// permission on the thread's IAM policy (roles/thread.viewer or
// roles/thread.owner).
func (s *ThreadService) GetThread(ctx context.Context, req *pb.GetThreadRequest) (*pb.Thread, error) {
	validator := validation.NewValidator()
	validator.String("name", req.GetName()).IsPopulated().Matches(threadRegex)
	if err := validator.Validate(); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	az, _, err := authorizerFromCtx(ctx)
	if err != nil {
		return nil, err
	}

	thread, policy, err := s.readThread(ctx, req.GetName())
	if err != nil {
		return nil, err
	}

	if !canGetThread(az, policy) {
		return nil, status.Errorf(codes.PermissionDenied, "you do not have permission to access this resource")
	}

	return thread, nil
}

// DeleteThread removes a thread and all associated user thread states. The
// caller must have DeleteThread permission on the thread's IAM policy
// (roles/thread.owner).
func (s *ThreadService) DeleteThread(ctx context.Context, req *pb.DeleteThreadRequest) (*emptypb.Empty, error) {
	validator := validation.NewValidator()
	validator.String("name", req.GetName()).IsPopulated().Matches(threadRegex)
	if err := validator.Validate(); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	az, _, err := authorizerFromCtx(ctx)
	if err != nil {
		return nil, err
	}

	_, policy, err := s.readThread(ctx, req.GetName())
	if err != nil {
		return nil, err
	}

	if !canDeleteThread(az, policy) {
		return nil, status.Errorf(codes.PermissionDenied, "you do not have permission to access this resource")
	}

	userStatesPrefix := req.GetName() + "/userStates/%"
	if _, err := s.db.ReadWriteTransaction(ctx, func(ctx context.Context, rwt *spanner.ReadWriteTransaction) error {
		if s.userStatesTbl != "" {
			if _, err := rwt.Update(ctx, spanner.Statement{
				SQL:    fmt.Sprintf(`DELETE FROM %s WHERE key LIKE @userStatesPrefix`, s.userStatesTbl),
				Params: map[string]any{"userStatesPrefix": userStatesPrefix},
			}); err != nil {
				return status.Errorf(codes.Internal, "deleting user thread states for %q: %v", req.GetName(), err)
			}
		}
		if _, err := rwt.Update(ctx, spanner.Statement{
			SQL:    fmt.Sprintf(`DELETE FROM %s WHERE key = @name`, s.threadsTbl),
			Params: map[string]any{"name": req.GetName()},
		}); err != nil {
			return status.Errorf(codes.Internal, "deleting thread %q: %v", req.GetName(), err)
		}
		return nil
	}); err != nil {
		return nil, err
	}

	return &emptypb.Empty{}, nil
}

// ListThreads returns threads the caller has access to, ordered by
// last_activity_time descending. Results are paginated with cursor-based
// tokens. A SQL-level member prefilter limits the Spanner scan to rows where
// the caller appears in at least one policy binding; a per-row
// HasPermission(GetThread) check provides defense-in-depth. Privileged
// identities (system/admin) bypass the prefilter and see all threads.
func (s *ThreadService) ListThreads(ctx context.Context, req *pb.ListThreadsRequest) (*pb.ListThreadsResponse, error) {
	az, identity, err := authorizerFromCtx(ctx)
	if err != nil {
		return nil, err
	}

	if !az.HasPermission(pb.ThreadService_ListThreads_FullMethodName) {
		return nil, status.Errorf(codes.PermissionDenied, "you do not have permission to access this resource")
	}

	limit := int(req.GetPageSize())
	if limit < 1 || limit > 100 {
		limit = 100
	}

	const tsExpr = `TIMESTAMP_ADD(TIMESTAMP_SECONDS(t.Thread.last_activity_time.seconds),INTERVAL CAST(FLOOR(IFNULL(t.Thread.last_activity_time.nanos,0) / 1000) AS INT64) MICROSECOND)`

	statement := spanner.NewStatement(`SELECT Thread, Policy FROM ` + s.threadsTbl + ` AS t`)
	var conditions []string

	members := policyMembers(identity)
	if members != nil {
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM UNNEST(t.Policy.bindings) AS binding
			CROSS JOIN UNNEST(binding.members) AS member
			WHERE binding.role IN UNNEST(@roles)
			AND member IN UNNEST(@members)
		)`)
		statement.Params["members"] = members
		statement.Params["roles"] = []string{roles.ThreadViewer, roles.ThreadOwner}
	}
	if req.GetAgentId() != "" {
		conditions = append(conditions, `t.Thread.agent_id = @agentId`)
		statement.Params["agentId"] = req.GetAgentId()
	}
	if req.GetPageToken() != "" {
		cursor, cursorErr := decodeListCursor(req.GetPageToken())
		if cursorErr != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid page token")
		}
		cursorTS, parseErr := time.Parse(time.RFC3339Nano, cursor.TS)
		if parseErr != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid page token")
		}
		conditions = append(conditions, `(`+tsExpr+`, t.key) < (@cursorTS, @cursorKey)`)
		statement.Params["cursorTS"] = cursorTS
		statement.Params["cursorKey"] = cursor.Key
	}
	if len(conditions) > 0 {
		statement.SQL += ` WHERE ` + strings.Join(conditions, ` AND `)
	}
	statement.SQL += ` ORDER BY ` + tsExpr + ` DESC, t.key DESC`

	// Cap the number of rows decoded per RPC to bound Spanner read cost.
	// With the role-aware SQL prefilter most rows should pass canGetThread,
	// so this cap is rarely hit. When it is, the client gets a short page
	// with a continuation token.
	const maxScanRows = 2000

	var threads []*pb.Thread
	txn := s.db.ReadOnlyTransaction()
	defer txn.Close()
	iter := txn.Query(ctx, statement)
	defer iter.Stop()

	// Track the last decoded thread (authorized or not) so the cursor can
	// resume from the correct position even when the final SQL rows were
	// filtered out by canGetThread.
	var lastRow *pb.Thread
	rowsScanned := 0
	exhausted := false
	for len(threads) < limit && rowsScanned < maxScanRows {
		row, iterErr := iter.Next()
		if iterErr == iterator.Done {
			exhausted = true
			break
		}
		if iterErr != nil {
			return nil, status.Errorf(codes.Internal, "querying database: %v", iterErr)
		}
		rowsScanned++
		thread, policy, decodeErr := decodeThreadRow(row)
		if decodeErr != nil {
			return nil, status.Errorf(codes.Internal, "decoding thread: %v", decodeErr)
		}
		lastRow = thread
		if !canGetThread(az, policy) {
			continue
		}
		threads = append(threads, thread)
	}

	nextPageToken := ""
	if !exhausted && lastRow != nil {
		nextPageToken = encodeListCursor(lastRow.GetLastActivityTime().AsTime(), lastRow.GetName())
	}

	userStates, err := s.readUserThreadStates(ctx, currentUserResource(identity), threads)
	if err != nil {
		return nil, err
	}

	views := make([]*pb.ThreadView, 0, len(threads))
	for _, thread := range threads {
		state := userStates[thread.GetName()]
		view := &pb.ThreadView{
			Thread: thread,
		}
		if state != nil {
			view.ReadRunCount = state.GetReadRunCount()
			view.Pinned = state.GetPinned()
			view.PinnedTime = state.GetPinnedTime()
		}
		view.HasUnread = thread.GetRunCount() > view.GetReadRunCount()
		views = append(views, view)
	}

	return &pb.ListThreadsResponse{
		Threads:       views,
		NextPageToken: nextPageToken,
	}, nil
}

// GetUserThreadState returns the caller's per-user state (read count, pinned
// status) for a thread. The caller must have GetUserThreadState permission on
// the parent thread's IAM policy and may only access their own state.
func (s *ThreadService) GetUserThreadState(ctx context.Context, req *pb.GetUserThreadStateRequest) (*pb.UserThreadState, error) {
	threadName, userID, err := parseUserThreadStateName(req.GetName())
	if err != nil {
		return nil, err
	}

	az, identity, err := authorizerFromCtx(ctx)
	if err != nil {
		return nil, err
	}

	_, policy, err := s.readThread(ctx, threadName)
	if err != nil {
		return nil, err
	}
	if !canGetUserThreadState(az, policy) {
		return nil, status.Errorf(codes.PermissionDenied, "you do not have permission to access this resource")
	}

	userResource, err := requireCurrentUserResource(identity)
	if err != nil {
		return nil, err
	}
	if userID != userIDFromResource(userResource) {
		return nil, status.Error(codes.PermissionDenied, "you may only access your own thread state")
	}

	state, err := s.readUserThreadState(ctx, req.GetName())
	if err != nil {
		return nil, err
	}
	return state, nil
}

// UpdateUserThreadState updates the caller's per-user state for a thread
// (read count, pinned status). The caller must have UpdateUserThreadState
// permission on the parent thread's IAM policy and may only modify their
// own state. Supports field masks; when omitted, all fields are updated.
func (s *ThreadService) UpdateUserThreadState(ctx context.Context, req *pb.UpdateUserThreadStateRequest) (*pb.UserThreadState, error) {
	validator := validation.NewValidator()
	validator.MessageIsPopulated("user_thread_state", req.GetUserThreadState() != nil)
	if err := validator.Validate(); err != nil {
		return nil, err
	}

	threadName := req.GetUserThreadState().GetThread()
	if threadName == "" {
		parsedThread, _, parseErr := parseUserThreadStateName(req.GetUserThreadState().GetName())
		if parseErr != nil {
			return nil, status.Error(codes.InvalidArgument, "user_thread_state.thread is required")
		}
		threadName = parsedThread
	}
	if !validThreadName(threadName) {
		return nil, status.Error(codes.InvalidArgument, "invalid user_thread_state.thread")
	}

	az, identity, err := authorizerFromCtx(ctx)
	if err != nil {
		return nil, err
	}

	thread, policy, err := s.readThread(ctx, threadName)
	if err != nil {
		return nil, err
	}
	if !canUpdateUserThreadState(az, policy) {
		return nil, status.Errorf(codes.PermissionDenied, "you do not have permission to access this resource")
	}

	userResource, err := requireCurrentUserResource(identity)
	if err != nil {
		return nil, err
	}
	if req.GetUserThreadState().GetUser() != "" && req.GetUserThreadState().GetUser() != userResource {
		return nil, status.Error(codes.PermissionDenied, "you may only update your own thread state")
	}
	stateName := req.GetUserThreadState().GetName()
	if stateName == "" {
		stateName = userThreadStateName(threadName, userResource)
	}
	parsedThread, userID, err := parseUserThreadStateName(stateName)
	if err != nil {
		return nil, err
	}
	if parsedThread != threadName || userID != userIDFromResource(userResource) {
		return nil, status.Error(codes.PermissionDenied, "user_thread_state.name must match the current user and thread")
	}

	now := timestamppb.Now()
	state := &pb.UserThreadState{
		Name:       stateName,
		Thread:     threadName,
		User:       userResource,
		UpdateTime: now,
	}
	existing, err := s.readUserThreadState(ctx, stateName)
	if err != nil && status.Code(err) != codes.NotFound {
		return nil, err
	}
	if existing != nil {
		state.ReadRunCount = existing.GetReadRunCount()
		state.LastReadTime = existing.GetLastReadTime()
		state.Pinned = existing.GetPinned()
		state.PinnedTime = existing.GetPinnedTime()
	}

	updatePaths := req.GetUpdateMask().GetPaths()
	if len(updatePaths) == 0 {
		updatePaths = []string{"read_run_count", "last_read_time", "pinned", "pinned_time"}
	}
	for _, path := range updatePaths {
		switch path {
		case "read_run_count":
			if req.GetUserThreadState().GetReadRunCount() < 0 {
				return nil, status.Error(codes.InvalidArgument, "read_run_count must be non-negative")
			}
			if req.GetUserThreadState().GetReadRunCount() > thread.GetRunCount() {
				return nil, status.Error(codes.InvalidArgument, "read_run_count cannot exceed run_count")
			}
			state.ReadRunCount = req.GetUserThreadState().GetReadRunCount()
		case "last_read_time":
			state.LastReadTime = req.GetUserThreadState().GetLastReadTime()
		case "pinned":
			state.Pinned = req.GetUserThreadState().GetPinned()
			if !state.GetPinned() {
				state.PinnedTime = nil
			} else if state.GetPinnedTime() == nil {
				state.PinnedTime = now
			}
		case "pinned_time":
			state.PinnedTime = req.GetUserThreadState().GetPinnedTime()
		default:
			return nil, status.Errorf(codes.InvalidArgument, "unsupported update_mask path %q", path)
		}
	}
	if state.GetPinned() && state.GetPinnedTime() == nil {
		state.PinnedTime = now
	}
	if !state.GetPinned() {
		state.PinnedTime = nil
	}

	mutation := spanner.InsertOrUpdate(s.userStatesTbl, []string{"key", "UserThreadState"}, []any{state.GetName(), state})
	if _, err := s.db.Apply(ctx, []*spanner.Mutation{mutation}); err != nil {
		return nil, status.Errorf(codes.Internal, "writing user thread state %q: %v", state.GetName(), err)
	}
	return state, nil
}

// CreateOrUpdateThreadRequest is the input for [ThreadService.CreateOrUpdateThread].
type CreateOrUpdateThreadRequest struct {
	ThreadID         string
	AgentID          string
	AgentDisplayName string
	UserMessageText  string
}

// CreateOrUpdateThread upserts thread metadata. On first call for a thread, it
// creates the record with a Gemini-generated display name and grants
// roles/thread.owner to the caller. On subsequent calls, it increments
// run_count and updates last_activity_time.
func (s *ThreadService) CreateOrUpdateThread(ctx context.Context, req *CreateOrUpdateThreadRequest) error {
	if req.ThreadID == "" {
		return status.Error(codes.InvalidArgument, "thread_id is required")
	}

	identity, err := identityFromCtx(ctx)
	if err != nil {
		return status.Errorf(codes.Unauthenticated, "failed to get identity from context: %s", err.Error())
	}
	az, err := authz.New(identity)
	if err != nil {
		return status.Errorf(codes.Internal, "failed to create authorizer: %s", err.Error())
	}

	threadName := fmt.Sprintf("threads/%s", req.ThreadID)
	now := time.Now().UTC()

	if _, err := s.db.ReadWriteTransaction(ctx, func(ctx context.Context, rwt *spanner.ReadWriteTransaction) error {
		thread := &pb.Thread{}
		policy := &iampb.Policy{}
		threadExists := true

		row, err := rwt.ReadRow(ctx, s.threadsTbl, spanner.Key{threadName}, []string{"Thread", "Policy"})
		if err != nil {
			if spanner.ErrCode(err) != codes.NotFound {
				return status.Errorf(codes.Internal, "reading thread %q: %v", threadName, err)
			}
			threadExists = false
			displayName := s.generateThreadDisplayName(ctx, req.UserMessageText, now)
			thread = &pb.Thread{
				Name:             threadName,
				DisplayName:      displayName,
				AgentId:          req.AgentID,
				AgentDisplayName: req.AgentDisplayName,
				RunCount:         1,
				LastActivityTime: timestamppb.New(now),
				CreateTime:       timestamppb.New(now),
			}
			policy = &iampb.Policy{
				Bindings: []*iampb.Binding{
					{
						Role:    roles.ThreadOwner,
						Members: []string{identity.PolicyMember()},
					},
				},
			}
		} else if err := row.Columns(thread, policy); err != nil {
			return status.Errorf(codes.Internal, "decoding thread %q: %v", threadName, err)
		}

		if threadExists && !canGetThread(az, policy) {
			return status.Errorf(codes.PermissionDenied, "you do not have permission to access this resource")
		}

		if threadExists {
			thread.RunCount++
			thread.LastActivityTime = timestamppb.New(now)
			if err := rwt.BufferWrite([]*spanner.Mutation{
				spanner.Update(s.threadsTbl, []string{"key", "Thread"}, []any{thread.GetName(), thread}),
			}); err != nil {
				return status.Errorf(codes.Internal, "updating thread %q: %v", threadName, err)
			}
		} else {
			if err := rwt.BufferWrite([]*spanner.Mutation{
				spanner.Insert(s.threadsTbl, []string{"key", "Thread", "Policy"}, []any{thread.GetName(), thread, policy}),
			}); err != nil {
				return status.Errorf(codes.Internal, "inserting thread %q: %v", threadName, err)
			}
		}
		return nil
	}); err != nil {
		return err
	}

	return nil
}

func (s *ThreadService) readThread(ctx context.Context, name string) (*pb.Thread, *iampb.Policy, error) {
	row, err := s.db.Single().ReadRow(ctx, s.threadsTbl, spanner.Key{name}, []string{"Thread", "Policy"})
	if err != nil {
		if spanner.ErrCode(err) == codes.NotFound {
			return nil, nil, status.Errorf(codes.NotFound, "thread %q not found", name)
		}
		return nil, nil, status.Errorf(codes.Internal, "reading thread %q: %v", name, err)
	}
	thread, policy, err := decodeThreadRow(row)
	if err != nil {
		return nil, nil, status.Errorf(codes.Internal, "decoding thread %q: %v", name, err)
	}
	return thread, policy, nil
}

func decodeThreadRow(row *spanner.Row) (*pb.Thread, *iampb.Policy, error) {
	thread := &pb.Thread{}
	policy := &iampb.Policy{}
	if err := row.Columns(thread, policy); err != nil {
		return nil, nil, err
	}
	return thread, policy, nil
}

func (s *ThreadService) readUserThreadStates(ctx context.Context, userResource string, threads []*pb.Thread) (map[string]*pb.UserThreadState, error) {
	states := map[string]*pb.UserThreadState{}
	if s.userStatesTbl == "" || userResource == "" || len(threads) == 0 {
		return states, nil
	}

	keys := make([]string, 0, len(threads))
	for _, thread := range threads {
		keys = append(keys, userThreadStateName(thread.GetName(), userResource))
	}
	statement := spanner.NewStatement(fmt.Sprintf(`SELECT UserThreadState FROM %s WHERE key IN UNNEST(@keys)`, s.userStatesTbl))
	statement.Params["keys"] = keys

	txn := s.db.ReadOnlyTransaction()
	defer txn.Close()
	iterator := txn.Query(ctx, statement)
	if err := iterator.Do(func(r *spanner.Row) error {
		state := &pb.UserThreadState{}
		if err := r.ColumnByName("UserThreadState", state); err != nil {
			return err
		}
		states[state.GetThread()] = state
		return nil
	}); err != nil {
		return nil, status.Errorf(codes.Internal, "querying user thread states: %v", err)
	}
	return states, nil
}

func (s *ThreadService) readUserThreadState(ctx context.Context, name string) (*pb.UserThreadState, error) {
	row, err := s.db.Single().ReadRow(ctx, s.userStatesTbl, spanner.Key{name}, []string{"UserThreadState"})
	if err != nil {
		if spanner.ErrCode(err) == codes.NotFound {
			return nil, status.Errorf(codes.NotFound, "user thread state %q not found", name)
		}
		return nil, status.Errorf(codes.Internal, "reading user thread state %q: %v", name, err)
	}
	state := &pb.UserThreadState{}
	if err := row.ColumnByName("UserThreadState", state); err != nil {
		return nil, status.Errorf(codes.Internal, "decoding user thread state %q: %v", name, err)
	}
	return state, nil
}

func parseUserThreadStateName(name string) (threadName string, userID string, err error) {
	if name == "" {
		return "", "", status.Error(codes.InvalidArgument, "name is required")
	}
	parts := strings.Split(name, "/")
	if len(parts) != 4 || parts[0] != "threads" || parts[2] != "userStates" || parts[1] == "" || parts[3] == "" {
		return "", "", status.Errorf(codes.InvalidArgument, "invalid user thread state name %q", name)
	}
	return strings.Join(parts[:2], "/"), parts[3], nil
}

func userThreadStateName(threadName, userResource string) string {
	return fmt.Sprintf("%s/userStates/%s", threadName, userIDFromResource(userResource))
}

func userIDFromResource(userResource string) string {
	return strings.TrimPrefix(userResource, "users/")
}

// identityFromCtx returns the caller identity from context (gRPC interceptor or
// HTTP middleware) or from incoming metadata (in-process callers such as the
// AG-UI launcher that inject x-alis-identity without going through the transport).
func identityFromCtx(ctx context.Context) (*auth.Identity, error) {
	if identity, err := auth.FromContext(ctx); err == nil {
		return identity, nil
	}
	return auth.FromIncomingMetadata(ctx)
}

// authorizerFromCtx extracts the caller identity from ctx and returns an
// Authorizer pre-loaded with any roles embedded in the identity's IAM policy.
func authorizerFromCtx(ctx context.Context) (*authz.Authorizer, *auth.Identity, error) {
	identity, err := identityFromCtx(ctx)
	if err != nil {
		return nil, nil, status.Errorf(codes.Unauthenticated, "failed to get identity from context: %s", err.Error())
	}
	az, err := authz.New(identity)
	if err != nil {
		return nil, nil, status.Errorf(codes.Internal, "failed to create authorizer: %s", err.Error())
	}
	return az, identity, nil
}

// policyMembers returns all member strings that could match the identity in an
// IAM policy binding. Returns nil for privileged identities (they bypass
// policy checks entirely).
func policyMembers(identity *auth.Identity) []string {
	if identity.IsPrivileged() {
		return nil
	}
	members := []string{identity.PolicyMember()}
	if identity.Email != "" {
		members = append(members, "email:"+identity.Email)
		if _, domain, ok := strings.Cut(identity.Email, "@"); ok {
			members = append(members, "domain:"+domain)
		}
	}
	for _, gid := range identity.GroupIDs {
		members = append(members, "group:"+gid)
	}
	return members
}

// listCursor is the serialized form of a ListThreads page token. It encodes
// the last_activity_time and thread name of the final row on the current page,
// enabling cursor-based pagination without Spanner OFFSET.
type listCursor struct {
	TS  string `json:"ts"`
	Key string `json:"key"`
}

func encodeListCursor(ts time.Time, key string) string {
	c := listCursor{TS: ts.UTC().Format(time.RFC3339Nano), Key: key}
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeListCursor(token string) (*listCursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, err
	}
	var c listCursor
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	if c.TS == "" || c.Key == "" {
		return nil, fmt.Errorf("invalid cursor")
	}
	return &c, nil
}

func canGetThread(az *authz.Authorizer, policy *iampb.Policy) bool {
	return az.HasPermission(pb.ThreadService_GetThread_FullMethodName, policy)
}

func canDeleteThread(az *authz.Authorizer, policy *iampb.Policy) bool {
	return az.HasPermission(pb.ThreadService_DeleteThread_FullMethodName, policy)
}

func canGetUserThreadState(az *authz.Authorizer, policy *iampb.Policy) bool {
	return az.HasPermission(pb.ThreadService_GetUserThreadState_FullMethodName, policy)
}

func canUpdateUserThreadState(az *authz.Authorizer, policy *iampb.Policy) bool {
	return az.HasPermission(pb.ThreadService_UpdateUserThreadState_FullMethodName, policy)
}

func currentUserResource(identity *auth.Identity) string {
	if identity == nil || identity.IsSystem() {
		return ""
	}
	return identity.User()
}

func requireCurrentUserResource(identity *auth.Identity) (string, error) {
	userResource := currentUserResource(identity)
	if userResource == "" {
		return "", status.Error(codes.Unauthenticated, "an authenticated user is required")
	}
	return userResource, nil
}

func validThreadName(name string) bool {
	validator := validation.NewValidator()
	validator.String("name", name).IsPopulated().Matches(threadRegex)
	return validator.Validate() == nil
}

func (s *ThreadService) generateThreadDisplayName(ctx context.Context, userMessageText string, now time.Time) string {
	fallback := now.Format(time.RFC3339)
	if userMessageText == "" {
		return fallback
	}
	if s.geminiClient == nil {
		return fallback
	}

	prompt := fmt.Sprintf(`You create short conversation titles.
Return a concise title of at most 8 words.
Do not use quotes or punctuation unless necessary.
User message:
%s`, userMessageText)

	resp, err := s.geminiClient.Models.GenerateContent(ctx, s.titleModel, genai.Text(prompt), &genai.GenerateContentConfig{})
	if err != nil {
		return fallback
	}

	title := strings.TrimSpace(resp.Text())
	title = strings.Trim(title, `"'`)
	if title == "" {
		return fallback
	}
	return title
}
