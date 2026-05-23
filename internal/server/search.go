package server

import (
	"net/http"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

type searchResponse struct {
	Query   string            `json:"query"`
	Results []db.SearchResult `json:"results"`
	Count   int               `json:"count"`
	Next    int               `json:"next"`
}

// validateSort returns "recency" only for the exact string "recency";
// all other values (including empty) return "relevance".
// This is the same whitelist guard used inside db.Search() before
// ORDER BY interpolation.
func validateSort(s string) string {
	if s == "recency" {
		return "recency"
	}
	return "relevance"
}

// prepareFTSQuery wraps multi-word queries in quotes so
// SQLite FTS matches the exact phrase rather than individual
// terms.
func prepareFTSQuery(raw string) string {
	if strings.Contains(raw, " ") &&
		!strings.HasPrefix(raw, "\"") {
		return "\"" + raw + "\""
	}
	return raw
}

func (s *Server) handleSearch(
	w http.ResponseWriter, r *http.Request,
) {
	q := r.URL.Query()

	query := strings.TrimSpace(q.Get("q"))
	if query == "" {
		writeError(w, http.StatusBadRequest, "query required")
		return
	}

	limit, ok := parseIntParam(w, r, "limit")
	if !ok {
		return
	}
	limit = clampLimit(limit, db.DefaultSearchLimit, db.MaxSearchLimit)

	cursor, ok := parseNonNegativeIntParam(w, r, "cursor")
	if !ok {
		return
	}

	sort := validateSort(q.Get("sort"))

	if !s.db.HasFTS() {
		writeError(w, http.StatusNotImplemented, "search not available")
		return
	}

	filter := db.SearchFilter{
		Query:   prepareFTSQuery(query),
		Project: q.Get("project"),
		Sort:    sort,
		Cursor:  cursor,
		Limit:   limit,
	}

	page, err := s.db.Search(r.Context(), filter)
	if err != nil {
		if handleContextError(w, err) {
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	results := page.Results
	if results == nil {
		results = []db.SearchResult{}
	}
	writeJSON(w, http.StatusOK, searchResponse{
		Query:   query,
		Results: results,
		Count:   len(results),
		Next:    page.NextCursor,
	})
}
