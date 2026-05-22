package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

func TestValidateSort(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		sortParam string
		wantSort  string
	}{
		{"recency accepted", "recency", "recency"},
		{"relevance accepted", "relevance", "relevance"},
		{"empty defaults to relevance", "", "relevance"},
		{"invalid defaults to relevance", "injection", "relevance"},
		{"SQL injection attempt defaults to relevance", "'; DROP TABLE sessions; --", "relevance"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := validateSort(tt.sortParam)
			if got != tt.wantSort {
				t.Errorf("validateSort(%q) = %q, want %q",
					tt.sortParam, got, tt.wantSort)
			}
		})
	}
}

func TestPrepareFTSQuery(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "single word unchanged", raw: "login", want: "login"},
		{name: "multi-word gets quoted", raw: "fix bug", want: `"fix bug"`},
		{name: "already quoted unchanged", raw: `"fix bug"`, want: `"fix bug"`},
		{name: "empty string unchanged", raw: "", want: ""},
		{name: "three words quoted", raw: "a b c", want: `"a b c"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := prepareFTSQuery(tt.raw)
			if got != tt.want {
				t.Errorf("prepareFTSQuery(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

// searchSpy captures the SearchFilter passed to Search.
type searchSpy struct {
	db.Store
	filter db.SearchFilter
}

func (s *searchSpy) HasFTS() bool { return true }

func (s *searchSpy) Search(
	_ context.Context, f db.SearchFilter,
) (db.SearchPage, error) {
	s.filter = f
	return db.SearchPage{}, nil
}

func TestHandleSearchSortParam(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		query    string
		wantSort string
	}{
		{"recency", "q=hello&sort=recency", "recency"},
		{"relevance explicit", "q=hello&sort=relevance", "relevance"},
		{"default", "q=hello", "relevance"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			spy := &searchSpy{}
			srv := &Server{
				cfg: config.Config{Host: "127.0.0.1"},
				db:  spy,
			}
			req := httptest.NewRequest(
				http.MethodGet,
				"/api/v1/search?"+tt.query, nil,
			)
			w := httptest.NewRecorder()
			srv.handleSearch(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s",
					w.Code, w.Body.String())
			}
			if spy.filter.Sort != tt.wantSort {
				t.Errorf("SearchFilter.Sort = %q, want %q",
					spy.filter.Sort, tt.wantSort)
			}
		})
	}
}
