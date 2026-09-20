package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/royal007a/01agent/internal/engine"
	"github.com/royal007a/01agent/internal/provider"
	"github.com/royal007a/01agent/internal/schema"
	"github.com/royal007a/01agent/internal/taskstore"
	"github.com/royal007a/01agent/internal/tools"
)

const maxRequestBytes = 1 << 20

type Config struct {
	Version          string
	Token            string
	WorkDir          string
	Provider         provider.LLMProvider
	Registry         tools.Registry
	EnableThinking   bool
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
	ResumeRunID   string   `json:"resume_run_id,omitempty"`
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
	handler.mux.HandleFunc("POST /v1/tasks", handler.authorize(handler.createTask))
	handler.mux.HandleFunc("GET /v1/tasks/{taskID}", handler.authorize(handler.getTask))
	handler.mux.HandleFunc("POST /v1/tasks/{taskID}/events", handler.authorize(handler.taskEvent))
	return handler, nil
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

	agent, err := engine.New(h.config.Provider, h.config.Registry, engine.Config{
		WorkDir:         h.config.WorkDir,
		EnableThinking:  thinking,
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
