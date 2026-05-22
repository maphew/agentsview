package server

import (
	"net/http"

	dbpkg "go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

func (s *Server) handleGetMessages(
	w http.ResponseWriter, r *http.Request,
) {
	sessionID := r.PathValue("id")

	limit, ok := parseIntParam(w, r, "limit")
	if !ok {
		return
	}
	limit = clampLimit(limit, dbpkg.DefaultMessageLimit, dbpkg.MaxMessageLimit)

	direction := r.URL.Query().Get("direction")
	switch direction {
	case "", "asc", "desc":
	default:
		writeError(w, http.StatusBadRequest,
			"invalid direction: must be asc or desc")
		return
	}

	filter := service.MessageFilter{
		Limit:     limit,
		Direction: direction,
	}
	if r.URL.Query().Get("from") != "" {
		from, ok := parseIntParam(w, r, "from")
		if !ok {
			return
		}
		filter.From = &from
	}

	list, err := s.sessions.Messages(r.Context(), sessionID, filter)
	if err != nil {
		if handleContextError(w, err) {
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, list)
}
