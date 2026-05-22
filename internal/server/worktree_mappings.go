package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

type worktreeMappingsResponse struct {
	Machine  string                      `json:"machine"`
	Mappings []db.WorktreeProjectMapping `json:"mappings"`
}

type worktreeMappingRequest struct {
	PathPrefix *string `json:"path_prefix,omitempty"`
	Project    *string `json:"project,omitempty"`
	Enabled    *bool   `json:"enabled,omitempty"`
}

type applyWorktreeMappingsResponse struct {
	Machine string `json:"machine"`
	db.ApplyWorktreeProjectMappingsResult
}

func (s *Server) localWorktreeMappingDB(
	w http.ResponseWriter,
) (*db.DB, string, bool) {
	localDB, ok := s.db.(*db.DB)
	if !ok || localDB == nil || localDB.ReadOnly() || s.engine == nil {
		writeError(w, http.StatusNotImplemented,
			"not available in remote mode")
		return nil, "", false
	}
	machine := strings.TrimSpace(s.engine.Machine())
	if machine == "" {
		machine = "local"
	}
	return localDB, machine, true
}

func (s *Server) handleListWorktreeMappings(
	w http.ResponseWriter, r *http.Request,
) {
	localDB, machine, ok := s.localWorktreeMappingDB(w)
	if !ok {
		return
	}
	mappings, err := localDB.ListWorktreeProjectMappings(
		r.Context(), machine,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, worktreeMappingsResponse{
		Machine:  machine,
		Mappings: mappings,
	})
}

func (s *Server) handleCreateWorktreeMapping(
	w http.ResponseWriter, r *http.Request,
) {
	localDB, machine, ok := s.localWorktreeMappingDB(w)
	if !ok {
		return
	}

	var req worktreeMappingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.PathPrefix == nil || req.Project == nil {
		writeError(w, http.StatusBadRequest,
			"path_prefix and project are required")
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	mapping, err := localDB.CreateWorktreeProjectMapping(
		r.Context(),
		db.WorktreeProjectMapping{
			Machine:    machine,
			PathPrefix: *req.PathPrefix,
			Project:    *req.Project,
			Enabled:    enabled,
		},
	)
	writeWorktreeMappingResult(w, mapping, err, http.StatusCreated)
}

func (s *Server) handleUpdateWorktreeMapping(
	w http.ResponseWriter, r *http.Request,
) {
	localDB, machine, ok := s.localWorktreeMappingDB(w)
	if !ok {
		return
	}
	id, ok := parseWorktreeMappingID(w, r)
	if !ok {
		return
	}

	var req worktreeMappingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.PathPrefix == nil || req.Project == nil ||
		req.Enabled == nil {
		writeError(w, http.StatusBadRequest,
			"path_prefix, project, and enabled are required")
		return
	}
	mapping, err := localDB.UpdateWorktreeProjectMapping(
		r.Context(),
		machine,
		id,
		db.WorktreeProjectMapping{
			PathPrefix: *req.PathPrefix,
			Project:    *req.Project,
			Enabled:    *req.Enabled,
		},
	)
	writeWorktreeMappingResult(w, mapping, err, http.StatusOK)
}

func (s *Server) handleDeleteWorktreeMapping(
	w http.ResponseWriter, r *http.Request,
) {
	localDB, machine, ok := s.localWorktreeMappingDB(w)
	if !ok {
		return
	}
	id, ok := parseWorktreeMappingID(w, r)
	if !ok {
		return
	}
	err := localDB.DeleteWorktreeProjectMapping(
		r.Context(), machine, id,
	)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "mapping not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleApplyWorktreeMappings(
	w http.ResponseWriter, r *http.Request,
) {
	localDB, machine, ok := s.localWorktreeMappingDB(w)
	if !ok {
		return
	}
	result, err := localDB.ApplyWorktreeProjectMappings(
		r.Context(), machine,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, applyWorktreeMappingsResponse{
		Machine:                            machine,
		ApplyWorktreeProjectMappingsResult: result,
	})
}

func parseWorktreeMappingID(
	w http.ResponseWriter,
	r *http.Request,
) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusNotFound, "mapping not found")
		return 0, false
	}
	return id, true
}

func writeWorktreeMappingResult(
	w http.ResponseWriter,
	mapping db.WorktreeProjectMapping,
	err error,
	status int,
) {
	switch {
	case err == nil:
		writeJSON(w, status, mapping)
	case strings.Contains(err.Error(), "required"):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, db.ErrWorktreeMappingDuplicate):
		writeError(w, http.StatusConflict,
			"worktree mapping already exists")
	case errors.Is(err, sql.ErrNoRows):
		writeError(w, http.StatusNotFound, "mapping not found")
	default:
		log.Printf("worktree mapping write: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}
