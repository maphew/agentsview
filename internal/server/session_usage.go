package server

import (
	"net/http"

	"go.kenn.io/agentsview/internal/db"
)

type sessionUsageResponse struct {
	db.SessionUsage
	UnpricedModels []string `json:"unpriced_models"`
	ServerRunning  bool     `json:"server_running"`
}

func (s *Server) handleSessionUsage(
	w http.ResponseWriter, r *http.Request,
) {
	usage, err := s.db.GetSessionUsage(r.Context(), r.PathValue("id"))
	if err != nil {
		if handleContextError(w, err) {
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if usage == nil {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}

	writeJSON(w, http.StatusOK, newSessionUsageResponse(usage))
}

func newSessionUsageResponse(usage *db.SessionUsage) sessionUsageResponse {
	unpricedModels := usage.UnpricedModels
	if unpricedModels == nil {
		unpricedModels = []string{}
	}
	return sessionUsageResponse{
		SessionUsage:   *usage,
		UnpricedModels: unpricedModels,
		ServerRunning:  true,
	}
}
