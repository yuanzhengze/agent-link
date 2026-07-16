package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/team/agentlink/pkg/auth"
)

// lockV2LeaseTTL is the default lease duration granted on acquire/refresh.
const lockV2LeaseTTL = 120 * time.Second

// lockAcquireV2Script atomically grants a per-file, team-scoped lock. The lock
// is free when nobody holds it, when the caller already holds it (refresh), or
// when the current holder's lease has expired (reclaim). Ownership is derived
// entirely from server-side ARGV, never from the request body.
//
// KEYS[1]=lock hash  KEYS[2]=holder's locks set
// ARGV[1]=owner_id  ARGV[2]=now  ARGV[3]=lease_expires  ARGV[4]=member
// ARGV[5]=task_id   ARGV[6]=owner_set  ARGV[7]=user_id  ARGV[8]=username
// ARGV[9]=device_id ARGV[10]=device_name ARGV[11]=session_name ARGV[12]=label
// ARGV[13]=path
var lockAcquireV2Script = goredis.NewScript(`
local cur = redis.call('HGET', KEYS[1], 'owner_id')
local exp = tonumber(redis.call('HGET', KEYS[1], 'lease_expires_at') or '0')
if cur and cur ~= ARGV[1] and exp > tonumber(ARGV[2]) then
  return 0
end
local acquired = ARGV[2]
if cur == ARGV[1] then
  local prev = redis.call('HGET', KEYS[1], 'acquired_at')
  if prev then acquired = prev end
elseif cur then
  local oldset = redis.call('HGET', KEYS[1], 'owner_set')
  if oldset then redis.call('SREM', oldset, ARGV[4]) end
end
redis.call('HSET', KEYS[1],
  'owner_id', ARGV[1],
  'acquired_at', acquired,
  'lease_expires_at', ARGV[3],
  'task_id', ARGV[5],
  'owner_set', ARGV[6],
  'user_id', ARGV[7],
  'username', ARGV[8],
  'device_id', ARGV[9],
  'device_name', ARGV[10],
  'session_name', ARGV[11],
  'label', ARGV[12],
  'path', ARGV[13])
redis.call('SADD', KEYS[2], ARGV[4])
return 1
`)

// lockReleaseV2Script atomically releases a lock: no-op if free, rejected if
// held by someone else (unless force), otherwise deletes the lock and removes
// the member from the actual holder's locks set.
//
// KEYS[1]=lock hash  ARGV[1]=owner_id  ARGV[2]=member  ARGV[3]=force("1"/"0")
var lockReleaseV2Script = goredis.NewScript(`
local cur = redis.call('HGET', KEYS[1], 'owner_id')
if cur == false then return 1 end
if cur ~= ARGV[1] and ARGV[3] ~= '1' then return 0 end
local oldset = redis.call('HGET', KEYS[1], 'owner_set')
redis.call('DEL', KEYS[1])
if oldset then redis.call('SREM', oldset, ARGV[2]) end
return 1
`)

// lockV2Key returns the Redis hash key for a team/project/path lock.
func lockV2Key(teamID, projectID, path string) string {
	return "agentlink:v2:lock:" + teamID + ":" + projectID + ":" + path
}

// actorLocksV2Key returns the Redis set key tracking locks held by a specific
// actor (user + device + session) within a team.
func actorLocksV2Key(teamID string, actor auth.Actor) string {
	return "agentlink:v2:locks:" + teamID + ":" + actor.UserID + ":" +
		actor.DeviceID + ":" + actor.SessionName
}

// LockOwnerV2 is the structured, non-secret identity of a lock holder.
type LockOwnerV2 struct {
	UserID      string `json:"user_id"`
	Username    string `json:"username"`
	DeviceID    string `json:"device_id"`
	DeviceName  string `json:"device_name"`
	SessionName string `json:"session_name"`
	Label       string `json:"label"`
}

func lockOwnerFromActor(actor auth.Actor) LockOwnerV2 {
	return LockOwnerV2{
		UserID:      actor.UserID,
		Username:    actor.Username,
		DeviceID:    actor.DeviceID,
		DeviceName:  actor.DeviceName,
		SessionName: actor.SessionName,
		Label:       lockOwnerLabel(actor.Username, actor.DeviceName, actor.SessionName),
	}
}

func lockOwnerLabel(username, deviceName, sessionName string) string {
	return username + "@" + deviceName + "/" + sessionName
}

// ownerID is the canonical stable identity string compared inside Lua.
func lockOwnerID(actor auth.Actor) string {
	return actor.UserID + "|" + actor.DeviceID + "|" + actor.SessionName
}

// lockMember is the value stored in a holder's locks set for reclaim/release.
func lockMember(projectID, path string) string {
	return projectID + "|" + path
}

type LockAcquireRequestV2 struct {
	ProjectID string `json:"project_id"`
	Path      string `json:"path"`
	TaskID    string `json:"task_id,omitempty"`
}

type LockAcquireResponseV2 struct {
	Owner LockOwnerV2 `json:"owner"`
}

type LockReleaseRequestV2 struct {
	ProjectID string `json:"project_id"`
	Path      string `json:"path"`
	Force     bool   `json:"force,omitempty"`
}

type LockReleaseResponseV2 struct {
	Ok bool `json:"ok"`
}

// LockConflictResponseV2 is the 409 body for acquire/release, exposing the
// current holder so callers can show who owns the file.
type LockConflictResponseV2 struct {
	Error string      `json:"error"`
	Owner LockOwnerV2 `json:"owner"`
}

type LockInfoV2 struct {
	Path           string      `json:"path"`
	Owner          LockOwnerV2 `json:"owner"`
	AcquiredAt     int64       `json:"acquired_at"`
	LeaseExpiresAt int64       `json:"lease_expires_at"`
	TaskID         string      `json:"task_id,omitempty"`
}

type LockListResponseV2 struct {
	Locks []LockInfoV2 `json:"locks"`
}

// cleanLockPathV2 validates that path is a safe relative path (no traversal,
// not absolute, not inside .git) and returns its canonical cleaned form.
func cleanLockPathV2(path string) (string, error) {
	if path == "" {
		return "", errors.New("missing field: path")
	}
	if filepath.IsAbs(path) {
		return "", errors.New("path must not be absolute")
	}
	clean := filepath.Clean(path)
	if clean == "." {
		return "", errors.New("path must not be empty")
	}
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("path escapes project directory")
	}
	for _, seg := range strings.Split(clean, string(filepath.Separator)) {
		if strings.EqualFold(seg, ".git") {
			return "", errors.New("path must not target the .git directory")
		}
	}
	return clean, nil
}

func writeLockConflictV2(w http.ResponseWriter, msg string, owner LockOwnerV2) {
	writeJSON(w, http.StatusConflict, LockConflictResponseV2{Error: msg, Owner: owner})
}

// decodeLockJSON decodes a size-limited JSON body while tolerating (and
// ignoring) unknown fields. Ownership is always derived from the authenticated
// actor, so a client that attempts to smuggle owner/user_id/session fields has
// no effect rather than being rejected outright.
func decodeLockJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxAuthBodyBytes)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	return true
}

// readLockOwnerV2 reads the current holder metadata from a lock hash. It
// returns a zero owner if the lock no longer exists.
func (s *Server) readLockOwnerV2(ctx context.Context, key string) LockOwnerV2 {
	data, err := s.rdb.HGetAll(ctx, key).Result()
	if err != nil || len(data) == 0 {
		return LockOwnerV2{}
	}
	return LockOwnerV2{
		UserID:      data["user_id"],
		Username:    data["username"],
		DeviceID:    data["device_id"],
		DeviceName:  data["device_name"],
		SessionName: data["session_name"],
		Label:       data["label"],
	}
}

func (s *Server) handleLockAcquireV2(w http.ResponseWriter, r *http.Request) {
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req LockAcquireRequestV2
	if !decodeLockJSON(w, r, &req) {
		return
	}
	if req.ProjectID == "" {
		writeError(w, http.StatusBadRequest, "missing field: project_id")
		return
	}
	path, err := cleanLockPathV2(req.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if _, err := s.loadTeamProject(r.Context(), actor.TeamID, req.ProjectID); err != nil {
		if errors.Is(err, errProjectNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	owner := lockOwnerFromActor(actor)
	now := time.Now().Unix()
	leaseExpiresAt := now + int64(lockV2LeaseTTL.Seconds())
	key := lockV2Key(actor.TeamID, req.ProjectID, path)

	code, err := lockAcquireV2Script.Run(
		r.Context(),
		s.rdb,
		[]string{key, actorLocksV2Key(actor.TeamID, actor)},
		lockOwnerID(actor),
		now,
		leaseExpiresAt,
		lockMember(req.ProjectID, path),
		req.TaskID,
		actorLocksV2Key(actor.TeamID, actor),
		owner.UserID,
		owner.Username,
		owner.DeviceID,
		owner.DeviceName,
		owner.SessionName,
		owner.Label,
		path,
	).Int64()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if code == 0 {
		writeLockConflictV2(w, "file is locked by another session", s.readLockOwnerV2(r.Context(), key))
		return
	}

	writeJSON(w, http.StatusOK, LockAcquireResponseV2{Owner: owner})
}

func (s *Server) handleLockReleaseV2(w http.ResponseWriter, r *http.Request) {
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req LockReleaseRequestV2
	if !decodeLockJSON(w, r, &req) {
		return
	}
	if req.ProjectID == "" {
		writeError(w, http.StatusBadRequest, "missing field: project_id")
		return
	}
	path, err := cleanLockPathV2(req.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if _, err := s.loadTeamProject(r.Context(), actor.TeamID, req.ProjectID); err != nil {
		if errors.Is(err, errProjectNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	force := "0"
	if req.Force {
		force = "1"
	}
	key := lockV2Key(actor.TeamID, req.ProjectID, path)

	code, err := lockReleaseV2Script.Run(
		r.Context(),
		s.rdb,
		[]string{key},
		lockOwnerID(actor),
		lockMember(req.ProjectID, path),
		force,
	).Int64()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if code == 0 {
		writeLockConflictV2(w, "file is locked by another session", s.readLockOwnerV2(r.Context(), key))
		return
	}

	writeJSON(w, http.StatusOK, LockReleaseResponseV2{Ok: true})
}

func (s *Server) handleLockListV2(w http.ResponseWriter, r *http.Request) {
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	projectID := r.URL.Query().Get("project_id")
	if projectID == "" {
		writeError(w, http.StatusBadRequest, "missing project_id parameter")
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

	prefix := "agentlink:v2:lock:" + actor.TeamID + ":" + projectID + ":"
	var keys []string
	var cursor uint64
	for {
		batch, next, err := s.rdb.Scan(r.Context(), cursor, prefix+"*", 100).Result()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		keys = append(keys, batch...)
		cursor = next
		if cursor == 0 {
			break
		}
	}

	locks := make([]LockInfoV2, 0, len(keys))
	for _, key := range keys {
		data, err := s.rdb.HGetAll(r.Context(), key).Result()
		if err != nil || len(data) == 0 {
			continue
		}
		acquiredAt, _ := strconv.ParseInt(data["acquired_at"], 10, 64)
		leaseExpiresAt, _ := strconv.ParseInt(data["lease_expires_at"], 10, 64)
		locks = append(locks, LockInfoV2{
			Path: data["path"],
			Owner: LockOwnerV2{
				UserID:      data["user_id"],
				Username:    data["username"],
				DeviceID:    data["device_id"],
				DeviceName:  data["device_name"],
				SessionName: data["session_name"],
				Label:       data["label"],
			},
			AcquiredAt:     acquiredAt,
			LeaseExpiresAt: leaseExpiresAt,
			TaskID:         data["task_id"],
		})
	}

	writeJSON(w, http.StatusOK, LockListResponseV2{Locks: locks})
}
