package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Event is a change notification broadcast to a project's connected
// clients. Task 4 (WebSocket Hub) implements the Broadcaster that delivers
// these; this task only defines the shape and fires it.
type Event struct {
	Type       string `json:"type"` // "file_changed" | "lock_changed" | "conflict"
	Project    string `json:"project"`
	Path       string `json:"path,omitempty"`
	Content    string `json:"content,omitempty"`
	HeadCommit string `json:"head_commit,omitempty"`
	By         string `json:"by,omitempty"`    // device:session
	Owner      string `json:"owner,omitempty"` // lock_changed 用
	At         string `json:"at"`
}

// Broadcaster delivers an Event to all clients subscribed to a project.
// Implemented by the WebSocket Hub (Task 4); nil in the server until then.
type Broadcaster interface {
	Broadcast(project string, ev Event)
}

// projectLock returns the mutex serializing writes+commits for a project,
// creating it on first use.
func (s *Server) projectLock(id string) *sync.Mutex {
	mu, _ := s.projMu.LoadOrStore(id, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

type ApplyRequest struct {
	Session string `json:"session"`
	Path    string `json:"path"`
	Content string `json:"content"`
}

type ApplyResponse struct {
	HeadCommit string `json:"head_commit"`
}

// safeApplyPath validates that path is a safe, relative path inside
// projectDir (no traversal, not absolute, not inside .git), and returns the
// resolved on-disk path to write to.
func safeApplyPath(projectDir, path string) (string, error) {
	if path == "" {
		return "", errors.New("missing field: path")
	}
	if filepath.IsAbs(path) {
		return "", errors.New("path must not be absolute")
	}
	clean := filepath.Clean(path)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.IsAbs(clean) {
		return "", errors.New("path escapes project directory")
	}

	full := filepath.Join(projectDir, clean)
	rel, err := filepath.Rel(projectDir, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", errors.New("path escapes project directory")
	}
	if rel == ".git" || strings.HasPrefix(rel, ".git"+string(filepath.Separator)) {
		return "", errors.New("path must not target the .git directory")
	}

	return full, nil
}

// handleApply is the server's sole git-write entry point: it verifies the
// caller holds the file lock, serializes the write+commit per project,
// writes the file into the project's git work tree, commits, updates
// head_commit, and broadcasts the change.
func (s *Server) handleApply(w http.ResponseWriter, r *http.Request) {
	device, _ := r.Context().Value(contextKeyDevice).(string)
	id := r.PathValue("id")

	var req ApplyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Session == "" {
		writeError(w, http.StatusBadRequest, "missing field: session")
		return
	}

	exists, err := s.rdb.Exists(r.Context(), "agentlink:project:"+id).Result()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if exists == 0 {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}

	projectDir := s.projectDir(id)
	fullPath, err := safeApplyPath(projectDir, req.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	owner := device + ":" + req.Session
	currentOwner, expired := s.lockOwner(r.Context(), id, req.Path)
	if currentOwner != owner || expired {
		writeLockConflict(w, "file is not locked by caller", currentOwner)
		return
	}

	mu := s.projectLock(id)
	mu.Lock()
	defer mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create parent directory")
		return
	}
	if err := os.WriteFile(fullPath, []byte(req.Content), 0o644); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to write file")
		return
	}

	sha, err := s.gitCommit(projectDir, "apply "+req.Path+" by "+owner)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to commit")
		return
	}

	if err := s.rdb.HSet(r.Context(), "agentlink:project:"+id, "head_commit", sha).Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	if s.hub != nil {
		s.hub.Broadcast(id, Event{
			Type:       "file_changed",
			Project:    id,
			Path:       req.Path,
			Content:    req.Content,
			HeadCommit: sha,
			By:         owner,
			At:         time.Now().UTC().Format(time.RFC3339),
		})
	}

	writeJSON(w, http.StatusOK, ApplyResponse{HeadCommit: sha})
}
