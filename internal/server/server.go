package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/royal007a/01agent/internal/agentregistry"
	"github.com/royal007a/01agent/internal/approvalstore"
	"github.com/royal007a/01agent/internal/attention"
	"github.com/royal007a/01agent/internal/engine"
	"github.com/royal007a/01agent/internal/provider"
	"github.com/royal007a/01agent/internal/schema"
	"github.com/royal007a/01agent/internal/sessionstore"
	"github.com/royal007a/01agent/internal/taskstore"
	"github.com/royal007a/01agent/internal/tools"
	"github.com/royal007a/01agent/internal/workitem"
)

const maxRequestBytes = 1 << 20

type Config struct {
	Version          string
	Token            string
	WorkDir          string
	Provider         provider.LLMProvider
	Registry         tools.Registry
	EnableThinking   bool
	PlanMode         bool
	MaxTurns         int
	MaxTokens        int64
	MaxRepeatedCall  int
	RunTimeout       time.Duration
	MaxConcurrent    int
	Store            engine.RunStore
	Compactor        engine.ContextCompactor
	InputQueue       engine.InputQueue
	InputEnqueuer    engine.InputEnqueuer
	Tasks            *taskstore.Store
	WorkItems        *workitem.Store
	Attention        *attention.Store
	Agents           *agentregistry.Store
	Sessions         *sessionstore.Store
	Approvals        *approvalstore.Store
	ReadinessTTL     time.Duration
	ReadinessTimeout time.Duration
}

type Handler struct {
	config    Config
	semaphore chan struct{}
	mux       *http.ServeMux
	readyMu   sync.Mutex
	readyAt   time.Time
	readyErr  error
}

type runRequest struct {
	RunID         string   `json:"run_id,omitempty"`
	Prompt        string   `json:"prompt"`
	Thinking      *bool    `json:"thinking,omitempty"`
	PlanMode      *bool    `json:"plan_mode,omitempty"`
	ResumeRunID   string   `json:"resume_run_id,omitempty"`
	ApprovedTools []string `json:"approved_tools,omitempty"`
}

type sessionTurnRequest struct {
	OperationID   string   `json:"operation_id"`
	Prompt        string   `json:"prompt"`
	Thinking      *bool    `json:"thinking,omitempty"`
	PlanMode      *bool    `json:"plan_mode,omitempty"`
	ApprovedTools []string `json:"approved_tools,omitempty"`
}

type enqueueRequest struct {
	ID      string           `json:"id,omitempty"`
	Kind    engine.InputKind `json:"kind,omitempty"`
	Content string           `json:"content"`
}

type createTaskRequest struct {
	ID           string `json:"id,omitempty"`
	ParentRunID  string `json:"parent_run_id"`
	ParentTurnID string `json:"parent_turn_id,omitempty"`
	Title        string `json:"title"`
}

type taskEventRequest struct {
	Action  string `json:"action"`
	OwnerID string `json:"owner_id,omitempty"`
	Output  string `json:"output,omitempty"`
	Failure string `json:"failure,omitempty"`
}

type workItemActionRequest struct {
	Action           string                 `json:"action"`
	OperationID      string                 `json:"operation_id"`
	ExpectedRevision int64                  `json:"expected_revision"`
	ActorID          string                 `json:"actor_id,omitempty"`
	OwnerID          string                 `json:"owner_id,omitempty"`
	LeaseID          string                 `json:"lease_id,omitempty"`
	TTLSeconds       int64                  `json:"ttl_seconds,omitempty"`
	Reason           string                 `json:"reason,omitempty"`
	Requirements     []workitem.Requirement `json:"requirements,omitempty"`
	Scope            workitem.Scope         `json:"scope,omitempty"`
	StopConditions   []string               `json:"stop_conditions,omitempty"`
	Gate             workitem.GateSpec      `json:"gate,omitempty"`
	Handoff          workitem.Handoff       `json:"handoff,omitempty"`
	GateResult       workitem.GateResult    `json:"gate_result,omitempty"`
}

type inboxClaimRequest struct {
	OperationID string `json:"operation_id"`
	LeaseID     string `json:"lease_id,omitempty"`
	TTLSeconds  int64  `json:"ttl_seconds"`
}

type inboxActionRequest struct {
	Action      string `json:"action"`
	OperationID string `json:"operation_id"`
	AgentID     string `json:"agent_id"`
	LeaseID     string `json:"lease_id"`
}

type clearWorkMarkRequest struct {
	OperationID string `json:"operation_id"`
}

type approvalDecisionRequest struct {
	Decision approvalstore.State `json:"decision"`
	Actor    string              `json:"actor,omitempty"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func New(config Config) (*Handler, error) {
	if strings.TrimSpace(config.Token) == "" {
		return nil, errors.New("server: AGENT_API_TOKEN is required")
	}
	if strings.TrimSpace(config.WorkDir) == "" {
		return nil, errors.New("server: workdir is required")
	}
	if config.Registry == nil {
		return nil, errors.New("server: registry is required")
	}
	if config.MaxTurns <= 0 {
		config.MaxTurns = 32
	}
	if config.MaxRepeatedCall <= 0 {
		config.MaxRepeatedCall = 3
	}
	if config.RunTimeout <= 0 {
		config.RunTimeout = 10 * time.Minute
	}
	if config.MaxConcurrent <= 0 {
		config.MaxConcurrent = 2
	}
	if config.ReadinessTTL <= 0 {
		config.ReadinessTTL = 5 * time.Minute
	}
	if config.ReadinessTimeout <= 0 {
		config.ReadinessTimeout = 10 * time.Second
	}
	handler := &Handler{config: config, semaphore: make(chan struct{}, config.MaxConcurrent), mux: http.NewServeMux()}
	handler.mux.HandleFunc("GET /healthz", handler.health)
	handler.mux.HandleFunc("GET /readyz", handler.ready)
	handler.mux.HandleFunc("POST /v1/runs", handler.authorize(handler.run))
	handler.mux.HandleFunc("POST /v1/runs/{runID}/inputs", handler.authorize(handler.enqueueInput))
	handler.mux.HandleFunc("POST /v1/sessions/{sessionID}/turns", handler.authorize(handler.sessionTurn))
	handler.mux.HandleFunc("GET /v1/sessions/{sessionID}", handler.authorize(handler.getSession))
	handler.mux.HandleFunc("POST /v1/tasks", handler.authorize(handler.createTask))
	handler.mux.HandleFunc("GET /v1/tasks/{taskID}", handler.authorize(handler.getTask))
	handler.mux.HandleFunc("POST /v1/tasks/{taskID}/events", handler.authorize(handler.taskEvent))
	handler.mux.HandleFunc("POST /v2/tasks", handler.authorize(handler.createWorkItem))
	handler.mux.HandleFunc("GET /v2/tasks", handler.authorize(handler.listWorkItems))
	handler.mux.HandleFunc("GET /v2/tasks/{taskID}", handler.authorize(handler.getWorkItem))
	handler.mux.HandleFunc("POST /v2/tasks/{taskID}/actions", handler.authorize(handler.workItemAction))
	handler.mux.HandleFunc("POST /v2/tasks/{taskID}/artifacts", handler.authorize(handler.addWorkItemArtifact))
	handler.mux.HandleFunc("GET /v2/artifacts/{artifactID}/versions/{version}", handler.authorize(handler.getWorkItemArtifact))
	handler.mux.HandleFunc("POST /v2/conversations/{conversationID}/messages", handler.authorize(handler.publishAttentionMessage))
	handler.mux.HandleFunc("POST /v2/agents/{agentID}/inbox/claim", handler.authorize(handler.claimAgentInbox))
	handler.mux.HandleFunc("POST /v2/inbox/{itemID}/actions", handler.authorize(handler.agentInboxAction))
	handler.mux.HandleFunc("POST /v2/inbox/{itemID}/fresh-replies", handler.authorize(handler.sendFreshReply))
	handler.mux.HandleFunc("POST /v2/agents/{agentID}/work-marks", handler.authorize(handler.setAgentWorkMark))
	handler.mux.HandleFunc("POST /v2/agents/{agentID}/work-marks/{conversationID}/clear", handler.authorize(handler.clearAgentWorkMark))
	handler.mux.HandleFunc("POST /v2/agents", handler.authorize(handler.createPersistentAgent))
	handler.mux.HandleFunc("GET /v2/agents", handler.authorize(handler.listPersistentAgents))
	handler.mux.HandleFunc("GET /v2/agents/{agentID}", handler.authorize(handler.getPersistentAgent))
	handler.mux.HandleFunc("POST /v2/agents/{agentID}/relationships/revisions", handler.authorize(handler.reviseAgentRelationships))
	handler.mux.HandleFunc("POST /v2/agents/{agentID}/sessions/rotate", handler.authorize(handler.rotateAgentSession))
	handler.mux.HandleFunc("GET /v1/approvals", handler.authorize(handler.listApprovals))
	handler.mux.HandleFunc("GET /v1/approvals/{approvalID}", handler.authorize(handler.getApproval))
	handler.mux.HandleFunc("POST /v1/approvals/{approvalID}/decision", handler.authorize(handler.decideApproval))
	return handler, nil
}

type resultLoader interface {
	LoadResult(context.Context, string) (engine.RunResult, error)
}

func (h *Handler) sessionTurn(writer http.ResponseWriter, request *http.Request) {
	if h.config.Provider == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "model provider is not configured"})
		return
	}
	if h.config.Sessions == nil || h.config.Store == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "session and run stores are not configured"})
		return
	}
	results, ok := h.config.Store.(resultLoader)
	if !ok {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "run store does not support durable result recovery"})
		return
	}
	var input sessionTurnRequest
	if err := decodeRequest(writer, request, &input); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	sessionID := strings.TrimSpace(request.PathValue("sessionID"))
	input.OperationID = strings.TrimSpace(input.OperationID)
	input.Prompt = strings.TrimSpace(input.Prompt)
	if sessionID == "" || input.OperationID == "" || input.Prompt == "" {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: "session_id, operation_id, and prompt are required"})
		return
	}
	if len(input.Prompt) > 32_768 {
		writeJSON(writer, http.StatusRequestEntityTooLarge, errorResponse{Error: "prompt exceeds 32768 bytes"})
		return
	}
	if err := h.validateApprovals(input.ApprovedTools); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	thinking := h.config.EnableThinking
	if input.Thinking != nil {
		thinking = *input.Thinking
	}
	planMode := h.config.PlanMode
	if input.PlanMode != nil {
		planMode = *input.PlanMode
	}
	approvedTools := append([]string{}, input.ApprovedTools...)
	sort.Strings(approvedTools)
	semanticAttributes, _ := json.Marshal(struct {
		Thinking      bool     `json:"thinking"`
		PlanMode      bool     `json:"plan_mode"`
		ApprovedTools []string `json:"approved_tools"`
	}{thinking, planMode, approvedTools})
	release, err := h.config.Sessions.Acquire(request.Context(), sessionID)
	if err != nil {
		writeJSON(writer, http.StatusRequestTimeout, errorResponse{Error: err.Error()})
		return
	}
	defer release()
	select {
	case h.semaphore <- struct{}{}:
		defer func() { <-h.semaphore }()
	default:
		writer.Header().Set("Retry-After", "1")
		writeJSON(writer, http.StatusTooManyRequests, errorResponse{Error: "concurrency limit reached"})
		return
	}

	admission, err := h.config.Sessions.BeginWithSemantics(
		request.Context(), sessionID, input.OperationID, input.Prompt, h.config.WorkDir, "", string(semanticAttributes),
	)
	if err != nil {
		status := http.StatusConflict
		if !errors.Is(err, sessionstore.ErrBusy) && !errors.Is(err, sessionstore.ErrConflict) {
			status = http.StatusBadRequest
		}
		writeJSON(writer, status, errorResponse{Error: err.Error()})
		return
	}
	if admission.Cached != nil {
		result, err := results.LoadResult(request.Context(), admission.Cached.RunID)
		if err != nil {
			writeJSON(writer, http.StatusInternalServerError, errorResponse{Error: "load cached session result: " + err.Error()})
			return
		}
		writeJSON(writer, statusFor(result.Reason), map[string]any{
			"result": result, "session_id": sessionID, "session_revision": admission.State.Revision, "cached": true,
		})
		return
	}

	agent, err := h.newAgent(thinking, planMode, admission.Pending.RunID, sessionID)
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, errorResponse{Error: err.Error()})
		return
	}
	runContext := tools.WithApprovedTools(request.Context(), input.ApprovedTools)
	var result engine.RunResult
	var runErr error
	if admission.Resuming {
		result, err = results.LoadResult(runContext, admission.Pending.RunID)
		if err == nil {
			// The query loop committed its result before the process stopped; only
			// the session commit barrier remains.
		} else if !errors.Is(err, os.ErrNotExist) {
			writeJSON(writer, http.StatusInternalServerError, errorResponse{Error: "load pending run result: " + err.Error()})
			return
		} else {
			checkpoint, checkpointErr := h.config.Store.LoadCheckpoint(runContext, admission.Pending.RunID)
			if checkpointErr == nil {
				result, runErr = agent.Resume(runContext, checkpoint)
			} else if errors.Is(checkpointErr, os.ErrNotExist) {
				result, runErr = agent.RunWithHistory(runContext, admission.Pending.Prompt, admission.State.Messages)
			} else {
				writeJSON(writer, http.StatusInternalServerError, errorResponse{Error: "load pending checkpoint: " + checkpointErr.Error()})
				return
			}
		}
	} else {
		result, runErr = agent.RunWithHistory(runContext, admission.Pending.Prompt, admission.State.Messages)
	}
	if result.RunID == "" || result.CompletedAt.IsZero() {
		response := map[string]any{"result": result, "session_id": sessionID}
		if runErr != nil {
			response["error"] = runErr.Error()
		}
		writeJSON(writer, statusFor(result.Reason), response)
		return
	}
	state, commitErr := h.config.Sessions.Commit(context.WithoutCancel(request.Context()), sessionID, input.OperationID, admission.State.Revision, result)
	if commitErr != nil {
		writeJSON(writer, http.StatusInternalServerError, errorResponse{Error: "commit session turn: " + commitErr.Error()})
		return
	}
	response := map[string]any{
		"result": result, "session_id": sessionID, "session_revision": state.Revision, "cached": false,
	}
	if runErr != nil {
		response["error"] = runErr.Error()
	}
	writeJSON(writer, statusFor(result.Reason), response)
}

func (h *Handler) getSession(writer http.ResponseWriter, request *http.Request) {
	if h.config.Sessions == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "session store is not configured"})
		return
	}
	state, err := h.config.Sessions.Get(request.Context(), strings.TrimSpace(request.PathValue("sessionID")))
	if err != nil {
		writeJSON(writer, http.StatusNotFound, errorResponse{Error: err.Error()})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"session": state})
}

func (h *Handler) newAgent(thinking, planMode bool, runID, memoryScope string) (*engine.AgentEngine, error) {
	return engine.New(h.config.Provider, h.config.Registry, engine.Config{
		WorkDir: h.config.WorkDir, EnableThinking: thinking, PlanMode: planMode, MaxTurns: h.config.MaxTurns,
		MaxTokens: h.config.MaxTokens, MaxRepeatedCall: h.config.MaxRepeatedCall,
		Timeout: h.config.RunTimeout, Store: h.config.Store, Compactor: h.config.Compactor,
		InputQueue: h.config.InputQueue, RunID: runID, MemoryScope: memoryScope,
	})
}

func (h *Handler) validateApprovals(names []string) error {
	if len(names) > 16 {
		return errors.New("approved_tools exceeds 16 entries")
	}
	available := make(map[string]bool)
	for _, definition := range h.config.Registry.GetAvailableTools() {
		available[definition.Name] = true
	}
	for _, name := range names {
		if !available[name] {
			return errors.New("cannot approve unavailable tool: " + name)
		}
	}
	return nil
}

func (h *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	h.mux.ServeHTTP(writer, request)
}

func (h *Handler) health(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]any{"status": "ok", "version": h.config.Version})
}

func (h *Handler) ready(writer http.ResponseWriter, request *http.Request) {
	if h.config.Provider == nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]any{"status": "not_ready", "reason": "model provider is not configured"})
		return
	}
	h.readyMu.Lock()
	defer h.readyMu.Unlock()
	if h.readyAt.IsZero() || time.Since(h.readyAt) >= h.config.ReadinessTTL {
		ctx, cancel := context.WithTimeout(request.Context(), h.config.ReadinessTimeout)
		_, h.readyErr = h.config.Provider.Generate(ctx, []schema.Message{{Role: schema.RoleUser, Content: "Reply with OK only."}}, nil)
		cancel()
		h.readyAt = time.Now().UTC()
	}
	if h.readyErr != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]any{"status": "not_ready", "reason": h.readyErr.Error(), "checked_at": h.readyAt})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"status": "ready", "checked_at": h.readyAt})
}

func (h *Handler) run(writer http.ResponseWriter, request *http.Request) {
	if h.config.Provider == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "model provider is not configured"})
		return
	}
	select {
	case h.semaphore <- struct{}{}:
		defer func() { <-h.semaphore }()
	default:
		writer.Header().Set("Retry-After", "1")
		writeJSON(writer, http.StatusTooManyRequests, errorResponse{Error: "concurrency limit reached"})
		return
	}

	request.Body = http.MaxBytesReader(writer, request.Body, maxRequestBytes)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var input runRequest
	if err := decoder.Decode(&input); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: "invalid JSON request: " + err.Error()})
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: "request body must contain exactly one JSON object"})
		return
	}
	input.Prompt = strings.TrimSpace(input.Prompt)
	input.RunID = strings.TrimSpace(input.RunID)
	input.ResumeRunID = strings.TrimSpace(input.ResumeRunID)
	if (input.Prompt == "") == (input.ResumeRunID == "") {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: "provide exactly one of prompt or resume_run_id"})
		return
	}
	if len(input.Prompt) > 32_768 {
		writeJSON(writer, http.StatusRequestEntityTooLarge, errorResponse{Error: "prompt exceeds 32768 bytes"})
		return
	}
	if len(input.ApprovedTools) > 16 {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: "approved_tools exceeds 16 entries"})
		return
	}
	available := make(map[string]bool)
	for _, definition := range h.config.Registry.GetAvailableTools() {
		available[definition.Name] = true
	}
	for _, name := range input.ApprovedTools {
		if !available[name] {
			writeJSON(writer, http.StatusBadRequest, errorResponse{Error: "cannot approve unavailable tool: " + name})
			return
		}
	}
	thinking := h.config.EnableThinking
	if input.Thinking != nil {
		thinking = *input.Thinking
	}
	planMode := h.config.PlanMode
	if input.PlanMode != nil {
		planMode = *input.PlanMode
	}

	agent, err := engine.New(h.config.Provider, h.config.Registry, engine.Config{
		WorkDir:         h.config.WorkDir,
		EnableThinking:  thinking,
		PlanMode:        planMode,
		MaxTurns:        h.config.MaxTurns,
		MaxTokens:       h.config.MaxTokens,
		MaxRepeatedCall: h.config.MaxRepeatedCall,
		Timeout:         h.config.RunTimeout,
		Store:           h.config.Store,
		Compactor:       h.config.Compactor,
		InputQueue:      h.config.InputQueue,
		RunID:           input.RunID,
	})
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, errorResponse{Error: err.Error()})
		return
	}
	var result engine.RunResult
	var runErr error
	runContext := tools.WithApprovedTools(request.Context(), input.ApprovedTools)
	if input.ResumeRunID != "" {
		if h.config.Store == nil {
			writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "checkpoint store is not configured"})
			return
		}
		checkpoint, err := h.config.Store.LoadCheckpoint(runContext, input.ResumeRunID)
		if err != nil {
			writeJSON(writer, http.StatusNotFound, errorResponse{Error: "checkpoint not found: " + err.Error()})
			return
		}
		result, runErr = agent.Resume(runContext, checkpoint)
	} else {
		result, runErr = agent.Run(runContext, input.Prompt)
	}
	status := statusFor(result.Reason)
	response := map[string]any{"result": result}
	if runErr != nil {
		response["error"] = runErr.Error()
	}
	writeJSON(writer, status, response)
}

func (h *Handler) enqueueInput(writer http.ResponseWriter, request *http.Request) {
	if h.config.InputEnqueuer == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "input queue is not configured"})
		return
	}
	runID := strings.TrimSpace(request.PathValue("runID"))
	if runID == "" || len(runID) > 128 {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: "valid run id is required"})
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, maxRequestBytes)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var input enqueueRequest
	if err := decoder.Decode(&input); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: "invalid JSON request: " + err.Error()})
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: "request body must contain exactly one JSON object"})
		return
	}
	if input.Kind == "" {
		input.Kind = engine.InputUserSteer
	}
	queued, err := h.config.InputEnqueuer.Enqueue(request.Context(), engine.QueuedInput{
		ID: strings.TrimSpace(input.ID), RunID: runID, Kind: input.Kind, Content: input.Content,
	})
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	writeJSON(writer, http.StatusAccepted, map[string]any{"input": queued})
}

func (h *Handler) createTask(writer http.ResponseWriter, request *http.Request) {
	if h.config.Tasks == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "background task store is not configured"})
		return
	}
	var input createTaskRequest
	if err := decodeRequest(writer, request, &input); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	task, err := h.config.Tasks.Create(request.Context(), taskstore.Task{
		ID: input.ID, ParentRunID: input.ParentRunID, ParentTurnID: input.ParentTurnID, Title: input.Title,
	})
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]any{"task": task})
}

func (h *Handler) getTask(writer http.ResponseWriter, request *http.Request) {
	if h.config.Tasks == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "background task store is not configured"})
		return
	}
	task, err := h.config.Tasks.Get(request.Context(), request.PathValue("taskID"))
	if err != nil {
		writeJSON(writer, http.StatusNotFound, errorResponse{Error: err.Error()})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"task": task})
}

func (h *Handler) taskEvent(writer http.ResponseWriter, request *http.Request) {
	if h.config.Tasks == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "background task store is not configured"})
		return
	}
	var input taskEventRequest
	if err := decodeRequest(writer, request, &input); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	taskID := request.PathValue("taskID")
	var task taskstore.Task
	var err error
	switch input.Action {
	case "start":
		task, err = h.config.Tasks.Start(request.Context(), taskID, input.OwnerID)
	case "heartbeat":
		task, err = h.config.Tasks.Heartbeat(request.Context(), taskID, input.OwnerID)
	case "stop":
		task, err = h.config.Tasks.RequestStop(request.Context(), taskID)
	case "complete":
		task, err = h.config.Tasks.Complete(request.Context(), taskID, input.OwnerID, input.Output)
	case "fail":
		task, err = h.config.Tasks.Fail(request.Context(), taskID, input.OwnerID, input.Failure)
	case "cancel":
		task, err = h.config.Tasks.Cancel(request.Context(), taskID)
	default:
		err = fmt.Errorf("unsupported task action %q", input.Action)
	}
	if err != nil {
		writeJSON(writer, http.StatusConflict, errorResponse{Error: err.Error()})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"task": task})
}

func (h *Handler) listApprovals(writer http.ResponseWriter, request *http.Request) {
	if h.config.Approvals == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "approval store is not configured"})
		return
	}
	state := approvalstore.State(strings.TrimSpace(request.URL.Query().Get("state")))
	items, err := h.config.Approvals.List(request.Context(), state)
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, errorResponse{Error: err.Error()})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"approvals": items})
}

func (h *Handler) getApproval(writer http.ResponseWriter, request *http.Request) {
	if h.config.Approvals == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "approval store is not configured"})
		return
	}
	item, err := h.config.Approvals.Get(request.Context(), request.PathValue("approvalID"))
	if err != nil {
		writeJSON(writer, http.StatusNotFound, errorResponse{Error: err.Error()})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"approval": item})
}

func (h *Handler) decideApproval(writer http.ResponseWriter, request *http.Request) {
	if h.config.Approvals == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "approval store is not configured"})
		return
	}
	var input approvalDecisionRequest
	if err := decodeRequest(writer, request, &input); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	item, err := h.config.Approvals.Decide(request.Context(), request.PathValue("approvalID"), input.Decision, input.Actor)
	if err != nil {
		writeJSON(writer, http.StatusConflict, errorResponse{Error: err.Error()})
		return
	}
	if h.config.InputEnqueuer != nil {
		directive := fmt.Sprintf("Approval %s was %s for the exact %s call.", item.ID, item.State, item.ToolName)
		if item.State == approvalstore.Approved {
			directive += " Retry that exact tool call if it is still required; changed arguments require a new approval."
		} else {
			directive += " Do not retry the denied action; choose a safe alternative or report the block."
		}
		if _, err := h.config.InputEnqueuer.Enqueue(context.WithoutCancel(request.Context()), engine.QueuedInput{
			ID: "approval-decision-" + item.ID, RunID: item.RunID, Kind: engine.InputTool, Content: directive,
		}); err != nil {
			writeJSON(writer, http.StatusInternalServerError, errorResponse{Error: "persist approval decision input: " + err.Error()})
			return
		}
	}
	writeJSON(writer, http.StatusOK, map[string]any{"approval": item})
}

func decodeRequest(writer http.ResponseWriter, request *http.Request, target any) error {
	request.Body = http.MaxBytesReader(writer, request.Body, maxRequestBytes)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid JSON request: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("request body must contain exactly one JSON object")
	}
	return nil
}

func (h *Handler) authorize(next http.HandlerFunc) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		value := strings.TrimSpace(request.Header.Get("Authorization"))
		provided, found := strings.CutPrefix(value, "Bearer ")
		if !found || subtle.ConstantTimeCompare([]byte(provided), []byte(h.config.Token)) != 1 {
			writer.Header().Set("WWW-Authenticate", "Bearer")
			writeJSON(writer, http.StatusUnauthorized, errorResponse{Error: "invalid bearer token"})
			return
		}
		next(writer, request)
	}
}

func statusFor(reason schema.TerminalReason) int {
	switch reason {
	case schema.TerminalCompleted:
		return http.StatusOK
	case schema.TerminalPermissionDenied:
		return http.StatusForbidden
	case schema.TerminalApprovalRequired:
		return http.StatusPreconditionRequired
	case schema.TerminalTimeout, schema.TerminalAborted:
		return http.StatusRequestTimeout
	case schema.TerminalProviderError:
		return http.StatusBadGateway
	default:
		return http.StatusConflict
	}
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		fmt.Fprintf(writer, "{\"error\":%q}\n", err.Error())
	}
}
