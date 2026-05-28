package service

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/spanner"
	pb "go.alis.build/common/alis/agui/history/v1"
	"go.alis.build/iam/v2"
	"go.alis.build/validation"
	"google.golang.org/genai"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	threadRegex          = `^threads/[a-z0-9-]{2,50}$`
	roleOpen             = "roles/open"
	roleThreadViewer     = "roles/thread.viewer"
	roleThreadAdmin      = "roles/thread.admin"
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

	// TitleModel is the Gemini model used to generate thread display names.
	// Defaults to "gemini-2.5-flash-lite" if empty.
	TitleModel string
	// TitleLocation is the Vertex AI location for the Gemini client.
	// Defaults to "global" if empty.
	TitleLocation string
}

// ThreadService is an implementation for managing AG-UI thread metadata via Google Cloud Spanner.
type ThreadService struct {
	db            *spanner.Client
	threadsTbl    string
	userStatesTbl string
	geminiClient  *genai.Client
	titleModel    string
	authorizer    *iam.IAM
	pb.UnimplementedThreadServiceServer
}

// NewThreadService constructs a [ThreadService] with a Spanner client and IAM authorizer.
func NewThreadService(ctx context.Context, config *SpannerStoreConfig) (*ThreadService, error) {
	dbName := fmt.Sprintf("projects/%s/instances/%s/databases/%s", config.Project, config.Instance, config.Database)

	db, err := spanner.NewClientWithConfig(ctx, dbName, spanner.ClientConfig{
		DisableNativeMetrics: true,
		DatabaseRole:         config.DatabaseRole,
	})
	if err != nil {
		return nil, err
	}

	authorizer, err := iam.New([]*iam.Role{
		{
			Name: roleOpen,
			Permissions: []string{
				pb.ThreadService_ListThreads_FullMethodName,
				pb.ThreadService_GetUserThreadState_FullMethodName,
				pb.ThreadService_UpdateUserThreadState_FullMethodName,
			},
			AllUsers: true,
		},
		{
			Name: roleThreadViewer,
			Permissions: []string{
				pb.ThreadService_GetThread_FullMethodName,
			},
			AllUsers: false,
		},
		{
			Name: roleThreadAdmin,
			Permissions: []string{
				pb.ThreadService_GetThread_FullMethodName,
				pb.ThreadService_DeleteThread_FullMethodName,
			},
			AllUsers: false,
		},
	})
	if err != nil {
		return nil, err
	}

	titleModel := config.TitleModel
	if titleModel == "" {
		titleModel = defaultTitleModel
	}
	titleLocation := config.TitleLocation
	if titleLocation == "" {
		titleLocation = defaultTitleLocation
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
			Location: titleLocation,
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
		titleModel:    titleModel,
		authorizer:    authorizer,
	}, nil
}

// GetThread implements the ThreadService.GetThread method.
func (s *ThreadService) GetThread(ctx context.Context, req *pb.GetThreadRequest) (*pb.Thread, error) {
	az, ctx, err := s.authorizer.NewAuthorizer(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create authorizer: %s", err.Error())
	}

	validator := validation.NewValidator()
	validator.String("name", req.GetName()).IsPopulated().Matches(threadRegex)
	if err := validator.Validate(); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	thread, policy, err := s.readThread(ctx, req.GetName())
	if err != nil {
		return nil, err
	}

	az.AddPolicy(policy)
	if !az.HasAccess(pb.ThreadService_GetThread_FullMethodName) {
		return nil, status.Errorf(codes.PermissionDenied, "you do not have permission to access this resource")
	}

	return thread, nil
}

// DeleteThread implements the ThreadService.DeleteThread method.
func (s *ThreadService) DeleteThread(ctx context.Context, req *pb.DeleteThreadRequest) (*emptypb.Empty, error) {
	validator := validation.NewValidator()
	validator.String("name", req.GetName()).IsPopulated().Matches(threadRegex)
	if err := validator.Validate(); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	az, ctx, err := s.authorizer.NewAuthorizer(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create authorizer: %s", err.Error())
	}

	_, policy, err := s.readThread(ctx, req.GetName())
	if err != nil {
		return nil, err
	}

	az.AddPolicy(policy)
	if err := az.AuthorizeRpc(); err != nil {
		return nil, err
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

// ListThreads implements the ThreadService.ListThreads method.
func (s *ThreadService) ListThreads(ctx context.Context, req *pb.ListThreadsRequest) (*pb.ListThreadsResponse, error) {
	az, ctx, err := s.authorizer.NewAuthorizer(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create authorizer: %s", err.Error())
	}

	if err = az.AuthorizeRpc(); err != nil {
		return nil, err
	}

	statement := spanner.NewStatement(`select Thread, Policy from ` + s.threadsTbl + " as t")
	if !az.Identity.IsDeploymentServiceAccount() {
		statement.SQL += `
			WHERE EXISTS (
			SELECT 1
			FROM UNNEST(t.Policy.bindings) AS binding
			CROSS JOIN UNNEST(binding.members) AS member
			WHERE member = @member
			)`
		statement.Params["member"] = az.Identity.PolicyMember()
	}
	if req.GetAgentId() != "" {
		if strings.Contains(statement.SQL, "WHERE") {
			statement.SQL += ` AND t.Thread.agent_id = @agentId`
		} else {
			statement.SQL += ` WHERE t.Thread.agent_id = @agentId`
		}
		statement.Params["agentId"] = req.GetAgentId()
	}
	statement.SQL += ` order by t.Thread.last_activity_time DESC limit @limit offset @offset;`

	limit := int(req.GetPageSize())
	if limit < 1 || limit > 100 {
		limit = 100
	}
	statement.Params["limit"] = limit
	offset := 0
	if req.GetPageToken() != "" {
		offset, err = strconv.Atoi(req.GetPageToken())
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid page token")
		}
	}
	statement.Params["offset"] = offset

	var threads []*pb.Thread
	iterator := s.db.ReadOnlyTransaction().Query(ctx, statement)
	if err := iterator.Do(func(r *spanner.Row) error {
		thread, _, decodeErr := decodeThreadRow(r)
		if decodeErr != nil {
			return decodeErr
		}
		threads = append(threads, thread)
		return nil
	}); err != nil {
		return nil, status.Errorf(codes.Internal, "querying database: %v", err)
	}

	userStates, err := s.readUserThreadStates(ctx, currentUserResource(az.Identity), threads)
	if err != nil {
		return nil, err
	}

	nextPageToken := ""
	if len(threads) == limit {
		nextPageToken = fmt.Sprintf("%d", offset+limit)
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

// GetUserThreadState implements the ThreadService.GetUserThreadState method.
func (s *ThreadService) GetUserThreadState(ctx context.Context, req *pb.GetUserThreadStateRequest) (*pb.UserThreadState, error) {
	az, ctx, err := s.authorizer.NewAuthorizer(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create authorizer: %s", err.Error())
	}
	if err = az.AuthorizeRpc(); err != nil {
		return nil, err
	}

	threadName, userID, err := parseUserThreadStateName(req.GetName())
	if err != nil {
		return nil, err
	}
	userResource, err := requireCurrentUserResource(az.Identity)
	if err != nil {
		return nil, err
	}
	if userID != userIDFromResource(userResource) {
		return nil, status.Error(codes.PermissionDenied, "you may only access your own thread state")
	}

	_, policy, err := s.readThread(ctx, threadName)
	if err != nil {
		return nil, err
	}
	az.AddPolicy(policy)
	if !az.HasAccess(pb.ThreadService_ListThreads_FullMethodName) {
		return nil, status.Error(codes.PermissionDenied, "you do not have permission to access this thread")
	}

	state, err := s.readUserThreadState(ctx, req.GetName())
	if err != nil {
		return nil, err
	}
	return state, nil
}

// UpdateUserThreadState implements the ThreadService.UpdateUserThreadState method.
func (s *ThreadService) UpdateUserThreadState(ctx context.Context, req *pb.UpdateUserThreadStateRequest) (*pb.UserThreadState, error) {
	validator := validation.NewValidator()
	validator.MessageIsPopulated("user_thread_state", req.GetUserThreadState() != nil)
	if err := validator.Validate(); err != nil {
		return nil, err
	}

	az, ctx, err := s.authorizer.NewAuthorizer(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create authorizer: %s", err.Error())
	}
	if err = az.AuthorizeRpc(); err != nil {
		return nil, err
	}

	userResource, err := requireCurrentUserResource(az.Identity)
	if err != nil {
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

	thread, policy, err := s.readThread(ctx, threadName)
	if err != nil {
		return nil, err
	}
	az.AddPolicy(policy)
	if !az.HasAccess(pb.ThreadService_ListThreads_FullMethodName) {
		return nil, status.Error(codes.PermissionDenied, "you do not have permission to access this thread")
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
// roles/thread.admin to the caller. On subsequent calls, it increments
// run_count and updates last_activity_time.
func (s *ThreadService) CreateOrUpdateThread(ctx context.Context, req *CreateOrUpdateThreadRequest) error {
	if req.ThreadID == "" {
		return status.Error(codes.InvalidArgument, "thread_id is required")
	}

	az, ctx, err := s.authorizer.NewAuthorizer(ctx)
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
						Role:    roleThreadAdmin,
						Members: []string{az.Identity.PolicyMember()},
					},
				},
			}
		} else if err := row.Columns(thread, policy); err != nil {
			return status.Errorf(codes.Internal, "decoding thread %q: %v", threadName, err)
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

	iterator := s.db.ReadOnlyTransaction().Query(ctx, statement)
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

func currentUserResource(identity *iam.Identity) string {
	if identity == nil || identity.IsDeploymentServiceAccount() {
		return ""
	}
	if identity.Id() != "" {
		return "users/" + identity.Id()
	}
	return ""
}

func requireCurrentUserResource(identity *iam.Identity) (string, error) {
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

