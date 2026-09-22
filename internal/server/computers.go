package server

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/royal007a/01agent/internal/computer"
)

const daemonLeaseTTL = 45 * time.Second

func (h *Handler) registerComputer(writer http.ResponseWriter, request *http.Request) {
	if h.config.Computers == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "computer store is not configured"})
		return
	}
	var input computer.RegisterInput
	if err := decodeRequest(writer, request, &input); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	item, err := h.config.Computers.Register(request.Context(), input)
	if err != nil {
		writeComputerError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]any{"computer": item})
}

func (h *Handler) listComputers(writer http.ResponseWriter, request *http.Request) {
	if h.config.Computers == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "computer store is not configured"})
		return
	}
	state, err := h.config.Computers.GetState(request.Context())
	if err != nil {
		writeComputerError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"computers": state.Computers, "bindings": state.Bindings, "commands": state.Commands})
}

func (h *Handler) rebindAgentComputer(writer http.ResponseWriter, request *http.Request) {
	if h.config.Computers == nil || h.config.Agents == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "computer and agent stores are required"})
		return
	}
	agentID := request.PathValue("agentID")
	if _, err := h.config.Agents.Get(request.Context(), agentID); err != nil {
		writeAgentRegistryError(writer, err)
		return
	}
	var input computer.RebindInput
	if err := decodeRequest(writer, request, &input); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	input.AgentID = agentID
	binding, cleanup, err := h.config.Computers.Rebind(request.Context(), input)
	if err != nil {
		writeComputerError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"binding": binding, "old_computer_cleanup": cleanup})
}

func (h *Handler) acquireAgentRunLease(writer http.ResponseWriter, request *http.Request) {
	if h.config.Computers == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "computer store is not configured"})
		return
	}
	var input computer.RunLeaseInput
	if err := decodeRequest(writer, request, &input); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	input.AgentID = request.PathValue("agentID")
	lease, err := h.config.Computers.AcquireRun(request.Context(), input)
	if err != nil {
		writeComputerError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]any{"run_lease": lease})
}

func (h *Handler) releaseAgentRunLease(writer http.ResponseWriter, request *http.Request) {
	if h.config.Computers == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "computer store is not configured"})
		return
	}
	var input releaseRunLeaseRequest
	if err := decodeRequest(writer, request, &input); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	if err := h.config.Computers.ReleaseRun(request.Context(), request.PathValue("runID"), input.OperationID, input.LeaseID); err != nil {
		writeComputerError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"released": true})
}

func (h *Handler) daemonConnect(writer http.ResponseWriter, request *http.Request) {
	if h.config.Computers == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "computer store is not configured"})
		return
	}
	connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	defer connection.Close(websocket.StatusNormalClosure, "daemon disconnected")
	connection.SetReadLimit(maxRequestBytes)
	ctx := request.Context()
	readContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	var first computer.ClientMessage
	err = wsjson.Read(readContext, connection, &first)
	cancel()
	if err != nil || first.Type != "hello" || first.Hello == nil {
		connection.Close(websocket.StatusPolicyViolation, "hello required")
		return
	}
	hello := *first.Hello
	if _, err := h.config.Computers.Connect(ctx, hello, daemonLeaseTTL); err != nil {
		connection.Close(websocket.StatusPolicyViolation, err.Error())
		return
	}
	defer h.config.Computers.Disconnect(context.WithoutCancel(ctx), hello.ComputerID, hello.LeaseID)
	if err := wsjson.Write(ctx, connection, computer.ServerMessage{Type: "hello_ack"}); err != nil {
		return
	}
	for {
		var input computer.ClientMessage
		if err := wsjson.Read(ctx, connection, &input); err != nil {
			return
		}
		switch input.Type {
		case "poll":
			if _, err := h.config.Computers.Heartbeat(ctx, hello.ComputerID, hello.LeaseID, daemonLeaseTTL); err != nil {
				connection.Close(websocket.StatusPolicyViolation, err.Error())
				return
			}
			command, err := h.config.Computers.PollCommand(ctx, hello.ComputerID, hello.LeaseID)
			if errors.Is(err, computer.ErrNoCommand) {
				err = wsjson.Write(ctx, connection, computer.ServerMessage{Type: "noop"})
			} else if err == nil {
				err = wsjson.Write(ctx, connection, computer.ServerMessage{Type: "command", Command: &command})
			}
			if err != nil {
				return
			}
		case "ack":
			if input.Ack == nil {
				connection.Close(websocket.StatusPolicyViolation, "ack payload required")
				return
			}
			if _, err := h.config.Computers.AckCommand(ctx, hello.ComputerID, hello.LeaseID, *input.Ack); err != nil {
				connection.Close(websocket.StatusPolicyViolation, err.Error())
				return
			}
			if err := wsjson.Write(ctx, connection, computer.ServerMessage{Type: "acknowledged"}); err != nil {
				return
			}
		default:
			connection.Close(websocket.StatusUnsupportedData, "unsupported daemon message")
			return
		}
	}
}

func writeComputerError(writer http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, os.ErrNotExist):
		status = http.StatusNotFound
	case errors.Is(err, computer.ErrConflict), errors.Is(err, computer.ErrOffline), errors.Is(err, computer.ErrActiveRun), errors.Is(err, computer.ErrLease):
		status = http.StatusConflict
	}
	writeJSON(writer, status, errorResponse{Error: strings.TrimSpace(err.Error())})
}
