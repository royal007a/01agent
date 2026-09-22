package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/royal007a/01agent/internal/attention"
)

func (h *Handler) publishAttentionMessage(writer http.ResponseWriter, request *http.Request) {
	if h.config.Attention == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "agent attention store is not configured"})
		return
	}
	var input attention.PublishInput
	if err := decodeRequest(writer, request, &input); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	input.ConversationID = request.PathValue("conversationID")
	message, items, err := h.config.Attention.Publish(request.Context(), input)
	if err != nil {
		writeAttentionError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]any{"message": message, "inbox_items": items})
}

func (h *Handler) claimAgentInbox(writer http.ResponseWriter, request *http.Request) {
	if h.config.Attention == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "agent attention store is not configured"})
		return
	}
	var input inboxClaimRequest
	if err := decodeRequest(writer, request, &input); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	claim, err := h.config.Attention.Claim(request.Context(), request.PathValue("agentID"), input.OperationID, input.LeaseID, time.Duration(input.TTLSeconds)*time.Second)
	if err != nil {
		writeAttentionError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"claim": claim})
}

func (h *Handler) agentInboxAction(writer http.ResponseWriter, request *http.Request) {
	if h.config.Attention == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "agent attention store is not configured"})
		return
	}
	var input inboxActionRequest
	if err := decodeRequest(writer, request, &input); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	itemID := request.PathValue("itemID")
	switch strings.TrimSpace(input.Action) {
	case "ack":
		item, err := h.config.Attention.Ack(request.Context(), itemID, input.OperationID, input.AgentID, input.LeaseID)
		if err != nil {
			writeAttentionError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"item": item})
	case "refresh":
		claim, err := h.config.Attention.Refresh(request.Context(), itemID, input.OperationID, input.AgentID, input.LeaseID)
		if err != nil {
			writeAttentionError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"claim": claim})
	default:
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: "unsupported inbox action"})
	}
}

func (h *Handler) sendFreshReply(writer http.ResponseWriter, request *http.Request) {
	if h.config.Attention == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "agent attention store is not configured"})
		return
	}
	var input attention.FreshReplyInput
	if err := decodeRequest(writer, request, &input); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	input.ItemID = request.PathValue("itemID")
	result, err := h.config.Attention.SendFresh(request.Context(), input)
	if errors.Is(err, attention.ErrStale) {
		writeJSON(writer, http.StatusConflict, map[string]any{"error": err.Error(), "result": result})
		return
	}
	if err != nil {
		writeAttentionError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]any{"result": result})
}

func (h *Handler) setAgentWorkMark(writer http.ResponseWriter, request *http.Request) {
	if h.config.Attention == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "agent attention store is not configured"})
		return
	}
	var input attention.WorkMarkInput
	if err := decodeRequest(writer, request, &input); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	input.AgentID = request.PathValue("agentID")
	mark, err := h.config.Attention.SetWorkMark(request.Context(), input)
	if err != nil {
		writeAttentionError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]any{"work_mark": mark})
}

func (h *Handler) clearAgentWorkMark(writer http.ResponseWriter, request *http.Request) {
	if h.config.Attention == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "agent attention store is not configured"})
		return
	}
	var input clearWorkMarkRequest
	if err := decodeRequest(writer, request, &input); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	mark, err := h.config.Attention.ClearWorkMark(request.Context(), request.PathValue("agentID"), request.PathValue("conversationID"), input.OperationID)
	if err != nil {
		writeAttentionError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"work_mark": mark})
}

func writeAttentionError(writer http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if errors.Is(err, attention.ErrConflict) || errors.Is(err, attention.ErrLease) {
		status = http.StatusConflict
	}
	if errors.Is(err, attention.ErrNoWork) {
		status = http.StatusNoContent
	}
	if status == http.StatusNoContent {
		writer.WriteHeader(status)
		return
	}
	writeJSON(writer, status, errorResponse{Error: err.Error()})
}
