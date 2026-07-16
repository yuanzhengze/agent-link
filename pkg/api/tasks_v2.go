package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// taskSendV2Script atomically checks task_id uniqueness, rejects a busy target
// (issued or in_progress task present), enforces a suspended cap of 2, then
// writes the task record, tracking set, issued index, and inbox item.
// KEYS[1]=task, KEYS[2]=received set, KEYS[3]=inbox, KEYS[4]=issued set
// ARGV[1]=task_id, [2]=now, [3]=assigned_to, [4]=issued_by, [5]=content,
// [6]=title, [7]=inbox_json, [8]=task key prefix
var taskSendV2Script = goredis.NewScript(`
if redis.call('EXISTS', KEYS[1]) > 0 then
  return {0, 'dup'}
end
local members = redis.call('SMEMBERS', KEYS[2])
local suspended = 0
for _, tid in ipairs(members) do
  local st = redis.call('HGET', ARGV[8] .. tid, 'status')
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
redis.call('HSET', KEYS[1],
  'task_id', ARGV[1], 'status', 'issued', 'assigned_to', ARGV[3],
  'issued_by', ARGV[4], 'content', ARGV[5], 'title', ARGV[6], 'issued_at', ARGV[2])
redis.call('EXPIRE', KEYS[1], 604800)
redis.call('SADD', KEYS[2], ARGV[1])
redis.call('SADD', KEYS[4], ARGV[1])
redis.call('EXPIRE', KEYS[4], 604800)
redis.call('LPUSH', KEYS[3], ARGV[7])
redis.call('EXPIRE', KEYS[3], 604800)
return {1, ''}
`)

// interruptTaskSendV2Script sends a task that interrupts a busy target: it skips
// the busy check and suspends any in_progress task first, then writes the new
// task. task_id uniqueness is still enforced.
var interruptTaskSendV2Script = goredis.NewScript(`
if redis.call('EXISTS', KEYS[1]) > 0 then
  return {0, 'dup'}
end
for _, tid in ipairs(redis.call('SMEMBERS', KEYS[2])) do
  local st = redis.call('HGET', ARGV[8] .. tid, 'status')
  if st == 'in_progress' then
    redis.call('HSET', ARGV[8] .. tid, 'status', 'suspended', 'suspended_at', ARGV[2])
  end
end
redis.call('HSET', KEYS[1],
  'task_id', ARGV[1], 'status', 'issued', 'assigned_to', ARGV[3],
  'issued_by', ARGV[4], 'content', ARGV[5], 'title', ARGV[6], 'issued_at', ARGV[2])
redis.call('EXPIRE', KEYS[1], 604800)
redis.call('SADD', KEYS[2], ARGV[1])
redis.call('SADD', KEYS[4], ARGV[1])
redis.call('EXPIRE', KEYS[4], 604800)
redis.call('LPUSH', KEYS[3], ARGV[7])
redis.call('EXPIRE', KEYS[3], 604800)
return {1, ''}
`)

type SendTaskRequestV2 struct {
	To        string `json:"to"`
	TaskID    string `json:"task_id,omitempty"`
	Title     string `json:"title,omitempty"`
	Interrupt bool   `json:"interrupt,omitempty"`
	Content   string `json:"content"`
}

type TaskResultRequestV2 struct {
	Status string `json:"status"`
	Result string `json:"result"`
}

type TaskResumeRequestV2 struct {
	Content string `json:"content"`
}

type TaskReopenRequestV2 struct {
	Reason string `json:"reason"`
}

type TaskStatusResponseV2 struct {
	TaskID      string `json:"task_id"`
	Status      string `json:"status"`
	AssignedTo  string `json:"assigned_to"`
	IssuedBy    string `json:"issued_by"`
	Content     string `json:"content"`
	Result      string `json:"result"`
	IssuedAt    string `json:"issued_at"`
	CompletedAt string `json:"completed_at"`
}

// handleSendTaskV2 assigns a task to a team member's device/session. The issuer
// identity is always the authenticated actor; the target is validated against
// the team index.
func (s *Server) handleSendTaskV2(w http.ResponseWriter, r *http.Request) {
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req SendTaskRequestV2
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
	if req.TaskID == "" {
		req.TaskID = generateID()[:8]
	} else if !deviceNameRE.MatchString(req.TaskID) {
		writeError(w, http.StatusBadRequest, "invalid task_id: 2-32 chars, lowercase letters/digits/hyphens/underscores, start with a letter")
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
	title := req.Title
	if title == "" {
		title = req.TaskID
	}
	assignedTo := targetDevice + ":" + targetSession
	issuedBy := actor.DeviceID + ":" + actor.SessionName

	inboxItem := MessageV2{
		ID:          generateID(),
		Type:        MsgTypeTask,
		FromDevice:  actor.DeviceID,
		FromSession: actor.SessionName,
		TaskID:      req.TaskID,
		Title:       title,
		Interrupt:   req.Interrupt,
		Content:     req.Content,
		CreatedAt:   now,
	}
	msgJSON, err := json.Marshal(inboxItem)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	script := taskSendV2Script
	if req.Interrupt {
		script = interruptTaskSendV2Script
	}
	res, err := script.Run(ctx, s.rdb,
		[]string{
			taskV2Key(teamID, req.TaskID),
			receivedTasksV2Key(teamID, targetDevice, targetSession),
			inboxV2Key(teamID, targetDevice, targetSession),
			issuedTasksV2Key(teamID, actor.DeviceID, actor.SessionName),
		},
		req.TaskID, now, assignedTo, issuedBy, req.Content, title, msgJSON, taskV2Prefix(teamID),
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
	codeVal, _ := arr[0].(int64)
	reason, _ := arr[1].(string)
	if codeVal == 0 {
		switch reason {
		case rejectDup:
			writeError(w, http.StatusConflict, "task_id already exists")
		case rejectBusy:
			s.writeBusyErrorV2(w, ctx, teamID, targetUserID, targetDevice, targetSession, "target session is busy")
		case rejectSuspended:
			s.writeBusyErrorV2(w, ctx, teamID, targetUserID, targetDevice, targetSession, "target has 2 suspended tasks")
		default:
			writeError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}

	status := s.buildRecipientStatusV2(ctx, teamID, targetUserID, targetDevice, targetSession)
	writeJSON(w, http.StatusOK, SendResponseV2{ID: inboxItem.ID, TaskID: req.TaskID, RecipientStatus: &status})
}

// loadTeamTask reads a team-scoped task hash. Because the key embeds the team
// id, a task belonging to another team is simply "not found" here.
func (s *Server) loadTeamTask(ctx context.Context, teamID, taskID string) (map[string]string, int, string) {
	if !deviceNameRE.MatchString(taskID) {
		return nil, http.StatusBadRequest, "invalid task_id"
	}
	data, err := s.rdb.HGetAll(ctx, taskV2Key(teamID, taskID)).Result()
	if err != nil {
		return nil, http.StatusInternalServerError, "internal error"
	}
	if len(data) == 0 {
		return nil, http.StatusNotFound, "task not found"
	}
	return data, 0, ""
}

func (s *Server) handleTaskResultV2(w http.ResponseWriter, r *http.Request) {
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	taskID := r.PathValue("task_id")

	var req TaskResultRequestV2
	if !decodeLockJSON(w, r, &req) {
		return
	}
	if req.Status != "completed" && req.Status != "suspended" {
		writeError(w, http.StatusBadRequest, "status must be completed or suspended")
		return
	}
	if strings.TrimSpace(req.Result) == "" {
		writeError(w, http.StatusBadRequest, "missing field: result")
		return
	}

	ctx := r.Context()
	teamID := actor.TeamID
	data, code, errMsg := s.loadTeamTask(ctx, teamID, taskID)
	if code != 0 {
		writeError(w, code, errMsg)
		return
	}
	if data["status"] != "in_progress" {
		hint := ""
		switch data["status"] {
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
	taskKey := taskV2Key(teamID, taskID)
	if err := s.rdb.HSet(ctx, taskKey,
		"status", req.Status,
		"result", req.Result,
		"completed_at", now,
	).Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	assignedDev, assignedSess := splitPair(data["assigned_to"])
	if assignedDev != "" {
		s.rdb.SRem(ctx, receivedTasksV2Key(teamID, assignedDev, assignedSess), taskID)
	}

	issuedDev, issuedSess := splitPair(data["issued_by"])
	if issuedDev != "" {
		s.rdb.SRem(ctx, issuedTasksV2Key(teamID, issuedDev, issuedSess), taskID)
		if assignedDev != "" {
			s.notifyInboxV2(ctx, teamID, issuedDev, issuedSess, MessageV2{
				ID:          generateID(),
				Type:        MsgTypeMsg,
				FromDevice:  assignedDev,
				FromSession: assignedSess,
				Title:       "任务回报 " + taskID,
				Content:     req.Status + ": " + req.Result,
				CreatedAt:   now,
			})
		}
	}

	s.rdb.Expire(ctx, taskKey, 30*24*time.Hour)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleTaskResumeV2(w http.ResponseWriter, r *http.Request) {
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	taskID := r.PathValue("task_id")

	var req TaskResumeRequestV2
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
	data, code, errMsg := s.loadTeamTask(ctx, teamID, taskID)
	if code != 0 {
		writeError(w, code, errMsg)
		return
	}
	if data["status"] != "suspended" {
		writeError(w, http.StatusBadRequest, "task is not suspended")
		return
	}

	now := time.Now().UTC().Format(time.RFC3339)
	taskKey := taskV2Key(teamID, taskID)
	if err := s.rdb.HSet(ctx, taskKey,
		"status", "issued",
		"content", req.Content,
		"issued_at", now,
		"result", "",
		"completed_at", "",
	).Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	assignedDev, assignedSess := splitPair(data["assigned_to"])
	issuedDev, issuedSess := splitPair(data["issued_by"])
	if assignedDev != "" {
		s.rdb.SAdd(ctx, receivedTasksV2Key(teamID, assignedDev, assignedSess), taskID)
	}
	if issuedDev != "" {
		issuedKey := issuedTasksV2Key(teamID, issuedDev, issuedSess)
		s.rdb.SAdd(ctx, issuedKey, taskID)
		s.rdb.Expire(ctx, issuedKey, taskTTLV2)
	}

	inboxItem := MessageV2{
		ID:          generateID(),
		Type:        MsgTypeTask,
		FromDevice:  issuedDev,
		FromSession: issuedSess,
		TaskID:      taskID,
		Title:       data["title"],
		Content:     req.Content,
		CreatedAt:   now,
	}
	if assignedDev != "" {
		if msgJSON, err := json.Marshal(inboxItem); err == nil {
			inboxKey := inboxV2Key(teamID, assignedDev, assignedSess)
			s.rdb.LPush(ctx, inboxKey, msgJSON)
			s.rdb.Expire(ctx, inboxKey, taskTTLV2)
		}
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleTaskCancelV2(w http.ResponseWriter, r *http.Request) {
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	taskID := r.PathValue("task_id")

	ctx := r.Context()
	teamID := actor.TeamID
	data, code, errMsg := s.loadTeamTask(ctx, teamID, taskID)
	if code != 0 {
		writeError(w, code, errMsg)
		return
	}
	prevStatus := data["status"]
	if prevStatus == "completed" || prevStatus == "cancelled" {
		writeError(w, http.StatusBadRequest, "task already "+prevStatus)
		return
	}

	now := time.Now().UTC().Format(time.RFC3339)
	taskKey := taskV2Key(teamID, taskID)
	if err := s.rdb.HSet(ctx, taskKey,
		"status", "cancelled",
		"completed_at", now,
	).Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	assignedDev, assignedSess := splitPair(data["assigned_to"])
	if assignedDev != "" {
		s.rdb.SRem(ctx, receivedTasksV2Key(teamID, assignedDev, assignedSess), taskID)
	}
	issuedDev, issuedSess := splitPair(data["issued_by"])
	if issuedDev != "" {
		s.rdb.SRem(ctx, issuedTasksV2Key(teamID, issuedDev, issuedSess), taskID)
	}

	// Only notify the target if it had already seen the task (in_progress or
	// suspended); a still-queued task is dropped by the pull-side filter.
	if issuedDev != "" && assignedDev != "" && (prevStatus == "in_progress" || prevStatus == "suspended") {
		s.notifyInboxV2(ctx, teamID, assignedDev, assignedSess, MessageV2{
			ID:          generateID(),
			Type:        MsgTypeMsg,
			FromDevice:  issuedDev,
			FromSession: issuedSess,
			Title:       "任务回报 " + taskID,
			Content:     "cancelled",
			CreatedAt:   now,
		})
	}

	s.rdb.Expire(ctx, taskKey, 30*24*time.Hour)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleTaskReopenV2(w http.ResponseWriter, r *http.Request) {
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	taskID := r.PathValue("task_id")

	var req TaskReopenRequestV2
	if !decodeLockJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		writeError(w, http.StatusBadRequest, "missing field: reason")
		return
	}
	if len(req.Reason) > maxReasonV2 {
		writeError(w, http.StatusBadRequest, "reason exceeds 1000 characters")
		return
	}

	ctx := r.Context()
	teamID := actor.TeamID
	data, code, errMsg := s.loadTeamTask(ctx, teamID, taskID)
	if code != 0 {
		writeError(w, code, errMsg)
		return
	}
	if data["status"] != "completed" && data["status"] != "cancelled" {
		writeError(w, http.StatusBadRequest, "task is not completed or cancelled (use task resume for suspended)")
		return
	}

	now := time.Now().UTC().Format(time.RFC3339)
	taskKey := taskV2Key(teamID, taskID)
	if err := s.rdb.HSet(ctx, taskKey,
		"status", "issued",
		"result", "",
		"completed_at", "",
		"reopen_reason", req.Reason,
	).Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	assignedDev, assignedSess := splitPair(data["assigned_to"])
	issuedDev, issuedSess := splitPair(data["issued_by"])
	if assignedDev != "" {
		s.rdb.SAdd(ctx, receivedTasksV2Key(teamID, assignedDev, assignedSess), taskID)
	}
	if issuedDev != "" {
		issuedKey := issuedTasksV2Key(teamID, issuedDev, issuedSess)
		s.rdb.SAdd(ctx, issuedKey, taskID)
		s.rdb.Expire(ctx, issuedKey, taskTTLV2)
	}

	inboxItem := MessageV2{
		ID:          generateID(),
		Type:        MsgTypeTask,
		FromDevice:  issuedDev,
		FromSession: issuedSess,
		TaskID:      taskID,
		Title:       data["title"],
		Content:     "[重发原因: " + req.Reason + "]\n" + data["content"],
		CreatedAt:   now,
	}
	if assignedDev != "" {
		if msgJSON, err := json.Marshal(inboxItem); err == nil {
			inboxKey := inboxV2Key(teamID, assignedDev, assignedSess)
			s.rdb.LPush(ctx, inboxKey, msgJSON)
			s.rdb.Expire(ctx, inboxKey, taskTTLV2)
		}
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleTaskStatusV2(w http.ResponseWriter, r *http.Request) {
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	taskID := r.PathValue("task_id")

	ctx := r.Context()
	data, code, errMsg := s.loadTeamTask(ctx, actor.TeamID, taskID)
	if code != 0 {
		writeError(w, code, errMsg)
		return
	}
	writeJSON(w, http.StatusOK, taskStatusFromHashV2(data))
}

func (s *Server) handleTaskListV2(w http.ResponseWriter, r *http.Request) {
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	ctx := r.Context()
	teamID := actor.TeamID
	received := s.readTasksV2(ctx, teamID, receivedTasksV2Key(teamID, actor.DeviceID, actor.SessionName))
	sent := s.readTasksV2(ctx, teamID, issuedTasksV2Key(teamID, actor.DeviceID, actor.SessionName))
	writeJSON(w, http.StatusOK, map[string]any{"received": received, "sent": sent})
}

func (s *Server) readTasksV2(ctx context.Context, teamID, setKey string) []TaskStatusResponseV2 {
	members, err := s.rdb.SMembers(ctx, setKey).Result()
	if err != nil {
		return nil
	}
	tasks := make([]TaskStatusResponseV2, 0, len(members))
	for _, tid := range members {
		data, err := s.rdb.HGetAll(ctx, taskV2Key(teamID, tid)).Result()
		if err != nil || len(data) == 0 {
			continue
		}
		tasks = append(tasks, taskStatusFromHashV2(data))
	}
	return tasks
}

func taskStatusFromHashV2(data map[string]string) TaskStatusResponseV2 {
	return TaskStatusResponseV2{
		TaskID:      data["task_id"],
		Status:      data["status"],
		AssignedTo:  data["assigned_to"],
		IssuedBy:    data["issued_by"],
		Content:     data["content"],
		Result:      data["result"],
		IssuedAt:    data["issued_at"],
		CompletedAt: data["completed_at"],
	}
}

// splitPair splits a stored "device:session" pair. It returns two empty strings
// if the value is malformed, so callers can skip absent counterparts safely.
func splitPair(v string) (string, string) {
	parts := strings.SplitN(v, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", ""
	}
	return parts[0], parts[1]
}
