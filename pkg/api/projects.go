package api

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// seedIndexHTML is the initial content of a newly created project's work tree.
const seedIndexHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <title>New Project</title>
</head>
<body>
  <h1>New Project</h1>
</body>
</html>
`

type CreateProjectRequest struct {
	Name string `json:"name"`
}

type ProjectResponse struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	HeadCommit string `json:"head_commit"`
}

type ProjectInfo struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	CreatedAt  string `json:"created_at"`
	HeadCommit string `json:"head_commit"`
}

type ListProjectsResponse struct {
	Projects []ProjectInfo `json:"projects"`
}

// projectDir returns the on-disk git work tree path for a project id.
func (s *Server) projectDir(id string) string {
	return filepath.Join(s.dataDir, "work", id)
}

// gitCommit stages all changes in dir and commits them under the repo-local
// cowork identity, returning the resulting HEAD sha.
func (s *Server) gitCommit(dir, msg string) (sha string, err error) {
	if out, err := exec.Command("git", "-C", dir, "add", "-A").CombinedOutput(); err != nil {
		return "", fmt.Errorf("git add: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("git", "-C", dir, "commit", "-m", msg).CombinedOutput(); err != nil {
		return "", fmt.Errorf("git commit: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// initProjectWorkTree creates the on-disk git work tree for a new project:
// mkdir, git init, set repo-local cowork identity, write a seed index.html,
// then commit. Never touches global git config.
func (s *Server) initProjectWorkTree(id string) (headCommit string, err error) {
	dir := s.projectDir(id)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir: %w", err)
	}
	if out, err := exec.Command("git", "-C", dir, "init").CombinedOutput(); err != nil {
		return "", fmt.Errorf("git init: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("git", "-C", dir, "config", "user.name", "cowork").CombinedOutput(); err != nil {
		return "", fmt.Errorf("git config user.name: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("git", "-C", dir, "config", "user.email", "cowork@localhost").CombinedOutput(); err != nil {
		return "", fmt.Errorf("git config user.email: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(seedIndexHTML), 0o644); err != nil {
		return "", fmt.Errorf("write seed index.html: %w", err)
	}

	return s.gitCommit(dir, "initial commit")
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	var req CreateProjectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "missing field: name")
		return
	}

	id := generateID()[:8]

	headCommit, err := s.initProjectWorkTree(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to initialize project work tree")
		return
	}

	now := time.Now().UTC().Format(time.RFC3339)
	projectKey := "agentlink:project:" + id
	if err := s.rdb.HSet(r.Context(), projectKey,
		"id", id,
		"name", req.Name,
		"created_at", now,
		"head_commit", headCommit,
	).Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := s.rdb.SAdd(r.Context(), "agentlink:projects", id).Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	writeJSON(w, http.StatusOK, ProjectResponse{
		ID:         id,
		Name:       req.Name,
		HeadCommit: headCommit,
	})
}

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	ids, err := s.rdb.SMembers(r.Context(), "agentlink:projects").Result()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	projects := make([]ProjectInfo, 0, len(ids))
	for _, id := range ids {
		data, err := s.rdb.HGetAll(r.Context(), "agentlink:project:"+id).Result()
		if err != nil || len(data) == 0 {
			continue
		}
		projects = append(projects, ProjectInfo{
			ID:         data["id"],
			Name:       data["name"],
			CreatedAt:  data["created_at"],
			HeadCommit: data["head_commit"],
		})
	}

	writeJSON(w, http.StatusOK, ListProjectsResponse{Projects: projects})
}

type TreeFileEntry struct {
	Path   string `json:"path"`
	Locked bool   `json:"locked"`
	Owner  string `json:"owner"`
}

type TreeResponse struct {
	Files []TreeFileEntry `json:"files"`
}

type SnapshotFileEntry struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type SnapshotResponse struct {
	HeadCommit string              `json:"head_commit"`
	Files      []SnapshotFileEntry `json:"files"`
}

// listProjectFiles walks a project's work tree and returns the forward-slash
// relative path of every regular file, skipping the .git directory entirely.
func listProjectFiles(dir string) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			// Non-regular entries (symlinks, sockets, devices, etc.) are
			// intentionally skipped: following a symlink here could walk
			// or read outside the project's work tree.
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return paths, nil
}

// handleTree lists every file in a project's work tree along with its
// current lock status, for the GUI's file tree view.
func (s *Server) handleTree(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	exists, err := s.rdb.Exists(r.Context(), "agentlink:project:"+id).Result()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if exists == 0 {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}

	paths, err := listProjectFiles(s.projectDir(id))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to walk project directory")
		return
	}

	files := make([]TreeFileEntry, 0, len(paths))
	for _, rel := range paths {
		owner, expired := s.lockOwner(r.Context(), id, rel)
		files = append(files, TreeFileEntry{
			Path:   rel,
			Locked: owner != "" && !expired,
			Owner:  owner,
		})
	}

	writeJSON(w, http.StatusOK, TreeResponse{Files: files})
}

// handleSnapshot returns the full content of every file in a project's work
// tree plus the current head_commit, so a sync daemon can seed a client's
// local copy on startup.
func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	projectKey := "agentlink:project:" + id
	exists, err := s.rdb.Exists(r.Context(), projectKey).Result()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if exists == 0 {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}

	// Hold the same per-project mutex handleApply uses around its
	// write+commit+HSet head_commit, spanning both the head_commit read
	// and the file walk/reads below. Without this, a concurrent apply can
	// land in the gap and produce a snapshot whose head_commit doesn't
	// match the returned file bytes (or a read mid-write).
	mu := s.projectLock(id)
	mu.Lock()
	defer mu.Unlock()

	headCommit, err := s.rdb.HGet(r.Context(), projectKey, "head_commit").Result()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	dir := s.projectDir(id)
	paths, err := listProjectFiles(dir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to walk project directory")
		return
	}

	files := make([]SnapshotFileEntry, 0, len(paths))
	for _, rel := range paths {
		content, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to read file")
			return
		}
		files = append(files, SnapshotFileEntry{Path: rel, Content: string(content)})
	}

	writeJSON(w, http.StatusOK, SnapshotResponse{HeadCommit: headCommit, Files: files})
}
