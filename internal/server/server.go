package server

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/royal007a/01agent/internal/engine"
	"github.com/royal007a/01agent/internal/provider"
	"github.com/royal007a/01agent/internal/schema"
	"github.com/royal007a/01agent/internal/tools"
)

const maxRequestBytes = 1 << 20

type Config struct {
	Version         string
	Token           string
	WorkDir         string
	Provider        provider.LLMProvider
	Registry        tools.Registry
	EnableThinking  bool
	MaxTurns        int
	MaxTokens       int64
	MaxRepeatedCall int
	RunTimeout      time.Duration
	MaxConcurrent   int
}

type Handler struct {
	config    Config
	semaphore chan struct{}
	mux       *http.ServeMux
}

type runRequest struct {
	Prompt   string `json:"prompt"`
	Thinking *bool  `json:"thinking,omitempty"`
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
	handler := &Handler{config: config, semaphore: make(chan struct{}, config.MaxConcurrent), mux: http.NewServeMux()}
	handler.mux.HandleFunc("GET /healthz", handler.health)
	handler.mux.HandleFunc("GET /readyz", handler.ready)
	handler.mux.HandleFunc("POST /v1/runs", handler.authorize(handler.run))
	return handler, nil
}

func (h *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	h.mux.ServeHTTP(writer, request)
}

func (h *Handler) health(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]any{"status": "ok", "version": h.config.Version})
}

func (h *Handler) ready(writer http.ResponseWriter, _ *http.Request) {
	if h.config.Provider == nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]any{"status": "not_ready", "reason": "model provider is not configured"})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"status": "ready"})
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
	input.Prompt = strings.TrimSpace(input.Prompt)
	if input.Prompt == "" {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: "prompt is required"})
		return
	}
	if len(input.Prompt) > 32_768 {
		writeJSON(writer, http.StatusRequestEntityTooLarge, errorResponse{Error: "prompt exceeds 32768 bytes"})
		return
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
	})
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, errorResponse{Error: err.Error()})
		return
	}
	result, runErr := agent.Run(request.Context(), input.Prompt)
	status := statusFor(result.Reason)
	response := map[string]any{"result": result}
	if runErr != nil {
		response["error"] = runErr.Error()
	}
	writeJSON(writer, status, response)
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
