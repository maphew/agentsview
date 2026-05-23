package server

import (
	"errors"
	"net/http"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

func (s *Server) handleSearchContent(
	w http.ResponseWriter, r *http.Request,
) {
	q := r.URL.Query()
	// Use the raw pattern (no trimming) so leading/trailing spaces match the
	// direct backend; only a truly absent/empty parameter is rejected.
	pattern := q.Get("pattern")
	if pattern == "" {
		writeError(w, http.StatusBadRequest, "pattern required")
		return
	}
	mode := q.Get("mode")
	if mode != "" && mode != "substring" && mode != "regex" && mode != "fts" {
		writeError(w, http.StatusBadRequest, "invalid mode")
		return
	}
	// reveal prints unmasked secrets, so it is honored only for localhost
	// callers; a remote client (e.g. a 0.0.0.0-bound pg serve) cannot extract
	// unredacted values. The CLI prints the re-leak warning for the human, so
	// there is deliberately no server-side warning here.
	reveal := q.Get("reveal") == "true"
	if reveal && !isLocalhostRequest(r) {
		writeError(w, http.StatusForbidden,
			"reveal is only permitted from localhost")
		return
	}
	var sources []string
	if in := q.Get("in"); in != "" {
		sources = strings.Split(in, ",")
	}
	limit, ok := parseIntParam(w, r, "limit")
	if !ok {
		return
	}
	cursor, ok := parseNonNegativeIntParam(w, r, "cursor")
	if !ok {
		return
	}
	date := q.Get("date")
	dateFrom := q.Get("date_from")
	dateTo := q.Get("date_to")
	activeSince := q.Get("active_since")
	if !validateDateFilters(w, date, dateFrom, dateTo, activeSince) {
		return
	}
	req := service.ContentSearchRequest{
		Pattern:          pattern,
		Mode:             mode,
		Sources:          sources,
		ExcludeSystem:    q.Get("exclude_system") == "true",
		Reveal:           reveal,
		Project:          q.Get("project"),
		ExcludeProject:   q.Get("exclude_project"),
		Machine:          q.Get("machine"),
		Agent:            q.Get("agent"),
		Date:             date,
		DateFrom:         dateFrom,
		DateTo:           dateTo,
		ActiveSince:      activeSince,
		IncludeChildren:  q.Get("include_children") == "true",
		IncludeAutomated: q.Get("include_automated") == "true",
		IncludeOneShot:   q.Get("include_one_shot") == "true",
		Limit:            limit,
		Cursor:           cursor,
	}
	res, err := s.sessions.SearchContent(r.Context(), req)
	if err != nil {
		if handleContextError(w, err) {
			return
		}
		if handleReadOnly(w, err) {
			return
		}
		// User-input problems (invalid regex, fts source guard, unknown
		// source) are 400; anything else is an internal fault → 500.
		var inputErr *db.SearchInputError
		if errors.As(err, &inputErr) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if res.Matches == nil {
		res.Matches = []db.ContentMatch{}
	}
	writeJSON(w, http.StatusOK, res)
}
