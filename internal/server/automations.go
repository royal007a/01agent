package server

import (
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/royal007a/01agent/internal/automation"
)

type automationTickRequest struct {
	At time.Time `json:"at,omitempty"`
}

type automationStatusRequest struct {
	OperationID string            `json:"operation_id"`
	Status      automation.Status `json:"status"`
}

func (h *Handler) createAutomation(writer http.ResponseWriter, request *http.Request) {
	if h.config.Automations == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "automation store is not configured"})
		return
	}
	var input automation.CreateInput
	if err := decodeRequest(writer, request, &input); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	item, err := h.config.Automations.Create(request.Context(), input)
	if err != nil {
		writeAutomationError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]any{"automation": item})
}

func (h *Handler) listAutomations(writer http.ResponseWriter, request *http.Request) {
	if h.config.Automations == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "automation store is not configured"})
		return
	}
	items, err := h.config.Automations.List(request.Context())
	if err != nil {
		writeAutomationError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"automations": items})
}

func (h *Handler) tickAutomations(writer http.ResponseWriter, request *http.Request) {
	if h.config.Automations == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "automation store is not configured"})
		return
	}
	var input automationTickRequest
	if err := decodeRequest(writer, request, &input); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	result, err := h.config.Automations.Tick(request.Context(), input.At)
	if err != nil {
		writeAutomationError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (h *Handler) reportAutomation(writer http.ResponseWriter, request *http.Request) {
	if h.config.Automations == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "automation store is not configured"})
		return
	}
	var input automation.ReportInput
	if err := decodeRequest(writer, request, &input); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	item, err := h.config.Automations.Report(request.Context(), request.PathValue("automationID"), input)
	if err != nil {
		writeAutomationError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"automation": item})
}

func (h *Handler) setAutomationStatus(writer http.ResponseWriter, request *http.Request) {
	if h.config.Automations == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "automation store is not configured"})
		return
	}
	var input automationStatusRequest
	if err := decodeRequest(writer, request, &input); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	item, err := h.config.Automations.SetStatus(request.Context(), strings.TrimSpace(request.PathValue("automationID")), input.OperationID, input.Status)
	if err != nil {
		writeAutomationError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"automation": item})
}

func writeAutomationError(writer http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, os.ErrNotExist):
		status = http.StatusNotFound
	case errors.Is(err, automation.ErrConflict), errors.Is(err, automation.ErrRun):
		status = http.StatusConflict
	}
	writeJSON(writer, status, errorResponse{Error: err.Error()})
}
