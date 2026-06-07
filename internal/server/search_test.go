package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

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
			assert.Equal(t, tt.want, prepareFTSQuery(tt.raw))
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
				mux: http.NewServeMux(),
			}
			srv.routes()
			req := httptest.NewRequest(
				http.MethodGet,
				"/api/v1/search?"+tt.query, nil,
			)
			w := httptest.NewRecorder()
			srv.mux.ServeHTTP(w, req)
			require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
			assert.Equal(t, tt.wantSort, spy.filter.Sort)
		})
	}
}
