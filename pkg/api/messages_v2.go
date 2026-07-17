package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// Team-scoped messaging and tasks. Every key is prefixed with the team id, and
// the sender identity is always taken from the authenticated actor (never from
// the request body), so a member can neither read another team's inbox nor
// forge a "from" identity. Targets are resolved only from the caller's team
// device/session index.

const (
	taskTTLV2    = 7 * 24 * time.Hour
	maxContentV2 = 3000
	maxReasonV2  = 1000
)

func inboxV2Key(teamID, deviceID, session string) string {
	return "agentlink:v2:inbox:" + teamID + ":" + deviceID + ":" + session
}

func currentMsgV2Key(teamID, deviceID, session string) string {
	return "agentlink:v2:current_msg:" + teamID + ":" + deviceID + ":" + session
}

func taskV2Key(teamID, taskID string) string {
	return "agentlink:v2:task:" + teamID + ":" + taskID
}

// taskV2Prefix is the key prefix Lua scripts concatenate with a task id read
// from a tracking set.
func taskV2Prefix(teamID string) string {
	return "agentlink:v2:task:" + teamID + ":"
}

func receivedTasksV2Key(teamID, deviceID, session string) string {
	return "agentlink:v2:tasks:" + teamID + ":" + deviceID + ":" + session
}

func issuedTasksV2Key(teamID, deviceID, session string) string {
	return "agentlink:v2:issued:" + teamID + ":" + deviceID + ":" + session
}

type MessageV2 struct {
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

type SendRequestV2 struct {
	To        string `json:"to"`
	Title     string `json:"title,omitempty"`
	Interrupt bool   `json:"interrupt,omitempty"`
	Content   string `json:"content"`
}

type RecipientStatusV2 struct {
	DeviceID string `json:"device_id"`
	Session  string `json:"session"`
	Current  string `json:"current"`
}

type SendResponseV2 struct {
	ID              string             `json:"id"`
	TaskID          string             `json:"task_id,omitempty"`
	RecipientStatus *RecipientStatusV2 `json:"recipient_status,omitempty"`
}

type PullResponseV2 struct {
	Items []MessageV2 `json:"items"`
}

// interruptSendV2Script suspends any in_progress task on the target session,
// then pushes an interrupt message to its inbox.
// KEYS[1]=received tracking set, KEYS[2]=inbox
// ARGV[1]=now, ARGV[2]=inbox_json, ARGV[3]=task key prefix
var interruptSendV2Script = goredis.NewScript(`
for _, tid in ipairs(redis.call('SMEMBERS', KEYS[1])) do
  local st = redis.call('HGET', ARGV[3] .. tid, 'status')
  if st == 'in_progress' then
    redis.call('HSET', ARGV[3] .. tid, 'status', 'suspended', 'suspended_at', ARGV[1])
  end
end
redis.call('LPUSH', KEYS[2], ARGV[2])
redis.call('EXPIRE', KEYS[2], 604800)
return 1
`)

// handleSendV2 delivers a plain message to a team member's device/session
// inbox. from identity comes from the actor; the target is validated against
// the team's device/session index.
func (s *Server) handleSendV2(w http.ResponseWriter, r *http.Request) {
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req SendRequestV2
	if !decodeLockJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Content) == "" {
		writeError(w, http.StatusBadRequest, "missing field: content")
		return
	}
	if len(req.Content) > maxContentV2 {
		writeError(w, http.StatusBadRequest, "content exceeds 3000 characters")
		return
	}

	ctx := r.Context()
	teamID := actor.TeamID
	targetUserID, targetDevice, targetSession, code, errMsg := s.resolveTeamTargetV2(ctx, teamID, req.To)
	if code != 0 {
		writeError(w, code, errMsg)
		return
	}

	now := time.Now().UTC().Format(time.RFC3339)
	msg := MessageV2{
		ID:          generateID(),
		Type:        MsgTypeMsg,
		FromDevice:  actor.DeviceID,
		FromSession: actor.SessionName,
		Title:       msgTitle(req.Title, req.Content),
		Interrupt:   req.Interrupt,
		Content:     req.Content,
		CreatedAt:   now,
	}
	msgJSON, err := json.Marshal(msg)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	inboxKey := inboxV2Key(teamID, targetDevice, targetSession)
	if req.Interrupt {
		if err := interruptSendV2Script.Run(ctx, s.rdb,
			[]string{receivedTasksV2Key(teamID, targetDevice, targetSession), inboxKey},
			now, msgJSON, taskV2Prefix(teamID),
		).Err(); err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
	} else {
		if err := s.rdb.LPush(ctx, inboxKey, msgJSON).Err(); err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		s.rdb.Expire(ctx, inboxKey, taskTTLV2)
	}

	status := s.buildRecipientStatusV2(ctx, teamID, targetUserID, targetDevice, targetSession)
	writeJSON(w, http.StatusOK, SendResponseV2{ID: msg.ID, RecipientStatus: &status})
}

// handlePullV2 pops queued messages from the caller's own inbox. The device and
// session are taken from the actor (requireActorSession), so a caller can only
// ever drain its own queue.
func (s *Server) handlePullV2(w http.ResponseWriter, r *http.Request) {
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
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

	ctx := r.Context()
	teamID := actor.TeamID
	now := time.Now().UTC().Format(time.RFC3339)
	inboxKey := inboxV2Key(teamID, actor.DeviceID, actor.SessionName)
	currentMsgKey := currentMsgV2Key(teamID, actor.DeviceID, actor.SessionName)
	items := make([]MessageV2, 0)

	// A fresh pull means the agent moved past any previously-handed message.
	s.rdb.Del(ctx, currentMsgKey)

	pulled := 0
	for pulled < limit {
		data, err := s.rdb.RPop(ctx, inboxKey).Result()
		if err == goredis.Nil {
			break
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		var msg MessageV2
		if err := json.Unmarshal([]byte(data), &msg); err != nil {
			continue
		}

		// Drop stale task items whose task is no longer issued; do not consume
		// the limit so the agent still gets `limit` usable items.
		if msg.Type == MsgTypeTask && msg.TaskID != "" {
			taskKey := taskV2Key(teamID, msg.TaskID)
			currentStatus, _ := s.rdb.HGet(ctx, taskKey, "status").Result()
			if currentStatus != "issued" {
				continue
			}
			s.rdb.HSet(ctx, taskKey, "status", "in_progress")
		}

		if msg.Type == MsgTypeMsg && pulled == 0 {
			s.rdb.HSet(ctx, currentMsgKey,
				"title", msgTitle(msg.Title, msg.Content),
				"started_at", now,
			)
			s.rdb.Expire(ctx, currentMsgKey, 10*time.Minute)
		}

		items = append(items, msg)
		pulled++
	}

	writeJSON(w, http.StatusOK, PullResponseV2{Items: items})
}

// resolveTeamTargetV2 parses a "device:session" target and confirms it belongs
// to the caller's team. It returns the owning user id (needed for presence
// lookups) plus the device/session. A target in another team is indistinguish-
// able from a nonexistent one (both 404), so team membership is never leaked.
func (s *Server) resolveTeamTargetV2(ctx context.Context, teamID, to string) (userID, deviceID, session string, code int, errMsg string) {
	parts := strings.SplitN(to, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", "", http.StatusBadRequest, "invalid target format, expected device:session"
	}
	deviceID, session = parts[0], parts[1]
	if strings.ContainsAny(deviceID, ": \t\r\n/") {
		return "", "", "", http.StatusBadRequest, "invalid target device name"
	}
	if !deviceNameRE.MatchString(session) {
		return "", "", "", http.StatusBadRequest, "invalid target session name"
	}

	members, err := s.rdb.SMembers(ctx, teamDevicesV2Key(teamID)).Result()
	if err != nil {
		return "", "", "", http.StatusInternalServerError, "internal error"
	}
	suffix := ":" + deviceID
	owner := ""
	for _, m := range members {
		if strings.HasSuffix(m, suffix) {
			owner = strings.TrimSuffix(m, suffix)
			break
		}
	}
	if owner == "" {
		return "", "", "", http.StatusNotFound, "target device not found"
	}

	isMember, err := s.rdb.SIsMember(ctx, deviceSessionsV2Key(teamID, owner, deviceID), session).Result()
	if err != nil {
		return "", "", "", http.StatusInternalServerError, "internal error"
	}
	if !isMember {
		return "", "", "", http.StatusNotFound, "target session not found on device"
	}
	return owner, deviceID, session, 0, ""
}

// buildRecipientStatusV2 reports a target session's current activity for send
// responses: an in-progress task takes priority over a processing message,
// then online/offline is derived from presence heartbeat freshness.
func (s *Server) buildRecipientStatusV2(ctx context.Context, teamID, userID, deviceID, session string) RecipientStatusV2 {
	rs := RecipientStatusV2{DeviceID: deviceID, Session: session}

	lastSeen, _ := s.rdb.HGet(ctx, deviceV2Key(teamID, userID, deviceID), "last_seen").Result()
	online := false
	var sinceLast time.Duration
	if lastSeen != "" {
		if t, err := time.Parse(time.RFC3339, lastSeen); err == nil {
			sinceLast = time.Since(t)
			online = sinceLast < agentOnlineWindowV2
		}
	}

	members, _ := s.rdb.SMembers(ctx, receivedTasksV2Key(teamID, deviceID, session)).Result()
	for _, tid := range members {
		taskKey := taskV2Key(teamID, tid)
		status, _ := s.rdb.HGet(ctx, taskKey, "status").Result()
		if status == "in_progress" {
			title, _ := s.rdb.HGet(ctx, taskKey, "title").Result()
			if title == "" {
				title = tid
			}
			issuedAt, _ := s.rdb.HGet(ctx, taskKey, "issued_at").Result()
			dur := ""
			if t, err := time.Parse(time.RFC3339, issuedAt); err == nil {
				dur = formatDuration(time.Since(t))
			}
			rs.Current = "task: " + tid + " " + title + " (" + dur + ")"
			return rs
		}
	}

	currentMsgKey := currentMsgV2Key(teamID, deviceID, session)
	if exists, _ := s.rdb.Exists(ctx, currentMsgKey).Result(); exists > 0 {
		title, _ := s.rdb.HGet(ctx, currentMsgKey, "title").Result()
		startedAt, _ := s.rdb.HGet(ctx, currentMsgKey, "started_at").Result()
		dur := ""
		if t, err := time.Parse(time.RFC3339, startedAt); err == nil {
			dur = formatDuration(time.Since(t))
		}
		rs.Current = "msg: " + title + " (" + dur + ")"
		return rs
	}

	if !online {
		rs.Current = "offline (" + formatDuration(sinceLast) + ")"
		return rs
	}
	rs.Current = "idle"
	return rs
}

// notifyInboxV2 pushes an informational message to a team member's inbox
// (best-effort; used for auto task reports).
func (s *Server) notifyInboxV2(ctx context.Context, teamID, deviceID, session string, msg MessageV2) {
	if deviceID == "" || session == "" {
		return
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	inboxKey := inboxV2Key(teamID, deviceID, session)
	s.rdb.LPush(ctx, inboxKey, data)
	s.rdb.Expire(ctx, inboxKey, taskTTLV2)
}

func (s *Server) writeBusyErrorV2(w http.ResponseWriter, ctx context.Context, teamID, userID, deviceID, session, msg string) {
	status := s.buildRecipientStatusV2(ctx, teamID, userID, deviceID, session)
	writeJSON(w, http.StatusConflict, map[string]any{
		"error":            msg,
		"recipient_status": &status,
	})
}
