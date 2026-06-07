package server

import (
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

type searchResponse struct {
	Query   string            `json:"query"`
	Results []db.SearchResult `json:"results"`
	Count   int               `json:"count"`
	Next    int               `json:"next"`
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
