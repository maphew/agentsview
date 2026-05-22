package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"go.kenn.io/agentsview/internal/db"
)

func TestWorktreeMappingsAPIUsesCurrentMachine(t *testing.T) {
	te := setup(t)
	prefix := filepath.Join(t.TempDir(), "app.worktrees")

	created := postWorktreeMapping(t, te, map[string]any{
		"path_prefix": prefix,
		"project":     "canonical-app",
		"machine":     "other-machine",
	})
	if created.Machine != "test" {
		t.Fatalf("machine = %q, want test", created.Machine)
	}
	if created.Project != "canonical_app" {
		t.Fatalf("project = %q, want canonical_app", created.Project)
	}
	if !created.Enabled {
		t.Fatal("created mapping should default enabled")
	}

	var list struct {
		Machine  string                      `json:"machine"`
		Mappings []db.WorktreeProjectMapping `json:"mappings"`
	}
	w := te.get(t, "/api/v1/settings/worktree-mappings")
	assertStatus(t, w, http.StatusOK)
	decodeInto(t, w, &list)
	if list.Machine != "test" || len(list.Mappings) != 1 {
		t.Fatalf("list = %+v, want one test-machine mapping", list)
	}

	updated := putWorktreeMapping(t, te, created.ID, map[string]any{
		"path_prefix": prefix,
		"project":     "disabled-app",
		"enabled":     false,
	})
	if updated.Enabled {
		t.Fatal("updated mapping should be disabled")
	}
	if updated.Project != "disabled_app" {
		t.Fatalf("updated project = %q, want disabled_app", updated.Project)
	}

	req := httptest.NewRequest(
		http.MethodDelete,
		"/api/v1/settings/worktree-mappings/"+
			strconv.FormatInt(created.ID, 10),
		nil,
	)
	req.Header.Set("Origin", "http://127.0.0.1:0")
	delW := httptest.NewRecorder()
	te.handler.ServeHTTP(delW, req)
	assertStatus(t, delW, http.StatusNoContent)

	w = te.get(t, "/api/v1/settings/worktree-mappings")
	assertStatus(t, w, http.StatusOK)
	decodeInto(t, w, &list)
	if len(list.Mappings) != 0 {
		t.Fatalf("mappings after delete = %+v, want none", list.Mappings)
	}
}

func TestWorktreeMappingsAPIApply(t *testing.T) {
	te := setup(t)
	prefix := filepath.Join(t.TempDir(), "app.worktrees")
	_ = postWorktreeMapping(t, te, map[string]any{
		"path_prefix": prefix,
		"project":     "canonical-app",
	})
	if err := te.db.UpsertSession(db.Session{
		ID:      "s1",
		Machine: "test",
		Agent:   "claude",
		Project: "feature_login",
		Cwd:     filepath.Join(prefix, "feature-login"),
	}); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}

	w := te.post(t, "/api/v1/settings/worktree-mappings/apply", `{}`)
	assertStatus(t, w, http.StatusOK)
	var resp struct {
		Machine         string `json:"machine"`
		MatchedSessions int    `json:"matched_sessions"`
		UpdatedSessions int    `json:"updated_sessions"`
	}
	decodeInto(t, w, &resp)
	if resp.Machine != "test" ||
		resp.MatchedSessions != 1 ||
		resp.UpdatedSessions != 1 {
		t.Fatalf("apply response = %+v, want test matched=1 updated=1", resp)
	}
	sess, err := te.db.GetSession(context.Background(), "s1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.Project != "canonical_app" {
		t.Fatalf("session project = %q, want canonical_app", sess.Project)
	}
}

func TestWorktreeMappingsAPIRejectsRemoteMode(t *testing.T) {
	te := setupPGMode(t)
	w := te.get(t, "/api/v1/settings/worktree-mappings")
	assertStatus(t, w, http.StatusNotImplemented)

	w = te.post(t, "/api/v1/settings/worktree-mappings", `{
		"path_prefix": "/tmp/app.worktrees",
		"project": "app"
	}`)
	assertStatus(t, w, http.StatusNotImplemented)

	w = te.post(t, "/api/v1/settings/worktree-mappings/apply", `{}`)
	assertStatus(t, w, http.StatusNotImplemented)
}

func TestWorktreeMappingsAPIMalformedIDIsNotFound(t *testing.T) {
	te := setup(t)
	req := httptest.NewRequest(
		http.MethodPut,
		"/api/v1/settings/worktree-mappings/apply",
		bytes.NewReader([]byte(`{}`)),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://127.0.0.1:0")
	w := httptest.NewRecorder()
	te.handler.ServeHTTP(w, req)
	assertStatus(t, w, http.StatusNotFound)
}

func postWorktreeMapping(
	t *testing.T,
	te *testEnv,
	body map[string]any,
) db.WorktreeProjectMapping {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	w := te.post(
		t,
		"/api/v1/settings/worktree-mappings",
		string(data),
	)
	assertStatus(t, w, http.StatusCreated)
	return decode[db.WorktreeProjectMapping](t, w)
}

func putWorktreeMapping(
	t *testing.T,
	te *testEnv,
	id int64,
	body map[string]any,
) db.WorktreeProjectMapping {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	req := httptest.NewRequest(
		http.MethodPut,
		"/api/v1/settings/worktree-mappings/"+
			strconv.FormatInt(id, 10),
		bytes.NewReader(data),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://127.0.0.1:0")
	w := httptest.NewRecorder()
	te.handler.ServeHTTP(w, req)
	assertStatus(t, w, http.StatusOK)
	return decode[db.WorktreeProjectMapping](t, w)
}

func decodeInto(
	t *testing.T,
	w *httptest.ResponseRecorder,
	target any,
) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), target); err != nil {
		t.Fatalf("decoding JSON: %v\nbody: %s", err, w.Body.String())
	}
}
