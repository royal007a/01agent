package server

import (
	"errors"
	"net/http"
	"os"

	"github.com/royal007a/01agent/internal/dispatcher"
)

func (h *Handler) startDispatch(writer http.ResponseWriter, request *http.Request) {
	if h.config.Dispatcher == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "dispatcher is not configured"})
		return
	}
	var input dispatcher.StartInput
	if err := decodeRequest(writer, request, &input); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	execution, err := h.config.Dispatcher.Start(request.Context(), input)
	if err != nil {
		writeDispatcherError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]any{"execution": execution})
}

func (h *Handler) listDispatches(writer http.ResponseWriter, request *http.Request) {
	if h.config.Dispatcher == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "dispatcher is not configured"})
		return
	}
	executions, err := h.config.Dispatcher.List(request.Context())
	if err != nil {
		writeDispatcherError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"executions": executions})
}

func (h *Handler) reconcileDispatches(writer http.ResponseWriter, request *http.Request) {
	if h.config.Dispatcher == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "dispatcher is not configured"})
		return
	}
	result, err := h.config.Dispatcher.Reconcile(request.Context())
	if err != nil {
		writeDispatcherError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (h *Handler) cancelDispatch(writer http.ResponseWriter, request *http.Request) {
	if h.config.Dispatcher == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "dispatcher is not configured"})
		return
	}
	var input dispatcher.CancelInput
	if err := decodeRequest(writer, request, &input); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	execution, err := h.config.Dispatcher.Cancel(request.Context(), request.PathValue("executionID"), input)
	if err != nil {
		writeDispatcherError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"execution": execution})
}

func writeDispatcherError(writer http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, os.ErrNotExist):
		status = http.StatusNotFound
	case errors.Is(err, dispatcher.ErrConflict), errors.Is(err, dispatcher.ErrTerminal):
		status = http.StatusConflict
	}
	writeJSON(writer, status, errorResponse{Error: err.Error()})
}
