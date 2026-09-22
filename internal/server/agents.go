package server

import (
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/royal007a/01agent/internal/agentregistry"
)

func (h *Handler) createPersistentAgent(writer http.ResponseWriter, request *http.Request) {
	if h.config.Agents == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "agent registry is not configured"})
		return
	}
	var input agentregistry.CreateInput
	if err := decodeRequest(writer, request, &input); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	agent, err := h.config.Agents.Create(request.Context(), input)
	if err != nil {
		writeAgentRegistryError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]any{"agent": agent})
}

func (h *Handler) listPersistentAgents(writer http.ResponseWriter, request *http.Request) {
	if h.config.Agents == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "agent registry is not configured"})
		return
	}
	agents, err := h.config.Agents.List(request.Context(), strings.TrimSpace(request.URL.Query().Get("workspace_id")))
	if err != nil {
		writeAgentRegistryError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"agents": agents})
}

func (h *Handler) getPersistentAgent(writer http.ResponseWriter, request *http.Request) {
	if h.config.Agents == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "agent registry is not configured"})
		return
	}
	agent, err := h.config.Agents.Get(request.Context(), request.PathValue("agentID"))
	if err != nil {
		writeAgentRegistryError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"agent": agent})
}

func (h *Handler) reviseAgentRelationships(writer http.ResponseWriter, request *http.Request) {
	if h.config.Agents == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "agent registry is not configured"})
		return
	}
	var input agentregistry.RelationshipUpdate
	if err := decodeRequest(writer, request, &input); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	agent, err := h.config.Agents.ReviseRelationships(request.Context(), request.PathValue("agentID"), input)
	if err != nil {
		writeAgentRegistryError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]any{"agent": agent})
}

func (h *Handler) rotateAgentSession(writer http.ResponseWriter, request *http.Request) {
	if h.config.Agents == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "agent registry is not configured"})
		return
	}
	var input agentregistry.SessionRotation
	if err := decodeRequest(writer, request, &input); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	agent, err := h.config.Agents.RotateSession(request.Context(), request.PathValue("agentID"), input)
	if err != nil {
		writeAgentRegistryError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]any{"agent": agent})
}

func (h *Handler) proposeAgentRevision(writer http.ResponseWriter, request *http.Request) {
	if h.config.Agents == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "agent registry is not configured"})
		return
	}
	var input agentregistry.AgentRevisionProposal
	if err := decodeRequest(writer, request, &input); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	agent, err := h.config.Agents.ProposeAgentRevision(request.Context(), request.PathValue("agentID"), input)
	if err != nil {
		writeAgentRegistryError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]any{"agent": agent})
}

func (h *Handler) selectAgentRevision(writer http.ResponseWriter, request *http.Request) {
	if h.config.Agents == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "agent registry is not configured"})
		return
	}
	var input agentregistry.SelectionInput
	if err := decodeRequest(writer, request, &input); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	agent, err := h.config.Agents.SelectAgentRevision(request.Context(), request.PathValue("agentID"), input)
	if err != nil {
		writeAgentRegistryError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]any{"agent": agent})
}

func (h *Handler) createAgentRelationshipPR(writer http.ResponseWriter, request *http.Request) {
	if h.config.Agents == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "agent registry is not configured"})
		return
	}
	var input agentregistry.RelationshipPRInput
	if err := decodeRequest(writer, request, &input); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	agent, err := h.config.Agents.CreateRelationshipPR(request.Context(), request.PathValue("agentID"), input)
	if err != nil {
		writeAgentRegistryError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]any{"agent": agent})
}

func (h *Handler) reviewAgentRelationshipPR(writer http.ResponseWriter, request *http.Request) {
	if h.config.Agents == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "agent registry is not configured"})
		return
	}
	var input agentregistry.RelationshipPRReview
	if err := decodeRequest(writer, request, &input); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	agent, err := h.config.Agents.ReviewRelationshipPR(request.Context(), request.PathValue("agentID"), request.PathValue("prID"), input)
	if err != nil {
		writeAgentRegistryError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"agent": agent})
}

func writeAgentRegistryError(writer http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, os.ErrNotExist):
		status = http.StatusNotFound
	case errors.Is(err, agentregistry.ErrConflict), errors.Is(err, agentregistry.ErrSession):
		status = http.StatusConflict
	}
	writeJSON(writer, status, errorResponse{Error: err.Error()})
}
