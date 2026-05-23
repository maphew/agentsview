package server

import (
	"net/http"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

func (s *Server) handleListSecrets(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	reveal := q.Get("reveal") == "true"
	if reveal && !isLocalhostRequest(r) {
		writeError(w, http.StatusForbidden,
			"reveal is only permitted from localhost")
		return
	}
	limit, ok := parseIntParam(w, r, "limit")
	if !ok {
		return
	}
	cursor, ok := parseNonNegativeIntParam(w, r, "cursor")
	if !ok {
		return
	}
	dateFrom := q.Get("date_from")
	dateTo := q.Get("date_to")
	if !validateDateFilters(w, "", dateFrom, dateTo, "") {
		return
	}
	res, err := s.sessions.ListSecrets(r.Context(), service.SecretListFilter{
		Project: q.Get("project"), Agent: q.Get("agent"),
		DateFrom: dateFrom, DateTo: dateTo,
		Rule: q.Get("rule"), Confidence: q.Get("confidence"),
		Reveal: reveal, Limit: limit, Cursor: cursor,
	})
	if err != nil {
		if handleContextError(w, err) {
			return
		}
		if handleReadOnly(w, err) {
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if res.Findings == nil {
		res.Findings = []db.SecretFindingRow{}
	}
	writeJSON(w, http.StatusOK, res)
}
