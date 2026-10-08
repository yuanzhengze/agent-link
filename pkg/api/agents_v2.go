package api

import (
	"net/http"
	"sort"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// Team-scoped device presence. Every (team, user, device) that heartbeats is
// tracked so a team's dashboard can show who is online. Keys are team-scoped so
// one team can never see another team's devices, and session mutation is bound
// to the authenticated actor's own device only.
const (
	agentOnlineWindowV2 = 120 * time.Second
	devicePresenceTTLV2 = 7 * 24 * time.Hour
	// maxSessionsV2 caps how many agent sessions one device may declare, so a
	// single PATCH cannot balloon the session set (the body size limit is a
	// coarser bound). Well above any realistic per-device agent count.
	maxSessionsV2 = 64
)

func deviceV2Key(teamID, userID, deviceID string) string {
	return "agentlink:v2:device:" + teamID + ":" + userID + ":" + deviceID
}

func deviceSessionsV2Key(teamID, userID, deviceID string) string {
	return "agentlink:v2:device:" + teamID + ":" + userID + ":" + deviceID + ":sessions"
}

func teamDevicesV2Key(teamID string) string {
	return "agentlink:v2:team:" + teamID + ":devices"
}

// teamDeviceMember is the set member stored in the team device index. userID and
// deviceID never contain ':' (they are "u_..."/"d_..." randoms or the literal
// "web"), so SplitN on the first ':' round-trips cleanly.
func teamDeviceMember(userID, deviceID string) string {
	return userID + ":" + deviceID
}

// patchSessionsReplaceV2Script atomically replaces a device's session set.
var patchSessionsReplaceV2Script = goredis.NewScript(`
redis.call('del', KEYS[1])
for i = 1, #ARGV do
  redis.call('sadd', KEYS[1], ARGV[i])
end
return #ARGV
`)

// deleteSessionV2Script atomically removes one session, refusing to remove the
// last one. Returns 1 on success, 0 if the session is absent, -1 if removing it
// would empty the set.
var deleteSessionV2Script = goredis.NewScript(`
if redis.call('sismember', KEYS[1], ARGV[1]) == 0 then
  return 0
end
if redis.call('scard', KEYS[1]) <= 1 then
  return -1
end
redis.call('srem', KEYS[1], ARGV[1])
return 1
`)

type AgentInfoV2 struct {
	UserID     string   `json:"user_id"`
	Username   string   `json:"username"`
	DeviceID   string   `json:"device_id"`
	DeviceName string   `json:"device_name"`
	ClientType string   `json:"client_type"`
	Sessions   []string `json:"sessions"`
	LastSeen   string   `json:"last_seen"`
	Online     bool     `json:"online"`
}

type AgentListResponseV2 struct {
	Agents []AgentInfoV2 `json:"agents"`
}

type PatchSessionsRequestV2 struct {
	Sessions []string `json:"sessions"`
}

type SessionsResponseV2 struct {
	Sessions []string `json:"sessions"`
}

// handleHeartbeatV2 records the authenticated actor's device presence in the
// team. It never trusts a body: identity comes entirely from the actor, so a
// caller can only ever mark its own device online. Web actors are recorded with
// the fixed "web"/"gui" identity so the dashboard shows them as a Web GUI agent.
func (s *Server) handleHeartbeatV2(w http.ResponseWriter, r *http.Request) {
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	ctx := r.Context()
	devKey := deviceV2Key(actor.TeamID, actor.UserID, actor.DeviceID)
	now := time.Now().UTC().Format(time.RFC3339)

	if err := s.rdb.HSet(ctx, devKey,
		"user_id", actor.UserID,
		"username", actor.Username,
		"device_id", actor.DeviceID,
		"device_name", actor.DeviceName,
		"client_type", actor.ClientType,
		"last_seen", now,
	).Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.rdb.Expire(ctx, devKey, devicePresenceTTLV2)

	sessKey := deviceSessionsV2Key(actor.TeamID, actor.UserID, actor.DeviceID)
	if actor.ClientType == "web" {
		// The Web GUI is not an agent host; its only "session" is the GUI.
		s.rdb.SAdd(ctx, sessKey, "gui")
	}
	// No-op if the set does not exist (a device that has not PATCHed sessions).
	s.rdb.Expire(ctx, sessKey, devicePresenceTTLV2)

	if err := s.rdb.SAdd(ctx, teamDevicesV2Key(actor.TeamID), teamDeviceMember(actor.UserID, actor.DeviceID)).Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleListAgentsV2 lists every device recorded for the caller's team. It is
// self-healing: an index member whose presence hash has expired is pruned. Team
// scoping in the key space guarantees another team's devices never appear.
func (s *Server) handleListAgentsV2(w http.ResponseWriter, r *http.Request) {
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	ctx := r.Context()
	indexKey := teamDevicesV2Key(actor.TeamID)
	members, err := s.rdb.SMembers(ctx, indexKey).Result()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	agents := make([]AgentInfoV2, 0, len(members))
	for _, m := range members {
		parts := strings.SplitN(m, ":", 2)
		if len(parts) != 2 {
			continue
		}
		userID, deviceID := parts[0], parts[1]

		data, err := s.rdb.HGetAll(ctx, deviceV2Key(actor.TeamID, userID, deviceID)).Result()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if len(data) == 0 {
			// Presence expired; drop the stale index entry.
			s.rdb.SRem(ctx, indexKey, m)
			continue
		}

		sessions, _ := s.rdb.SMembers(ctx, deviceSessionsV2Key(actor.TeamID, userID, deviceID)).Result()
		sort.Strings(sessions)
		if sessions == nil {
			sessions = []string{}
		}

		online := false
		if ls := data["last_seen"]; ls != "" {
			if t, perr := time.Parse(time.RFC3339, ls); perr == nil {
				online = time.Since(t) < agentOnlineWindowV2
			}
		}

		agents = append(agents, AgentInfoV2{
			UserID:     data["user_id"],
			Username:   data["username"],
			DeviceID:   data["device_id"],
			DeviceName: data["device_name"],
			ClientType: data["client_type"],
			Sessions:   sessions,
			LastSeen:   data["last_seen"],
			Online:     online,
		})
	}

	sort.Slice(agents, func(i, j int) bool {
		if agents[i].Username != agents[j].Username {
			return agents[i].Username < agents[j].Username
		}
		return agents[i].DeviceID < agents[j].DeviceID
	})

	writeJSON(w, http.StatusOK, AgentListResponseV2{Agents: agents})
}

// handlePatchSessionsV2 replaces the caller's own device session list. The
// target device is always the authenticated actor's device; any device_id in
// the body is ignored, so a member can never mutate another member's device.
func (s *Server) handlePatchSessionsV2(w http.ResponseWriter, r *http.Request) {
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if actor.ClientType == "web" {
		writeError(w, http.StatusBadRequest, "web GUI sessions are managed automatically")
		return
	}

	var req PatchSessionsRequestV2
	if !decodeLockJSON(w, r, &req) {
		return
	}
	sessions, ok := normalizeSessionsV2(req.Sessions)
	if !ok {
		writeError(w, http.StatusBadRequest, "sessions must be a non-empty list of valid names")
		return
	}

	ctx := r.Context()
	devKey := deviceV2Key(actor.TeamID, actor.UserID, actor.DeviceID)
	if exists, err := s.rdb.Exists(ctx, devKey).Result(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	} else if exists == 0 {
		writeError(w, http.StatusNotFound, "device not found; send a heartbeat first")
		return
	}

	sessKey := deviceSessionsV2Key(actor.TeamID, actor.UserID, actor.DeviceID)
	args := make([]any, 0, len(sessions))
	for _, sess := range sessions {
		args = append(args, sess)
	}
	if err := patchSessionsReplaceV2Script.Run(ctx, s.rdb, []string{sessKey}, args...).Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.rdb.Expire(ctx, sessKey, devicePresenceTTLV2)

	sort.Strings(sessions)
	writeJSON(w, http.StatusOK, SessionsResponseV2{Sessions: sessions})
}

// handleDeleteSessionV2 removes one session from the caller's own device,
// refusing to remove the last remaining session.
func (s *Server) handleDeleteSessionV2(w http.ResponseWriter, r *http.Request) {
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if actor.ClientType == "web" {
		writeError(w, http.StatusBadRequest, "web GUI sessions are managed automatically")
		return
	}

	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		writeError(w, http.StatusBadRequest, "missing name parameter")
		return
	}
	if !deviceNameRE.MatchString(name) {
		writeError(w, http.StatusBadRequest, "invalid session name")
		return
	}

	ctx := r.Context()
	devKey := deviceV2Key(actor.TeamID, actor.UserID, actor.DeviceID)
	if exists, err := s.rdb.Exists(ctx, devKey).Result(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	} else if exists == 0 {
		writeError(w, http.StatusNotFound, "device not found; send a heartbeat first")
		return
	}

	sessKey := deviceSessionsV2Key(actor.TeamID, actor.UserID, actor.DeviceID)
	code, err := deleteSessionV2Script.Run(ctx, s.rdb, []string{sessKey}, name).Int64()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	switch code {
	case -1:
		writeError(w, http.StatusBadRequest, "cannot remove the last session")
		return
	case 0:
		writeError(w, http.StatusNotFound, "session not found")
		return
	}

	sessions, _ := s.rdb.SMembers(ctx, sessKey).Result()
	sort.Strings(sessions)
	if sessions == nil {
		sessions = []string{}
	}
	writeJSON(w, http.StatusOK, SessionsResponseV2{Sessions: sessions})
}

// normalizeSessionsV2 validates and de-duplicates a requested session list,
// returning false if it is empty or contains an invalid name.
func normalizeSessionsV2(in []string) ([]string, bool) {
	if len(in) == 0 {
		return nil, false
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, sess := range in {
		if !deviceNameRE.MatchString(sess) {
			return nil, false
		}
		if _, dup := seen[sess]; dup {
			continue
		}
		seen[sess] = struct{}{}
		out = append(out, sess)
	}
	if len(out) == 0 || len(out) > maxSessionsV2 {
		return nil, false
	}
	return out, true
}
