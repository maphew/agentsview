package server

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.kenn.io/agentsview/internal/importer"
)

func TestHandleImportClaudeAI(t *testing.T) {
	srv := testServer(t, 5*time.Second)

	conversations := `[
      {
        "uuid": "api-test-001",
        "name": "API Test",
        "summary": "",
        "created_at": "2026-03-01T10:00:00.000000Z",
        "updated_at": "2026-03-01T10:05:00.000000Z",
        "account": {"uuid": "acct-1"},
        "chat_messages": [
          {
            "uuid": "m1",
            "text": "Test message",
            "content": [{"type":"text","text":"Test message"}],
            "sender": "human",
            "created_at": "2026-03-01T10:00:00.000000Z",
            "updated_at": "2026-03-01T10:00:00.000000Z",
            "attachments": [],
            "files": []
          }
        ]
      }
    ]`

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "conversations.json")
	if err != nil {
		t.Fatalf("creating form file: %v", err)
	}
	_, _ = part.Write([]byte(conversations))
	writer.Close()

	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/import/claude-ai",
		&body,
	)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf(
			"status = %d, want 200: %s",
			rec.Code, rec.Body.String(),
		)
	}

	var stats importer.ImportStats
	if err := json.NewDecoder(rec.Body).Decode(&stats); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if stats.Imported != 1 {
		t.Errorf("imported = %d, want 1", stats.Imported)
	}
	if stats.Updated != 0 {
		t.Errorf("updated = %d, want 0", stats.Updated)
	}
}

func TestHandleImportChatGPT_RequiresZip(t *testing.T) {
	srv := testServer(t, 5*time.Second)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "data.json")
	if err != nil {
		t.Fatalf("creating form file: %v", err)
	}
	_, _ = part.Write([]byte("[]"))
	writer.Close()

	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/import/chatgpt",
		&body,
	)
	req.Header.Set(
		"Content-Type", writer.FormDataContentType(),
	)

	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf(
			"status = %d, want 400: %s",
			rec.Code, rec.Body.String(),
		)
	}
}

func TestHandleImportClaudeAI_SSE(t *testing.T) {
	srv := testServer(t, 5*time.Second)

	conversations := `[{
      "uuid": "sse-test-001",
      "name": "SSE Test",
      "created_at": "2026-03-01T10:00:00.000000Z",
      "updated_at": "2026-03-01T10:05:00.000000Z",
      "chat_messages": [{
        "uuid": "m1", "text": "hello", "sender": "human",
        "content": [{"type":"text","text":"hello"}],
        "created_at": "2026-03-01T10:00:00.000000Z"
      }]
    }]`

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile(
		"file", "conversations.json",
	)
	if err != nil {
		t.Fatalf("creating form file: %v", err)
	}
	_, _ = part.Write([]byte(conversations))
	writer.Close()

	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/import/claude-ai",
		&body,
	)
	req.Header.Set(
		"Content-Type", writer.FormDataContentType(),
	)
	req.Header.Set("Accept", "text/event-stream")

	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf(
			"status = %d, want 200: %s",
			rec.Code, rec.Body.String(),
		)
	}

	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/event-stream") {
		t.Fatalf(
			"Content-Type = %q, want text/event-stream", ct,
		)
	}

	// Parse the done event from the SSE body.
	var stats importer.ImportStats
	lines := strings.Split(rec.Body.String(), "\n")
	for i, line := range lines {
		if line == "event: done" && i+1 < len(lines) {
			data := strings.TrimPrefix(
				lines[i+1], "data: ",
			)
			if err := json.Unmarshal(
				[]byte(data), &stats,
			); err != nil {
				t.Fatalf("decoding done event: %v", err)
			}
		}
	}
	if stats.Imported != 1 {
		t.Errorf("imported = %d, want 1", stats.Imported)
	}
}

func TestHandleImportClaudeAI_NoFile(t *testing.T) {
	srv := testServer(t, 5*time.Second)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	writer.Close()

	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/import/claude-ai",
		&body,
	)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf(
			"status = %d, want 400: %s",
			rec.Code, rec.Body.String(),
		)
	}
}
