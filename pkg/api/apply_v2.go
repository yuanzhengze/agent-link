package api

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// EventV2 is a team-scoped change notification broadcast to a project's
// connected clients. Task 4 implements the BroadcasterV2 that delivers these.
type EventV2 struct {
	Type       string       `json:"type"` // "file_changed" | "lock_changed" | "conflict"
	TeamID     string       `json:"team_id"`
	ProjectID  string       `json:"project_id"`
	Path       string       `json:"path,omitempty"`
	Content    string       `json:"content,omitempty"`
	HeadCommit string       `json:"head_commit,omitempty"`
	By         LockOwnerV2  `json:"by,omitempty"`
	Owner      *LockOwnerV2 `json:"owner,omitempty"`
	At         string       `json:"at"`
}

// BroadcasterV2 delivers an EventV2 to all clients subscribed to a team's
// project. Implemented by the team WebSocket hub (Task 4); nil until then.
type BroadcasterV2 interface {
	Broadcast(teamID, projectID string, event EventV2)
}

type ApplyRequestV2 struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type ApplyResponseV2 struct {
	HeadCommit string `json:"head_commit"`
}

// teamProjectMutexKey names the per-project mutex serializing write+commit and
// atomic snapshot reads for a specific team's project.
func teamProjectMutexKey(teamID, projectID string) string {
	return teamID + ":" + projectID
}

// lockStatusV2 reads the current lock holder for a team/project/path. active is
// true only when a holder exists and the lease has not expired.
func (s *Server) lockStatusV2(r *http.Request, teamID, projectID, path string) (owner LockOwnerV2, ownerID string, active bool) {
	data, err := s.rdb.HGetAll(r.Context(), lockV2Key(teamID, projectID, path)).Result()
	if err != nil || len(data) == 0 {
		return LockOwnerV2{}, "", false
	}
	exp, _ := strconv.ParseInt(data["lease_expires_at"], 10, 64)
	owner = LockOwnerV2{
		UserID:      data["user_id"],
		Username:    data["username"],
		DeviceID:    data["device_id"],
		DeviceName:  data["device_name"],
		SessionName: data["session_name"],
		Label:       data["label"],
	}
	ownerID = data["owner_id"]
	active = ownerID != "" && exp > time.Now().Unix()
	return owner, ownerID, active
}

// handleApplyV2 is the server's sole git-write entry point for team projects:
// it verifies the caller holds the file lock, serializes write+commit per
// project, writes into the team's work tree, commits, updates head_commit, and
// broadcasts the change.
func (s *Server) handleApplyV2(w http.ResponseWriter, r *http.Request) {
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	projectID := r.PathValue("project_id")

	var req ApplyRequestV2
	if !decodeLockJSON(w, r, &req) {
		return
	}

	if _, err := s.loadTeamProject(r.Context(), actor.TeamID, projectID); err != nil {
		if errors.Is(err, errProjectNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	projectDir := s.projectDirV2(actor.TeamID, projectID)
	fullPath, err := safeApplyPath(projectDir, req.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	lockPath, err := cleanLockPathV2(req.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	holder, ownerID, active := s.lockStatusV2(r, actor.TeamID, projectID, lockPath)
	if ownerID != lockOwnerID(actor) || !active {
		writeLockConflictV2(w, "file is not locked by caller", holder)
		return
	}

	mu := s.projectLock(teamProjectMutexKey(actor.TeamID, projectID))
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

	owner := lockOwnerFromActor(actor)
	sha, err := s.gitCommit(projectDir, "apply "+lockPath+" by "+owner.Label)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to commit")
		return
	}
	if !validGitObjectID(sha) {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	if err := s.rdb.HSet(r.Context(), projectV2Key(projectID), "head_commit", sha).Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	if s.hubV2 != nil {
		s.hubV2.Broadcast(actor.TeamID, projectID, EventV2{
			Type:       "file_changed",
			TeamID:     actor.TeamID,
			ProjectID:  projectID,
			Path:       lockPath,
			Content:    req.Content,
			HeadCommit: sha,
			By:         owner,
			At:         time.Now().UTC().Format(time.RFC3339),
		})
	}

	writeJSON(w, http.StatusOK, ApplyResponseV2{HeadCommit: sha})
}
