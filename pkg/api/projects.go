package api

import (
	"encoding/json"
	"fmt"
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
