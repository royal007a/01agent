package server

import (
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/royal007a/01agent/internal/workitem"
)

func (h *Handler) createWorkItem(writer http.ResponseWriter, request *http.Request) {
	if h.config.WorkItems == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "product task store is not configured"})
		return
	}
	var input workitem.Create
	if err := decodeRequest(writer, request, &input); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	task, err := h.config.WorkItems.Create(request.Context(), input)
	if err != nil {
		writeWorkItemError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]any{"task": task})
}

func (h *Handler) listWorkItems(writer http.ResponseWriter, request *http.Request) {
	if h.config.WorkItems == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "product task store is not configured"})
		return
	}
	tasks, err := h.config.WorkItems.List(request.Context(), strings.TrimSpace(request.URL.Query().Get("workspace_id")), strings.TrimSpace(request.URL.Query().Get("channel_id")))
	if err != nil {
		writeWorkItemError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"tasks": tasks})
}

func (h *Handler) getWorkItem(writer http.ResponseWriter, request *http.Request) {
	if h.config.WorkItems == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "product task store is not configured"})
		return
	}
	task, err := h.config.WorkItems.Get(request.Context(), request.PathValue("taskID"))
	if err != nil {
		writeWorkItemError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"task": task})
}

func (h *Handler) workItemAction(writer http.ResponseWriter, request *http.Request) {
	if h.config.WorkItems == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "product task store is not configured"})
		return
	}
	var input workItemActionRequest
	if err := decodeRequest(writer, request, &input); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	taskID := request.PathValue("taskID")
	var task workitem.Task
	var err error
	switch strings.TrimSpace(input.Action) {
	case "claim":
		task, err = h.config.WorkItems.Claim(request.Context(), taskID, input.OperationID, input.OwnerID, input.LeaseID, input.ExpectedRevision, time.Duration(input.TTLSeconds)*time.Second)
	case "renew":
		task, err = h.config.WorkItems.Renew(request.Context(), taskID, input.OperationID, input.OwnerID, input.LeaseID, input.ExpectedRevision, time.Duration(input.TTLSeconds)*time.Second)
	case "revise_contract":
		task, err = h.config.WorkItems.ReviseContract(request.Context(), taskID, workitem.ContractUpdate{
			OperationID: input.OperationID, ExpectedRevision: input.ExpectedRevision, ActorID: input.ActorID,
			Requirements: input.Requirements, Scope: input.Scope, StopConditions: input.StopConditions, Gate: input.Gate,
		})
	case "submit":
		task, err = h.config.WorkItems.Submit(request.Context(), taskID, input.OperationID, input.OwnerID, input.LeaseID, input.ExpectedRevision, input.Handoff)
	case "review":
		task, err = h.config.WorkItems.Review(request.Context(), taskID, input.OperationID, input.ExpectedRevision, input.GateResult)
	case "close":
		task, err = h.config.WorkItems.Close(request.Context(), taskID, input.OperationID, input.ActorID, input.Reason, input.ExpectedRevision)
	default:
		err = errors.New("unsupported product task action")
	}
	if err != nil {
		writeWorkItemError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"task": task})
}

func (h *Handler) addWorkItemArtifact(writer http.ResponseWriter, request *http.Request) {
	if h.config.WorkItems == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "product task store is not configured"})
		return
	}
	var input workitem.ArtifactInput
	if err := decodeRequest(writer, request, &input); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	task, artifact, err := h.config.WorkItems.AddArtifact(request.Context(), request.PathValue("taskID"), input)
	if err != nil {
		writeWorkItemError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]any{"task": task, "artifact": artifact})
}

func (h *Handler) getWorkItemArtifact(writer http.ResponseWriter, request *http.Request) {
	if h.config.WorkItems == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "product task store is not configured"})
		return
	}
	artifact, err := h.config.WorkItems.GetArtifact(request.Context(), request.PathValue("artifactID"), request.PathValue("version"))
	if err != nil {
		writeWorkItemError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"artifact": artifact})
}

func writeWorkItemError(writer http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, os.ErrNotExist):
		status = http.StatusNotFound
	case errors.Is(err, workitem.ErrConflict), errors.Is(err, workitem.ErrLease), errors.Is(err, workitem.ErrTransition):
		status = http.StatusConflict
	}
	writeJSON(writer, status, errorResponse{Error: err.Error()})
}
