package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

var deviceNameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,31}$`)

// TaskTTL is the Redis TTL for task records and inbox lists. Lua scripts
// below hardcode 604800 (7 days in seconds) to match — keep them in sync.
const TaskTTL = 7 * 24 * time.Hour

// Lua script reject reasons — returned in the second element of the result
// array. Shared between Go (switch) and Lua (return strings).
const (
	rejectDup       = "dup"
	rejectBusy      = "busy"
	rejectSuspended = "suspended"
)

// Message types — shared between server, net layer, and poller.
const (
	MsgTypeMsg  = "msg"
	MsgTypeTask = "task"
)

// luaSuspendInProgress iterates a tasks tracking set and suspends any
// in_progress task. Used by interrupt scripts to clear the prior task before
// pushing the interrupt message.
const luaSuspendInProgress = `
for _, tid in ipairs(redis.call('SMEMBERS', %s)) do
  local st = redis.call('HGET', 'agentlink:task:' .. tid, 'status')
  if st == 'in_progress' then
    redis.call('HSET', 'agentlink:task:' .. tid,
      'status', 'suspended',
      'suspended_at', %s)
  end
end
`

// luaWriteTaskRecord writes the task Hash + tracking set + issued index +
// inbox push, common tail of taskSendScript and interruptTaskSendScript.
//
// KEYS[4] = agentlink:issued:<from_device>:<from_session> (issued reverse index)
const luaWriteTaskRecord = `
redis.call('HSET', KEYS[1],
  'task_id', ARGV[1],
  'status', 'issued',
  'assigned_to', ARGV[3],
  'issued_by', ARGV[4],
  'content', ARGV[5],
  'title', ARGV[6],
  'issued_at', ARGV[2])
redis.call('EXPIRE', KEYS[1], 604800)
redis.call('SADD', KEYS[2], ARGV[1])
redis.call('SADD', KEYS[4], ARGV[1])
redis.call('EXPIRE', KEYS[4], 604800)
redis.call('LPUSH', KEYS[3], ARGV[7])
redis.call('EXPIRE', KEYS[3], 604800)
return {1, ''}
`

// taskSendScript atomically: checks task_id uniqueness, checks no issued or
// in_progress task exists on the target session (busy), checks suspended
// count < 2, then creates the task record, adds to tracking set, and pushes
// to inbox. Returns {1, ""} on success or {0, reason} on rejection.
//
// KEYS[1] = agentlink:task:<task_id>
// KEYS[2] = agentlink:tasks:<device>:<session>
// KEYS[3] = agentlink:inbox:<device>:<session>
// KEYS[4] = agentlink:issued:<from_device>:<from_session>
// ARGV[1] = task_id
// ARGV[2] = now (RFC3339)
// ARGV[3] = assigned_to
// ARGV[4] = issued_by
// ARGV[5] = content
// ARGV[6] = title
// ARGV[7] = inbox_item_json
const taskSendScript = `
if redis.call('EXISTS', KEYS[1]) > 0 then
  return {0, 'dup'}
end
local members = redis.call('SMEMBERS', KEYS[2])
local suspended = 0
for _, tid in ipairs(members) do
  local st = redis.call('HGET', 'agentlink:task:' .. tid, 'status')
  if st == 'issued' or st == 'in_progress' then
    return {0, 'busy'}
  end
  if st == 'suspended' then
    suspended = suspended + 1
  end
end
if suspended >= 2 then
  return {0, 'suspended'}
end
` + luaWriteTaskRecord

// interruptSendScript atomically suspends any in_progress task on the target
// session and pushes the interrupt message to inbox. Called when a msg or
// task is sent with Interrupt=true. Suspending the prior task prevents it
// from being stuck in_progress forever after the poller Ctrl+C's the agent.
//
// KEYS[1] = agentlink:tasks:<device>:<session>
// KEYS[2] = agentlink:inbox:<device>:<session>
// ARGV[1] = now (RFC3339)
// ARGV[2] = inbox_item_json
var interruptSendScript = fmt.Sprintf(`
%s
redis.call('LPUSH', KEYS[2], ARGV[2])
redis.call('EXPIRE', KEYS[2], 604800)
return 1
`, fmt.Sprintf(luaSuspendInProgress, "KEYS[1]", "ARGV[1]"))

// interruptTaskSendScript handles a task sent with Interrupt=true. Unlike
// taskSendScript, it skips the busy check (the whole point is to interrupt
// a busy agent). It suspends any in_progress task, checks task_id uniqueness,
// then writes the new task record and pushes to inbox.
//
// KEYS[1] = agentlink:task:<new_task_id>
// KEYS[2] = agentlink:tasks:<device>:<session>
// KEYS[3] = agentlink:inbox:<device>:<session>
// KEYS[4] = agentlink:issued:<from_device>:<from_session>
// ARGV[1] = task_id
// ARGV[2] = now (RFC3339)
// ARGV[3] = assigned_to
// ARGV[4] = issued_by
// ARGV[5] = content
// ARGV[6] = title
// ARGV[7] = inbox_item_json
var interruptTaskSendScript = fmt.Sprintf(`
if redis.call('EXISTS', KEYS[1]) > 0 then
  return {0, 'dup'}
end
%s
`+luaWriteTaskRecord, fmt.Sprintf(luaSuspendInProgress, "KEYS[2]", "ARGV[2]"))

type HealthResponse struct {
	Ok    bool   `json:"ok"`
	Redis string `json:"redis"`
}

type RegisterRequest struct {
	Device           string   `json:"device"`
	Sessions         []string `json:"sessions"`
	RegisterPassword string   `json:"register_password"`
	Force            bool     `json:"force,omitempty"`
}

type RegisterResponse struct {
	APIKey       string   `json:"api_key"`
	Device       string   `json:"device"`
	Sessions     []string `json:"sessions"`
	RegisteredAt string   `json:"registered_at"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	redisStatus := "connected"
	if err := s.rdb.Ping(r.Context()).Err(); err != nil {
		redisStatus = "disconnected"
	}

	ok := redisStatus == "connected"
	status := http.StatusOK
	if !ok {
		status = http.StatusServiceUnavailable
	}

	writeJSON(w, status, HealthResponse{
		Ok:    ok,
		Redis: redisStatus,
	})
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req RegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Validate register password
	if req.RegisterPassword != s.registerPassword {
		writeError(w, http.StatusUnauthorized, "invalid register password")
		return
	}

	// Validate device name
	if !deviceNameRE.MatchString(req.Device) {
		writeError(w, http.StatusBadRequest, "invalid device name: 2-32 chars, lowercase letters/digits/hyphens/underscores, start with a letter")
		return
	}

	// Validate sessions
	if len(req.Sessions) == 0 {
		writeError(w, http.StatusBadRequest, "sessions must not be empty")
		return
	}
	for _, s := range req.Sessions {
		if !deviceNameRE.MatchString(s) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid session name %q: 2-32 chars, lowercase letters/digits/hyphens/underscores, start with a letter", s))
			return
		}
	}

	// Check device already exists
	exists, err := s.rdb.Exists(r.Context(), "agentlink:device:"+req.Device).Result()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if exists > 0 {
		if req.RegisterPassword != s.registerPassword {
			writeError(w, http.StatusUnauthorized, "invalid register password"+"; use --force to override")
			return
		}
		if req.Force {
			s.deleteDeviceData(r.Context(), req.Device)
			// Fall through to normal registration
		} else {
			// Reuse: generate new API key, update device record
			apiKey, apiKeyHash, err := generateAPIKey()
			if err != nil {
				writeError(w, http.StatusInternalServerError, "internal error")
				return
			}
			deviceKey := "agentlink:device:" + req.Device
			oldHash, _ := s.rdb.HGet(r.Context(), deviceKey, "api_key_hash").Result()
			if oldHash != "" {
				s.rdb.Del(r.Context(), "agentlink:api_key:"+oldHash)
			}
			now := time.Now().UTC().Format(time.RFC3339)
			s.rdb.HSet(r.Context(), deviceKey, "api_key_hash", apiKeyHash, "registered_at", now, "last_seen", now)
			s.rdb.Set(r.Context(), "agentlink:api_key:"+apiKeyHash, req.Device, 0)
			writeJSON(w, http.StatusOK, RegisterResponse{
				APIKey:       apiKey,
				Device:       req.Device,
				Sessions:     req.Sessions,
				RegisteredAt: now,
			})
			return
		}
	}

	// Generate API key
	apiKey, apiKeyHash, err := generateAPIKey()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	now := time.Now().UTC().Format(time.RFC3339)

	// Store device data in a Hash
	deviceKey := "agentlink:device:" + req.Device
	sessionsJSON, _ := json.Marshal(req.Sessions)
	if err := s.rdb.HSet(r.Context(), deviceKey,
		"sessions", string(sessionsJSON),
		"api_key_hash", apiKeyHash,
		"registered_at", now,
		"last_seen", now,
	).Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Reverse index for auth lookup
	indexKey := "agentlink:api_key:" + apiKeyHash
	if err := s.rdb.Set(r.Context(), indexKey, req.Device, 0).Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	writeJSON(w, http.StatusOK, RegisterResponse{
		APIKey:       apiKey,
		Device:       req.Device,
		Sessions:     req.Sessions,
		RegisteredAt: now,
	})
}

type Message struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	FromDevice  string `json:"from_device"`
	FromSession string `json:"from_session"`
	TaskID      string `json:"task_id,omitempty"`
	Title       string `json:"title,omitempty"`
	Interrupt   bool   `json:"interrupt,omitempty"`
	Content     string `json:"content"`
	CreatedAt   string `json:"created_at"`
}

type SendRequest struct {
	To          string `json:"to"`
	FromSession string `json:"from_session"`
	Interrupt   bool   `json:"interrupt,omitempty"`
	Title       string `json:"title,omitempty"`
	Content     string `json:"content"`
}

type SendResponse struct {
	ID              string           `json:"id"`
	TaskID          string           `json:"task_id,omitempty"`
	RecipientStatus *RecipientStatus `json:"recipient_status,omitempty"`
}

type RecipientStatus struct {
	Device  string `json:"device"`
	Session string `json:"session"`
	Current string `json:"current"` // "idle" / "msg: <title> (<dur>)" / "task: <task_id> <title> (<dur>)" / "offline (<dur>)"
}

type PullResponse struct {
	Items []Message `json:"items"`
}

type SendTaskRequest struct {
	To          string `json:"to"`
	FromSession string `json:"from_session"`
	TaskID      string `json:"task_id,omitempty"`
	Title       string `json:"title,omitempty"`
	Interrupt   bool   `json:"interrupt,omitempty"`
	Content     string `json:"content"`
}

type TaskResultRequest struct {
	TaskID string `json:"task_id"`
	Status string `json:"status"`
	Result string `json:"result"`
}

type TaskResumeRequest struct {
	TaskID  string `json:"task_id"`
	Content string `json:"content"`
}

type TaskCancelRequest struct {
	TaskID string `json:"task_id"`
}

type TaskReopenRequest struct {
	TaskID string `json:"task_id"`
	Reason string `json:"reason"`
}

type TaskStatusResponse struct {
	TaskID      string `json:"task_id,omitempty"`
	Status      string `json:"status"`
	AssignedTo  string `json:"assigned_to"`
	IssuedBy    string `json:"issued_by"`
	Content     string `json:"content"`
	Result      string `json:"result,omitempty"`
	IssuedAt    string `json:"issued_at"`
	CompletedAt string `json:"completed_at,omitempty"`
}

type SessionInfo struct {
	Name    string `json:"name"`
	Current string `json:"current"` // same format as RecipientStatus.Current
}

type AgentInfo struct {
	Device        string        `json:"device"`
	Sessions      []string      `json:"sessions"`
	SessionStatus []SessionInfo `json:"session_status"`
	LastSeen      string        `json:"last_seen"`
	Online        bool          `json:"online"`
}

type ListResponse struct {
	Agents []AgentInfo `json:"agents"`
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	device, _ := r.Context().Value(contextKeyDevice).(string)

	now := time.Now().UTC().Format(time.RFC3339)
	if err := s.rdb.HSet(r.Context(), "agentlink:device:"+device, "last_seen", now).Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	device, _ := r.Context().Value(contextKeyDevice).(string)
	all := r.URL.Query().Get("all") == "true"

	var agents []AgentInfo

	if all {
		keys, err := s.rdb.Keys(r.Context(), "agentlink:device:*").Result()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		for _, key := range keys {
			name := strings.TrimPrefix(key, "agentlink:device:")
			agents = append(agents, s.getAgentInfo(r.Context(), name))
		}
	} else {
		agents = append(agents, s.getAgentInfo(r.Context(), device))
	}

	if agents == nil {
		agents = []AgentInfo{}
	}

	writeJSON(w, http.StatusOK, ListResponse{Agents: agents})
}

func (s *Server) getAgentInfo(ctx context.Context, device string) AgentInfo {
	data, err := s.rdb.HGetAll(ctx, "agentlink:device:"+device).Result()
	if err != nil || len(data) == 0 {
		return AgentInfo{Device: device, Online: false}
	}

	var sessions []string
	json.Unmarshal([]byte(data["sessions"]), &sessions)

	lastSeen := data["last_seen"]
	online := false
	if lastSeen != "" {
		t, err := time.Parse(time.RFC3339, lastSeen)
		if err == nil {
			online = time.Since(t) < 120*time.Second
		}
	}

	return AgentInfo{
		Device:        device,
		Sessions:      sessions,
		SessionStatus: s.buildSessionStatuses(ctx, device, sessions),
		LastSeen:      lastSeen,
		Online:        online,
	}
}

// buildSessionStatuses returns per-session current status for list responses.
func (s *Server) buildSessionStatuses(ctx context.Context, device string, sessions []string) []SessionInfo {
	result := make([]SessionInfo, 0, len(sessions))
	for _, sess := range sessions {
		rs := s.buildRecipientStatus(ctx, device, sess)
		result = append(result, SessionInfo{Name: sess, Current: rs.Current})
	}
	return result
}

func (s *Server) buildRecipientStatus(ctx context.Context, device, session string) RecipientStatus {
	rs := RecipientStatus{Device: device, Session: session}

	deviceKey := "agentlink:device:" + device
	exists, err := s.rdb.Exists(ctx, deviceKey).Result()
	if err != nil || exists == 0 {
		rs.Current = "offline"
		return rs
	}

	lastSeen, _ := s.rdb.HGet(ctx, deviceKey, "last_seen").Result()
	online := false
	var sinceLast time.Duration
	if lastSeen != "" {
		t, err := time.Parse(time.RFC3339, lastSeen)
		if err == nil {
			sinceLast = time.Since(t)
			online = sinceLast < 120*time.Second
		}
	}

	// Check for in_progress task first — task takes priority over msg.
	trackingKey := "agentlink:tasks:" + device + ":" + session
	members, _ := s.rdb.SMembers(ctx, trackingKey).Result()
	for _, tid := range members {
		status, _ := s.rdb.HGet(ctx, "agentlink:task:"+tid, "status").Result()
		if status == "in_progress" {
			title, _ := s.rdb.HGet(ctx, "agentlink:task:"+tid, "title").Result()
			if title == "" {
				title = tid
			}
			issuedAt, _ := s.rdb.HGet(ctx, "agentlink:task:"+tid, "issued_at").Result()
			dur := ""
			if t, err := time.Parse(time.RFC3339, issuedAt); err == nil {
				dur = formatDuration(time.Since(t))
			}
			rs.Current = fmt.Sprintf("task: %s %s (%s)", tid, title, dur)
			return rs
		}
	}

	// Check current_msg — agent is processing a msg.
	currentMsgKey := "agentlink:current_msg:" + device + ":" + session
	cmExists, _ := s.rdb.Exists(ctx, currentMsgKey).Result()
	if cmExists > 0 {
		title, _ := s.rdb.HGet(ctx, currentMsgKey, "title").Result()
		startedAt, _ := s.rdb.HGet(ctx, currentMsgKey, "started_at").Result()
		dur := ""
		if t, err := time.Parse(time.RFC3339, startedAt); err == nil {
			dur = formatDuration(time.Since(t))
		}
		rs.Current = fmt.Sprintf("msg: %s (%s)", title, dur)
		return rs
	}

	if !online {
		rs.Current = "offline (" + formatDuration(sinceLast) + ")"
		return rs
	}
	rs.Current = "idle"
	return rs
}

// formatDuration renders a duration as a human-readable short string:
// <60s → "Xs", <60m → "Xm", else → "Xh".
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh", int(d.Hours()))
}

type PatchSessionsRequest struct {
	Sessions []string `json:"sessions"`
}

func (s *Server) handlePatchSessions(w http.ResponseWriter, r *http.Request) {
	device, _ := r.Context().Value(contextKeyDevice).(string)

	var req PatchSessionsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if len(req.Sessions) == 0 {
		writeError(w, http.StatusBadRequest, "sessions must not be empty")
		return
	}

	for _, session := range req.Sessions {
		if !deviceNameRE.MatchString(session) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid session name %q", session))
			return
		}
	}

	sessionsJSON, _ := json.Marshal(req.Sessions)
	if err := s.rdb.HSet(r.Context(), "agentlink:device:"+device, "sessions", string(sessionsJSON)).Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"sessions": req.Sessions})
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	device, _ := r.Context().Value(contextKeyDevice).(string)

	session := r.URL.Query().Get("name")
	if session == "" {
		writeError(w, http.StatusBadRequest, "missing name parameter")
		return
	}
	if !deviceNameRE.MatchString(session) {
		writeError(w, http.StatusBadRequest, "invalid session name")
		return
	}

	deviceKey := "agentlink:device:" + device
	sessionsJSON, err := s.rdb.HGet(r.Context(), deviceKey, "sessions").Result()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	var sessions []string
	json.Unmarshal([]byte(sessionsJSON), &sessions)

	found := false
	for i, s := range sessions {
		if s == session {
			sessions = append(sessions[:i], sessions[i+1:]...)
			found = true
			break
		}
	}
	if !found {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}

	if len(sessions) == 0 {
		writeError(w, http.StatusBadRequest, "cannot remove the last session")
		return
	}

	updated, _ := json.Marshal(sessions)
	if err := s.rdb.HSet(r.Context(), deviceKey, "sessions", string(updated)).Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Clean up inbox
	s.rdb.Del(r.Context(), "agentlink:inbox:"+device+":"+session)

	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

func (s *Server) handleDeleteDevice(w http.ResponseWriter, r *http.Request) {
	device, _ := r.Context().Value(contextKeyDevice).(string)
	s.deleteDeviceData(r.Context(), device)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) deleteDeviceData(ctx context.Context, device string) {
	apiKeyHash, _ := s.rdb.HGet(ctx, "agentlink:device:"+device, "api_key_hash").Result()

	s.rdb.Del(ctx, "agentlink:device:"+device)
	if apiKeyHash != "" {
		s.rdb.Del(ctx, "agentlink:api_key:"+apiKeyHash)
	}

	inboxKeys, _ := s.rdb.Keys(ctx, "agentlink:inbox:"+device+":*").Result()
	for _, k := range inboxKeys {
		s.rdb.Del(ctx, k)
	}

	trackingKeys, _ := s.rdb.Keys(ctx, "agentlink:tasks:"+device+":*").Result()
	for _, tk := range trackingKeys {
		members, _ := s.rdb.SMembers(ctx, tk).Result()
		for _, tid := range members {
			s.rdb.Del(ctx, "agentlink:task:"+tid)
		}
		s.rdb.Del(ctx, tk)
	}
}

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	fromDevice, _ := r.Context().Value(contextKeyDevice).(string)

	var req SendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.To == "" {
		writeError(w, http.StatusBadRequest, "missing field: to")
		return
	}
	if req.FromSession == "" {
		writeError(w, http.StatusBadRequest, "missing field: from_session")
		return
	}
	if req.Content == "" {
		writeError(w, http.StatusBadRequest, "missing field: content")
		return
	}
	if len(req.Content) > 3000 {
		writeError(w, http.StatusBadRequest, "content exceeds 3000 characters")
		return
	}
	if !deviceNameRE.MatchString(req.FromSession) {
		writeError(w, http.StatusBadRequest, "invalid from_session name")
		return
	}

	parts := strings.SplitN(req.To, ":", 2)
	if len(parts) != 2 {
		writeError(w, http.StatusBadRequest, "invalid target format, expected device:session")
		return
	}
	targetDevice, targetSession := parts[0], parts[1]
	if !deviceNameRE.MatchString(targetDevice) {
		writeError(w, http.StatusBadRequest, "invalid target device name")
		return
	}
	if !deviceNameRE.MatchString(targetSession) {
		writeError(w, http.StatusBadRequest, "invalid target session name")
		return
	}

	exists, err := s.rdb.Exists(r.Context(), "agentlink:device:"+targetDevice).Result()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if exists == 0 {
		writeError(w, http.StatusNotFound, "target device not found")
		return
	}

	sessionsJSON, err := s.rdb.HGet(r.Context(), "agentlink:device:"+targetDevice, "sessions").Result()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	var sessions []string
	json.Unmarshal([]byte(sessionsJSON), &sessions)
	if !containsStr(sessions, targetSession) {
		writeError(w, http.StatusNotFound, "target session not found on device")
		return
	}

	id := generateID()
	now := time.Now().UTC().Format(time.RFC3339)
	title := msgTitle(req.Title, req.Content)
	msg := Message{
		ID:          id,
		Type:        MsgTypeMsg,
		FromDevice:  fromDevice,
		FromSession: req.FromSession,
		Title:       title,
		Interrupt:   req.Interrupt,
		Content:     req.Content,
		CreatedAt:   now,
	}
	msgJSON, _ := json.Marshal(msg)

	inboxKey := "agentlink:inbox:" + targetDevice + ":" + targetSession
	trackingKey := "agentlink:tasks:" + targetDevice + ":" + targetSession
	if req.Interrupt {
		if err := s.rdb.Eval(r.Context(), interruptSendScript, []string{trackingKey, inboxKey},
			now, string(msgJSON),
		).Err(); err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
	} else {
		if err := s.rdb.LPush(r.Context(), inboxKey, msgJSON).Err(); err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		s.rdb.Expire(r.Context(), inboxKey, TaskTTL)
	}

	status := s.buildRecipientStatus(r.Context(), targetDevice, targetSession)
	writeJSON(w, http.StatusOK, SendResponse{ID: id, RecipientStatus: &status})
}

func (s *Server) handlePull(w http.ResponseWriter, r *http.Request) {
	device, _ := r.Context().Value(contextKeyDevice).(string)

	session := r.URL.Query().Get("session")
	if session == "" {
		writeError(w, http.StatusBadRequest, "missing session parameter")
		return
	}
	if !deviceNameRE.MatchString(session) {
		writeError(w, http.StatusBadRequest, "invalid session name")
		return
	}

	limit := 1
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		n, err := strconv.Atoi(limitStr)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		if n < 0 {
			writeError(w, http.StatusBadRequest, "limit must not be negative")
			return
		}
		if n > 0 {
			limit = n
		}
		if limit > 100 {
			limit = 100
		}
	}

	now := time.Now().UTC().Format(time.RFC3339)
	inboxKey := "agentlink:inbox:" + device + ":" + session
	currentMsgKey := "agentlink:current_msg:" + device + ":" + session
	items := make([]Message, 0)

	// Clear prior current_msg: a new pull means the agent moved on (either
	// processed the previous msg or we're about to hand it something new).
	s.rdb.Del(r.Context(), currentMsgKey)

	pulled := 0
	for pulled < limit {
		data, err := s.rdb.RPop(r.Context(), inboxKey).Result()
		if err == redis.Nil {
			break
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		var msg Message
		json.Unmarshal([]byte(data), &msg)

		// Skip tasks that are no longer issued (cancelled/suspended/completed).
		// The inbox item is stale — the task record is the source of truth.
		// Skip without consuming limit so the agent still gets `limit` usable
		// items.
		if msg.Type == MsgTypeTask && msg.TaskID != "" {
			taskKey := "agentlink:task:" + msg.TaskID
			currentStatus, _ := s.rdb.HGet(r.Context(), taskKey, "status").Result()
			if currentStatus != "issued" {
				continue
			}
			s.rdb.HSet(r.Context(), taskKey, "status", "in_progress")
		}

		// Track current_msg for the first msg pulled this round, so
		// buildRecipientStatus can report "msg: <title> (<dur>)" while the
		// agent processes it. Task pulls don't set this (tasks have their
		// own in_progress state).
		if msg.Type == MsgTypeMsg && pulled == 0 {
			title := msgTitle(msg.Title, msg.Content)
			s.rdb.HSet(r.Context(), currentMsgKey,
				"title", title,
				"started_at", now,
			)
			s.rdb.Expire(r.Context(), currentMsgKey, 10*time.Minute)
		}

		items = append(items, msg)
		pulled++
	}

	writeJSON(w, http.StatusOK, PullResponse{Items: items})
}

func (s *Server) handleSendTask(w http.ResponseWriter, r *http.Request) {
	fromDevice, _ := r.Context().Value(contextKeyDevice).(string)

	var req SendTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.To == "" {
		writeError(w, http.StatusBadRequest, "missing field: to")
		return
	}
	if req.FromSession == "" {
		writeError(w, http.StatusBadRequest, "missing field: from_session")
		return
	}
	if req.Content == "" {
		writeError(w, http.StatusBadRequest, "missing field: content")
		return
	}
	if len(req.Content) > 3000 {
		writeError(w, http.StatusBadRequest, "content exceeds 3000 characters")
		return
	}
	if req.TaskID == "" {
		req.TaskID = generateID()[:8]
	} else if !deviceNameRE.MatchString(req.TaskID) {
		writeError(w, http.StatusBadRequest, "invalid task_id: 2-32 chars, lowercase letters/digits/hyphens/underscores, start with a letter")
		return
	}
	if !deviceNameRE.MatchString(req.FromSession) {
		writeError(w, http.StatusBadRequest, "invalid from_session name")
		return
	}

	parts := strings.SplitN(req.To, ":", 2)
	if len(parts) != 2 {
		writeError(w, http.StatusBadRequest, "invalid target format, expected device:session")
		return
	}
	targetDevice, targetSession := parts[0], parts[1]
	if !deviceNameRE.MatchString(targetDevice) {
		writeError(w, http.StatusBadRequest, "invalid target device name")
		return
	}
	if !deviceNameRE.MatchString(targetSession) {
		writeError(w, http.StatusBadRequest, "invalid target session name")
		return
	}

	// Check target device exists
	exists, err := s.rdb.Exists(r.Context(), "agentlink:device:"+targetDevice).Result()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if exists == 0 {
		writeError(w, http.StatusNotFound, "target device not found")
		return
	}

	// Check target session exists
	sessionsJSON, err := s.rdb.HGet(r.Context(), "agentlink:device:"+targetDevice, "sessions").Result()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	var sessions []string
	json.Unmarshal([]byte(sessionsJSON), &sessions)
	if !containsStr(sessions, targetSession) {
		writeError(w, http.StatusNotFound, "target session not found on device")
		return
	}

	// Check task_id uniqueness
	taskKey := "agentlink:task:" + req.TaskID
	trackingKey := "agentlink:tasks:" + targetDevice + ":" + targetSession
	inboxKey := "agentlink:inbox:" + targetDevice + ":" + targetSession
	issuedKey := "agentlink:issued:" + fromDevice + ":" + req.FromSession

	now := time.Now().UTC().Format(time.RFC3339)
	title := req.Title
	if title == "" {
		title = req.TaskID
	}
	id := generateID()
	inboxItem := Message{
		ID:          id,
		Type:        MsgTypeTask,
		FromDevice:  fromDevice,
		FromSession: req.FromSession,
		TaskID:      req.TaskID,
		Title:       title,
		Interrupt:   req.Interrupt,
		Content:     req.Content,
		CreatedAt:   now,
	}
	msgJSON, _ := json.Marshal(inboxItem)

	// Atomic: task_id uniqueness + busy check (issued OR in_progress) +
	// suspended limit + write task record + SAdd tracking + LPush inbox.
	// Interrupt tasks skip the busy check (they're here to interrupt) and
	// suspend any in_progress task first.
	script := taskSendScript
	if req.Interrupt {
		script = interruptTaskSendScript
	}
	res, err := s.rdb.Eval(r.Context(), script, []string{taskKey, trackingKey, inboxKey, issuedKey},
		req.TaskID, now, req.To, fromDevice+":"+req.FromSession, req.Content, title, string(msgJSON),
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
	reason, _ := arr[1].(string)
	if code == 0 {
		switch reason {
		case rejectDup:
			writeError(w, http.StatusConflict, "task_id already exists")
		case rejectBusy:
			s.writeBusyError(w, r.Context(), targetDevice, targetSession, "target session is busy")
		case rejectSuspended:
			s.writeBusyError(w, r.Context(), targetDevice, targetSession, "target has 2 suspended tasks")
		default:
			writeError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}

	status := s.buildRecipientStatus(r.Context(), targetDevice, targetSession)
	writeJSON(w, http.StatusOK, SendResponse{ID: id, TaskID: req.TaskID, RecipientStatus: &status})
}

func (s *Server) handleTaskResult(w http.ResponseWriter, r *http.Request) {
	var req TaskResultRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.TaskID == "" {
		writeError(w, http.StatusBadRequest, "missing field: task_id")
		return
	}
	if req.Status != "completed" && req.Status != "suspended" {
		writeError(w, http.StatusBadRequest, "status must be completed or suspended")
		return
	}
	if req.Result == "" {
		writeError(w, http.StatusBadRequest, "missing field: result")
		return
	}

	taskKey := "agentlink:task:" + req.TaskID
	status, err := s.rdb.HGet(r.Context(), taskKey, "status").Result()
	if err == redis.Nil {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if status != "in_progress" {
		hint := ""
		switch status {
		case "suspended":
			hint = " (resume it: task resume <id> \"<updated guidance>\")"
		case "issued":
			hint = " (task not pulled yet, wait for it)"
		case "completed":
			hint = " (task already completed; use reopen if needed)"
		case "cancelled":
			hint = " (task was cancelled; use reopen if needed)"
		}
		writeError(w, http.StatusBadRequest, "task is not in_progress"+hint)
		return
	}

	now := time.Now().UTC().Format(time.RFC3339)
	if err := s.rdb.HSet(r.Context(), taskKey,
		"status", req.Status,
		"result", req.Result,
		"completed_at", now,
	).Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	var assignedToDev, assignedToSess string
	assignedTo, err := s.rdb.HGet(r.Context(), taskKey, "assigned_to").Result()
	if err == nil {
		parts := strings.SplitN(assignedTo, ":", 2)
		if len(parts) == 2 {
			s.rdb.SRem(r.Context(), "agentlink:tasks:"+parts[0]+":"+parts[1], req.TaskID)
			assignedToDev, assignedToSess = parts[0], parts[1]
		}
	}


	issuedBy, _ := s.rdb.HGet(r.Context(), taskKey, "issued_by").Result()
	issuedParts := strings.SplitN(issuedBy, ":", 2)
	if len(issuedParts) == 2 {
		s.rdb.SRem(r.Context(), "agentlink:issued:"+issuedParts[0]+":"+issuedParts[1], req.TaskID)

		// Notify issuer (auto-notify, no extra agent action needed)
		if assignedToDev != "" {
			notifyInbox(r.Context(), s.rdb.Client, issuedParts[0], issuedParts[1], Message{
				ID:          generateID(),
				Type:        MsgTypeMsg,
				FromDevice:  assignedToDev,
				FromSession: assignedToSess,
				Title:       "任务回报 " + req.TaskID,
				Content:     req.Status + ": " + req.Result,
				CreatedAt:   now,
			})
		}
	}

	s.rdb.Expire(r.Context(), taskKey, 30*24*time.Hour)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleTaskResume(w http.ResponseWriter, r *http.Request) {
	var req TaskResumeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.TaskID == "" {
		writeError(w, http.StatusBadRequest, "missing field: task_id")
		return
	}
	if req.Content == "" {
		writeError(w, http.StatusBadRequest, "missing field: content")
		return
	}
	if len(req.Content) > 3000 {
		writeError(w, http.StatusBadRequest, "content exceeds 3000 characters")
		return
	}

	taskKey := "agentlink:task:" + req.TaskID
	status, err := s.rdb.HGet(r.Context(), taskKey, "status").Result()
	if err == redis.Nil {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if status != "suspended" {
		writeError(w, http.StatusBadRequest, "task is not suspended")
		return
	}

	// Get task info before modifying
	taskData, err := s.rdb.HGetAll(r.Context(), taskKey).Result()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	now := time.Now().UTC().Format(time.RFC3339)
	if err := s.rdb.HSet(r.Context(), taskKey,
		"status", "issued",
		"content", req.Content,
		"issued_at", now, // Update issued_at to reflect new guidance
		"result", "",
		"completed_at", "",
	).Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	assignedTo := taskData["assigned_to"]
	parts := strings.SplitN(assignedTo, ":", 2)
	issuedParts := strings.SplitN(taskData["issued_by"], ":", 2)

	if len(parts) == 2 {
		s.rdb.SAdd(r.Context(), "agentlink:tasks:"+parts[0]+":"+parts[1], req.TaskID)
	}
	if len(issuedParts) == 2 {
		s.rdb.SAdd(r.Context(), "agentlink:issued:"+issuedParts[0]+":"+issuedParts[1], req.TaskID)
		s.rdb.Expire(r.Context(), "agentlink:issued:"+issuedParts[0]+":"+issuedParts[1], TaskTTL)
	}

	id := generateID()
	inboxItem := Message{
		ID:          id,
		Type:        MsgTypeTask,
		FromDevice:  "",
		FromSession: "",
		TaskID:      req.TaskID,
		Content:     req.Content,
		CreatedAt:   now,
	}
	if len(issuedParts) == 2 {
		inboxItem.FromDevice = issuedParts[0]
		inboxItem.FromSession = issuedParts[1]
	}

	msgJSON, _ := json.Marshal(inboxItem)
	if len(parts) == 2 {
		inboxKey := "agentlink:inbox:" + parts[0] + ":" + parts[1]
		s.rdb.LPush(r.Context(), inboxKey, msgJSON)
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleTaskCancel(w http.ResponseWriter, r *http.Request) {
	var req TaskCancelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.TaskID == "" {
		writeError(w, http.StatusBadRequest, "missing field: task_id")
		return
	}

	taskKey := "agentlink:task:" + req.TaskID
	status, err := s.rdb.HGet(r.Context(), taskKey, "status").Result()
	if err == redis.Nil {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if status == "completed" || status == "cancelled" {
		writeError(w, http.StatusBadRequest, "task already "+status)
		return
	}

	now := time.Now().UTC().Format(time.RFC3339)
	if err := s.rdb.HSet(r.Context(), taskKey,
		"status", "cancelled",
		"completed_at", now,
	).Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	var assignedToDev, assignedToSess string
	assignedTo, err := s.rdb.HGet(r.Context(), taskKey, "assigned_to").Result()
	if err == nil {
		parts := strings.SplitN(assignedTo, ":", 2)
		if len(parts) == 2 {
			s.rdb.SRem(r.Context(), "agentlink:tasks:"+parts[0]+":"+parts[1], req.TaskID)
			assignedToDev, assignedToSess = parts[0], parts[1]
		}
	}


	issuedBy, _ := s.rdb.HGet(r.Context(), taskKey, "issued_by").Result()
	issuedParts := strings.SplitN(issuedBy, ":", 2)
	if len(issuedParts) == 2 {
		s.rdb.SRem(r.Context(), "agentlink:issued:"+issuedParts[0]+":"+issuedParts[1], req.TaskID)
	}

	// Notify target only if the task had been pulled (in_progress) or
	// previously suspended — the recipient has context to understand the
	// cancel. For issued tasks (still queued, never pulled), skip the
	// notification: pull-side filtering drops the stale inbox item, so the
	// recipient never sees the task at all.
	if len(issuedParts) == 2 && assignedToDev != "" && (status == "in_progress" || status == "suspended") {
		notifyInbox(r.Context(), s.rdb.Client, assignedToDev, assignedToSess, Message{
			ID:          generateID(),
			Type:        MsgTypeMsg,
			FromDevice:  issuedParts[0],
			FromSession: issuedParts[1],
			Title:       "任务回报 " + req.TaskID,
			Content:     "cancelled",
			CreatedAt:   now,
		})
	}

	s.rdb.Expire(r.Context(), taskKey, 30*24*time.Hour)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleTaskReopen resets a completed/cancelled task back to issued and
// re-pushes it to the target inbox. The reason is injected into the inbox
// item content so the recipient understands why the task reappeared.
func (s *Server) handleTaskReopen(w http.ResponseWriter, r *http.Request) {
	var req TaskReopenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.TaskID == "" {
		writeError(w, http.StatusBadRequest, "missing field: task_id")
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "missing field: reason")
		return
	}
	if len(req.Reason) > 1000 {
		writeError(w, http.StatusBadRequest, "reason exceeds 1000 characters")
		return
	}

	taskKey := "agentlink:task:" + req.TaskID
	status, err := s.rdb.HGet(r.Context(), taskKey, "status").Result()
	if err == redis.Nil {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if status != "completed" && status != "cancelled" {
		writeError(w, http.StatusBadRequest, "task is not completed or cancelled (use task resume for suspended)")
		return
	}

	taskData, err := s.rdb.HGetAll(r.Context(), taskKey).Result()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	now := time.Now().UTC().Format(time.RFC3339)
	if err := s.rdb.HSet(r.Context(), taskKey,
		"status", "issued",
		"result", "",
		"completed_at", "",
		"reopen_reason", req.Reason,
	).Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	assignedTo := taskData["assigned_to"]
	parts := strings.SplitN(assignedTo, ":", 2)
	issuedParts := strings.SplitN(taskData["issued_by"], ":", 2)

	if len(parts) == 2 {
		s.rdb.SAdd(r.Context(), "agentlink:tasks:"+parts[0]+":"+parts[1], req.TaskID)
	}
	if len(issuedParts) == 2 {
		s.rdb.SAdd(r.Context(), "agentlink:issued:"+issuedParts[0]+":"+issuedParts[1], req.TaskID)
		s.rdb.Expire(r.Context(), "agentlink:issued:"+issuedParts[0]+":"+issuedParts[1], TaskTTL)
	}

	inboxContent := "[重发原因: " + req.Reason + "]\n" + taskData["content"]
	id := generateID()
	inboxItem := Message{
		ID:         id,
		Type:       MsgTypeTask,
		TaskID:     req.TaskID,
		Title:      taskData["title"],
		Content:    inboxContent,
		CreatedAt:  now,
	}
	if len(issuedParts) == 2 {
		inboxItem.FromDevice = issuedParts[0]
		inboxItem.FromSession = issuedParts[1]
	}

	msgJSON, _ := json.Marshal(inboxItem)
	if len(parts) == 2 {
		inboxKey := "agentlink:inbox:" + parts[0] + ":" + parts[1]
		s.rdb.LPush(r.Context(), inboxKey, msgJSON)
		s.rdb.Expire(r.Context(), inboxKey, TaskTTL)
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleTaskStatus(w http.ResponseWriter, r *http.Request) {
	taskID := r.URL.Query().Get("task_id")
	if taskID == "" {
		writeError(w, http.StatusBadRequest, "missing task_id parameter")
		return
	}

	taskKey := "agentlink:task:" + taskID
	data, err := s.rdb.HGetAll(r.Context(), taskKey).Result()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if len(data) == 0 {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}

	resp := TaskStatusResponse{
		TaskID:      data["task_id"],
		Status:      data["status"],
		AssignedTo:  data["assigned_to"],
		IssuedBy:    data["issued_by"],
		Content:     data["content"],
		Result:      data["result"],
		IssuedAt:    data["issued_at"],
		CompletedAt: data["completed_at"],
	}
	writeJSON(w, http.StatusOK, resp)
}

// readTasks reads task records from a Redis set key and returns them as a
// slice of TaskStatusResponse. Errors and missing records are skipped.
func (s *Server) readTasks(ctx context.Context, setKey string) []TaskStatusResponse {
	members, err := s.rdb.SMembers(ctx, setKey).Result()
	if err != nil {
		return nil
	}
	tasks := make([]TaskStatusResponse, 0, len(members))
	for _, tid := range members {
		taskKey := "agentlink:task:" + tid
		data, err := s.rdb.HGetAll(ctx, taskKey).Result()
		if err != nil || len(data) == 0 {
			continue
		}
		tasks = append(tasks, TaskStatusResponse{
			TaskID:      data["task_id"],
			Status:      data["status"],
			AssignedTo:  data["assigned_to"],
			IssuedBy:    data["issued_by"],
			Content:     data["content"],
			Result:      data["result"],
			IssuedAt:    data["issued_at"],
			CompletedAt: data["completed_at"],
		})
	}
	return tasks
}

func (s *Server) handleTaskList(w http.ResponseWriter, r *http.Request) {
	device, _ := r.Context().Value(contextKeyDevice).(string)

	session := r.URL.Query().Get("session")
	if session == "" {
		writeError(w, http.StatusBadRequest, "missing session parameter")
		return
	}
	if !deviceNameRE.MatchString(session) {
		writeError(w, http.StatusBadRequest, "invalid session name")
		return
	}

	receivedKey := "agentlink:tasks:" + device + ":" + session
	received := s.readTasks(r.Context(), receivedKey)

	sentKey := "agentlink:issued:" + device + ":" + session
	sent := s.readTasks(r.Context(), sentKey)

	writeJSON(w, http.StatusOK, map[string]any{"received": received, "sent": sent})
}

func (s *Server) handleWhoami(w http.ResponseWriter, r *http.Request) {
	device, _ := r.Context().Value(contextKeyDevice).(string)

	session := r.URL.Query().Get("session")
	if session == "" {
		writeError(w, http.StatusBadRequest, "missing session parameter")
		return
	}

	status := s.buildRecipientStatus(r.Context(), device, session)

	// Count received (tracking) and sent (issued) tasks
	recvKey := "agentlink:tasks:" + device + ":" + session
	sentKey := "agentlink:issued:" + device + ":" + session
	recvTasks := s.readTasks(r.Context(), recvKey)
	sentTasks := s.readTasks(r.Context(), sentKey)

	writeJSON(w, http.StatusOK, map[string]any{
		"device":  device,
		"session": session,
		"current": status.Current,
		"inbox": map[string]int{
			"received": len(recvTasks),
			"sent":     len(sentTasks),
		},
		"received_tasks": recvTasks,
		"sent_tasks":     sentTasks,
	})
}

func generateID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// truncateTitle returns the first up to 40 runes of s, for default msg titles.
func truncateTitle(s string) string {
	r := []rune(s)
	if len(r) <= 40 {
		return s
	}
	return string(r[:40]) + "..."
}

// msgTitle returns the title when non-empty, otherwise truncates content.
func msgTitle(title, content string) string {
	if title != "" {
		return title
	}
	return truncateTitle(content)
}

func containsStr(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

func generateAPIKey() (raw, sha256hex string, err error) {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		return "", "", e
	}
	raw = "sk_live_" + hex.EncodeToString(b)
	h := sha256.Sum256([]byte(raw))
	return raw, hex.EncodeToString(h[:]), nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// notifyInbox LPUSHes a msg into the target inbox with TaskTTL. Best-effort:
// errors are silently dropped (notification is informational, not transactional).
func notifyInbox(ctx context.Context, rdb *redis.Client, device, session string, msg Message) {
	if device == "" || session == "" {
		return
	}
	msgJSON, err := json.Marshal(msg)
	if err != nil {
		return
	}
	inboxKey := "agentlink:inbox:" + device + ":" + session
	rdb.LPush(ctx, inboxKey, msgJSON)
	rdb.Expire(ctx, inboxKey, TaskTTL)
}

// writeBusyError responds 409 with a recipient_status panel so the caller
// sees the target agent's current state on rejection.
func (s *Server) writeBusyError(w http.ResponseWriter, ctx context.Context, device, session, msg string) {
	status := s.buildRecipientStatus(ctx, device, session)
	writeJSON(w, http.StatusConflict, map[string]any{
		"error":            msg,
		"recipient_status": &status,
	})
}

// skipAuth returns true if the path does not require API key auth.
func skipAuth(path string) bool {
	return path == "/health" || path == "/agents/register" || path == "/ws"
}

// authMiddleware wraps a handler, checking Bearer API key on protected routes.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if skipAuth(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") || len(auth) < 8 {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		apiKey := strings.TrimSpace(auth[7:])
		if apiKey == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		h := sha256.Sum256([]byte(apiKey))
		hashHex := hex.EncodeToString(h[:])
		device, err := s.rdb.Get(r.Context(), "agentlink:api_key:"+hashHex).Result()
		if err == redis.Nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		// Inject device name into context
		ctx := context.WithValue(r.Context(), contextKeyDevice, device)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
