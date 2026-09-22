package server

import (
	"errors"
	"net/http"
	"os"
	"strconv"

	"github.com/royal007a/01agent/internal/team"
)

func (h *Handler) selectTeam(writer http.ResponseWriter, request *http.Request) {
	if h.config.Teams == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "team store is not configured"})
		return
	}
	var input team.SelectInput
	if err := decodeRequest(writer, request, &input); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	lockfile, err := h.config.Teams.Select(request.Context(), input)
	if err != nil {
		writeTeamError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]any{"lockfile": lockfile})
}

func (h *Handler) getTeamLockfile(writer http.ResponseWriter, request *http.Request) {
	if h.config.Teams == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "team store is not configured"})
		return
	}
	version, err := strconv.ParseInt(request.PathValue("version"), 10, 64)
	if request.PathValue("version") == "latest" {
		version, err = 0, nil
	}
	if err != nil || version < 0 {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: "version must be a positive integer or latest"})
		return
	}
	lockfile, err := h.config.Teams.Get(request.Context(), request.PathValue("teamID"), version)
	if err != nil {
		writeTeamError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"lockfile": lockfile})
}

func writeTeamError(writer http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, os.ErrNotExist):
		status = http.StatusNotFound
	case errors.Is(err, team.ErrConflict), errors.Is(err, team.ErrNoSelection):
		status = http.StatusConflict
	}
	writeJSON(writer, status, errorResponse{Error: err.Error()})
}
