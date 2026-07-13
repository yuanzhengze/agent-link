package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// lockLeaseTTL is the default lease duration granted on acquire/refresh.
const lockLeaseTTL = 120 * time.Second

// lockAcquireScript atomically grants a per-file lock: it's free if no one
// holds it, if the caller already holds it (refresh), or if the current
// holder's lease has expired (reclaim). Otherwise it's held by someone else
// and the acquire is rejected.
//
// KEYS[1]=agentlink:lock:<project>:<path>  KEYS[2]=agentlink:locks:<device>:<session>
// ARGV[1]=owner  ARGV[2]=now(unix)  ARGV[3]=lease_expires(unix)
// ARGV[4]=member("project|path")  ARGV[5]=task_id
const lockAcquireScript = `
local cur = redis.call('HGET', KEYS[1], 'owner')
local exp = tonumber(redis.call('HGET', KEYS[1], 'lease_expires_at') or '0')
if cur and cur ~= ARGV[1] and exp > tonumber(ARGV[2]) then
  return {0, cur}                      -- held by someone else, not expired
end
if cur and cur ~= ARGV[1] then
  redis.call('SREM', 'agentlink:locks:' .. cur, ARGV[4])  -- reclaim: clean old owner's set
end
redis.call('HSET', KEYS[1], 'owner', ARGV[1], 'acquired_at', ARGV[2],
  'lease_expires_at', ARGV[3], 'task_id', ARGV[5])
redis.call('SADD', KEYS[2], ARGV[4])
return {1, ARGV[1]}                     -- granted (new/refresh/reclaim)
`

// lockReleaseScript atomically releases a lock: no-op if free, rejected if
// held by someone else (unless force), otherwise deletes the lock and
// removes the member from the holder's locks set.
//
// KEYS[1]=lock  KEYS[2]=locks set  ARGV[1]=owner  ARGV[2]=member  ARGV[3]=force("1"/"0")
const lockReleaseScript = `
local cur = redis.call('HGET', KEYS[1], 'owner')
if cur == false then return {1, ''} end
if cur ~= ARGV[1] and ARGV[3] ~= '1' then return {0, cur} end
redis.call('DEL', KEYS[1]); redis.call('SREM', 'agentlink:locks:' .. cur, ARGV[2])
return {1, ''}
`

// lockKey returns the Redis hash key for a project/path lock.
func lockKey(project, path string) string {
	return "agentlink:lock:" + project + ":" + path
}

// locksSetKey returns the Redis set key tracking locks held by a device:session.
func locksSetKey(device, session string) string {
	return "agentlink:locks:" + device + ":" + session
}

// lockOwner reads the current owner and whether its lease has expired.
// owner is "" if the lock is free.
func (s *Server) lockOwner(ctx context.Context, project, path string) (owner string, expired bool) {
	data, err := s.rdb.HMGet(ctx, lockKey(project, path), "owner", "lease_expires_at").Result()
	if err != nil || len(data) < 2 {
		return "", false
	}
	owner, _ = data[0].(string)
	if owner == "" {
		return "", false
	}
	expStr, _ := data[1].(string)
	exp, _ := strconv.ParseInt(expStr, 10, 64)
	return owner, exp <= time.Now().Unix()
}

type LockAcquireRequest struct {
	Project string `json:"project"`
	Session string `json:"session"`
	Path    string `json:"path"`
	TaskID  string `json:"task_id,omitempty"`
}

type LockAcquireResponse struct {
	Owner string `json:"owner"`
}

type LockReleaseRequest struct {
	Project string `json:"project"`
	Session string `json:"session"`
	Path    string `json:"path"`
	Force   bool   `json:"force,omitempty"`
}

type LockReleaseResponse struct {
	Ok bool `json:"ok"`
}

// LockConflictResponse is the 409 body for both acquire and release,
// carrying the current lock owner (device:session) so callers can show who
// holds the file.
type LockConflictResponse struct {
	Error string `json:"error"`
	Owner string `json:"owner"`
}

type LockInfo struct {
	Path           string `json:"path"`
	Owner          string `json:"owner"`
	AcquiredAt     int64  `json:"acquired_at"`
	LeaseExpiresAt int64  `json:"lease_expires_at"`
}

type LockListResponse struct {
	Locks []LockInfo `json:"locks"`
}

func writeLockConflict(w http.ResponseWriter, msg, owner string) {
	writeJSON(w, http.StatusConflict, LockConflictResponse{Error: msg, Owner: owner})
}

func (s *Server) handleLockAcquire(w http.ResponseWriter, r *http.Request) {
	device, _ := r.Context().Value(contextKeyDevice).(string)

	var req LockAcquireRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Project == "" {
		writeError(w, http.StatusBadRequest, "missing field: project")
		return
	}
	if req.Session == "" {
		writeError(w, http.StatusBadRequest, "missing field: session")
		return
	}
	if req.Path == "" {
		writeError(w, http.StatusBadRequest, "missing field: path")
		return
	}

	owner := device + ":" + req.Session
	member := req.Project + "|" + req.Path
	now := time.Now().Unix()
	leaseExpiresAt := now + int64(lockLeaseTTL.Seconds())

	res, err := s.rdb.Eval(r.Context(), lockAcquireScript,
		[]string{lockKey(req.Project, req.Path), locksSetKey(device, req.Session)},
		owner, now, leaseExpiresAt, member, req.TaskID,
	).Result()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	arr, ok := res.([]any)
	if !ok || len(arr) < 2 {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	code, _ := arr[0].(int64)
	val, _ := arr[1].(string)
	if code == 0 {
		writeLockConflict(w, "file is locked by another session", val)
		return
	}

	writeJSON(w, http.StatusOK, LockAcquireResponse{Owner: val})
}

func (s *Server) handleLockRelease(w http.ResponseWriter, r *http.Request) {
	device, _ := r.Context().Value(contextKeyDevice).(string)

	var req LockReleaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Project == "" {
		writeError(w, http.StatusBadRequest, "missing field: project")
		return
	}
	if req.Session == "" {
		writeError(w, http.StatusBadRequest, "missing field: session")
		return
	}
	if req.Path == "" {
		writeError(w, http.StatusBadRequest, "missing field: path")
		return
	}

	owner := device + ":" + req.Session
	member := req.Project + "|" + req.Path
	force := "0"
	if req.Force {
		force = "1"
	}

	res, err := s.rdb.Eval(r.Context(), lockReleaseScript,
		[]string{lockKey(req.Project, req.Path), locksSetKey(device, req.Session)},
		owner, member, force,
	).Result()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	arr, ok := res.([]any)
	if !ok || len(arr) < 2 {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	code, _ := arr[0].(int64)
	val, _ := arr[1].(string)
	if code == 0 {
		writeLockConflict(w, "file is locked by another session", val)
		return
	}

	writeJSON(w, http.StatusOK, LockReleaseResponse{Ok: true})
}

func (s *Server) handleLockList(w http.ResponseWriter, r *http.Request) {
	project := r.URL.Query().Get("project")
	if project == "" {
		writeError(w, http.StatusBadRequest, "missing project parameter")
		return
	}

	prefix := "agentlink:lock:" + project + ":"
	keys, err := s.rdb.Keys(r.Context(), prefix+"*").Result()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	locks := make([]LockInfo, 0, len(keys))
	for _, key := range keys {
		data, err := s.rdb.HGetAll(r.Context(), key).Result()
		if err != nil || len(data) == 0 {
			continue
		}
		acquiredAt, _ := strconv.ParseInt(data["acquired_at"], 10, 64)
		leaseExpiresAt, _ := strconv.ParseInt(data["lease_expires_at"], 10, 64)
		locks = append(locks, LockInfo{
			Path:           strings.TrimPrefix(key, prefix),
			Owner:          data["owner"],
			AcquiredAt:     acquiredAt,
			LeaseExpiresAt: leaseExpiresAt,
		})
	}

	writeJSON(w, http.StatusOK, LockListResponse{Locks: locks})
}
