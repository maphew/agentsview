package server

import (
	"log"
	"net/http"

	"go.kenn.io/agentsview/internal/db"
)

func (s *Server) handleTrendsTerms(
	w http.ResponseWriter, r *http.Request,
) {
	f, ok := parseAnalyticsFilter(w, r)
	if !ok {
		return
	}

	granularity := r.URL.Query().Get("granularity")
	if granularity == "" {
		granularity = "week"
	}
	switch granularity {
	case "day", "week", "month":
	default:
		writeError(w, http.StatusBadRequest,
			"invalid granularity: must be day, week, or month")
		return
	}

	terms, err := db.ParseTrendTerms(r.URL.Query()["term"])
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	result, err := s.db.GetTrendsTerms(
		r.Context(), f, terms, granularity,
	)
	if err != nil {
		if handleContextError(w, err) {
			return
		}
		log.Printf("trends terms error: %v", err)
		writeError(w, http.StatusInternalServerError,
			"internal server error")
		return
	}
	writeJSON(w, http.StatusOK, result)
}
