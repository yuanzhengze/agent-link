package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/team/agentlink/pkg/auth"
)

var teamTestRoutesRegistered bool

func userIDFromAuthResult(t *testing.T, result map[string]any) string {
	t.Helper()
	user, ok := result["user"].(map[string]any)
	if !ok {
		t.Fatal("auth result missing user")
	}
	id, ok := user["id"].(string)
	if !ok || id == "" {
		t.Fatal("auth result missing user id")
	}
	return id
}

func ensureTeamTestRoutes(t *testing.T) {
	t.Helper()
	wasNil := authV2TS == nil
	setupAuthV2TestServer(t)
	if wasNil {
		teamTestRoutesRegistered = false
	}
	if teamTestRoutesRegistered {
		return
	}
	authV2Srv.mux.Handle(
		"GET /api/teams/{team_id}/test-actor",
		authV2Srv.requireIdentity(authV2Srv.requireTeamRole()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			actor, ok := ActorFromContext(r.Context())
			if !ok {
				writeError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{
				"team_id": actor.TeamID,
				"role":    string(actor.Role),
			})
		}))),
	)
	teamTestRoutesRegistered = true
}

func teamJSON(t *testing.T, method, path string, body any, session *http.Response, extra map[string]string) (*http.Response, []byte) {
	t.Helper()
	headers := map[string]string{"Cookie": withCookies(session)}
	if isCookieWriteMethod(method) {
		csrf := cookieByName(session.Cookies(), csrfCookieName)
		headers["Origin"] = authTestOrigin
		headers["X-CSRF-Token"] = csrf.Value
	}
	for k, v := range extra {
		headers[k] = v
	}
	return authJSON(t, method, path, body, headers)
}

func registerTeamUser(t *testing.T, suffix string) (*http.Response, map[string]any) {
	t.Helper()
	username := "team" + suffix + strings.Repeat("u", 6)
	return registerAuthUser(t, username, "correct horse battery staple")
}

func createTeamHTTP(t *testing.T, session *http.Response, name string) (map[string]any, string) {
	t.Helper()
	resp, body := teamJSON(t, http.MethodPost, "/api/teams", map[string]string{"name": name}, session, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create team expected 201, got %d body=%s", resp.StatusCode, body)
	}
	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	invite, _ := result["invite_code"].(string)
	if invite == "" {
		t.Fatalf("create team missing invite_code: %s", body)
	}
	team, ok := result["team"].(map[string]any)
	if !ok {
		t.Fatalf("create team missing team object: %s", body)
	}
	return team, invite
}

func joinTeamHTTP(t *testing.T, session *http.Response, teamID, invite string) map[string]any {
	t.Helper()
	resp, body := teamJSON(t, http.MethodPost, "/api/teams/join", map[string]string{
		"team_id":     teamID,
		"invite_code": invite,
	}, session, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("join team expected 200, got %d body=%s", resp.StatusCode, body)
	}
	var team map[string]any
	if err := json.Unmarshal(body, &team); err != nil {
		t.Fatal(err)
	}
	return team
}

func listTeamMembersHTTP(t *testing.T, session *http.Response, teamID string) []map[string]any {
	t.Helper()
	resp, body := teamJSON(t, http.MethodGet, "/api/teams/"+teamID+"/members", nil, session, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list members expected 200, got %d body=%s", resp.StatusCode, body)
	}
	var result map[string]any
	json.Unmarshal(body, &result)
	raw, ok := result["members"].([]any)
	if !ok {
		t.Fatalf("members response missing members: %s", body)
	}
	members := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		members = append(members, item.(map[string]any))
	}
	return members
}

func memberIDByRole(t *testing.T, members []map[string]any, role string, excludeID string) string {
	t.Helper()
	for _, m := range members {
		if m["role"] == role && m["user_id"] != excludeID {
			return m["user_id"].(string)
		}
	}
	t.Fatalf("no member with role %q excluding %q in %v", role, excludeID, members)
	return ""
}

func assertNoInviteHash(t *testing.T, payload []byte) {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatal(err)
	}
	if containsInviteHash(raw) {
		t.Fatalf("response must not expose invite_hash: %s", payload)
	}
}

func containsInviteHash(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if k == "invite_hash" {
				return true
			}
			if containsInviteHash(val) {
				return true
			}
		}
	case []any:
		for _, item := range x {
			if containsInviteHash(item) {
				return true
			}
		}
	}
	return false
}

func TestTeamCreateJoinList(t *testing.T) {
	ensureTeamTestRoutes(t)
	cleanupAuthV2Keys(t)

	ownerSess, _ := registerTeamUser(t, "owner1")
	team, invite := createTeamHTTP(t, ownerSess, "Alpha Squad")
	teamID, _ := team["id"].(string)
	if team["role"] != "owner" {
		t.Fatalf("created team role = %v; want owner", team["role"])
	}
	assertNoInviteHash(t, mustMarshal(t, team))

	memberSess, _ := registerTeamUser(t, "member1")
	joined := joinTeamHTTP(t, memberSess, teamID, invite)
	if joined["role"] != "member" {
		t.Fatalf("joined team role = %v; want member", joined["role"])
	}

	resp, body := teamJSON(t, http.MethodGet, "/api/teams", nil, ownerSess, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list teams expected 200, got %d body=%s", resp.StatusCode, body)
	}
	var listResult map[string]any
	json.Unmarshal(body, &listResult)
	teams, ok := listResult["teams"].([]any)
	if !ok || len(teams) != 1 {
		t.Fatalf("owner teams list = %v; want one team", listResult["teams"])
	}
	if teams[0].(map[string]any)["role"] != "owner" {
		t.Fatal("listed team role should be owner")
	}

	memberResp, memberBody := teamJSON(t, http.MethodGet, "/api/teams", nil, memberSess, nil)
	if memberResp.StatusCode != http.StatusOK {
		t.Fatalf("member list expected 200, got %d", memberResp.StatusCode)
	}
	var memberList map[string]any
	json.Unmarshal(memberBody, &memberList)
	memberTeams, _ := memberList["teams"].([]any)
	if len(memberTeams) != 1 || memberTeams[0].(map[string]any)["role"] != "member" {
		t.Fatalf("member list = %v; want one team with member role", memberList["teams"])
	}
}

func TestTeamInviteRotationRejectsOldCode(t *testing.T) {
	ensureTeamTestRoutes(t)
	cleanupAuthV2Keys(t)

	ownerSess, _ := registerTeamUser(t, "rotown")
	team, oldInvite := createTeamHTTP(t, ownerSess, "Rotate Me")
	teamID, _ := team["id"].(string)

	rotateResp, rotateBody := teamJSON(t, http.MethodPost, "/api/teams/"+teamID+"/invite/rotate", nil, ownerSess, nil)
	if rotateResp.StatusCode != http.StatusOK {
		t.Fatalf("rotate expected 200, got %d body=%s", rotateResp.StatusCode, rotateBody)
	}
	var rotateResult map[string]any
	json.Unmarshal(rotateBody, &rotateResult)
	newInvite, _ := rotateResult["invite_code"].(string)
	if newInvite == "" || newInvite == oldInvite {
		t.Fatalf("rotate invite_code = %q; want new code", newInvite)
	}
	assertNoInviteHash(t, rotateBody)

	joinerSess, _ := registerTeamUser(t, "rotjoin")
	badResp, badBody := teamJSON(t, http.MethodPost, "/api/teams/join", map[string]string{
		"team_id":     teamID,
		"invite_code": oldInvite,
	}, joinerSess, nil)
	if badResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("old invite expected 400, got %d body=%s", badResp.StatusCode, badBody)
	}
	var badErr map[string]string
	json.Unmarshal(badBody, &badErr)
	if badErr["error"] != "invalid team or invite code" {
		t.Fatalf("old invite error = %q; want generic message", badErr["error"])
	}

	joinTeamHTTP(t, joinerSess, teamID, newInvite)
}

func TestTeamMemberRoleMatrix(t *testing.T) {
	ensureTeamTestRoutes(t)
	cleanupAuthV2Keys(t)

	ownerSess, _ := registerTeamUser(t, "matrix")
	team, invite := createTeamHTTP(t, ownerSess, "Matrix Team")
	teamID, _ := team["id"].(string)

	adminSess, adminUser := registerTeamUser(t, "adminm")
	memberSess, memberUser := registerTeamUser(t, "memberm")
	joinTeamHTTP(t, adminSess, teamID, invite)
	joinTeamHTTP(t, memberSess, teamID, invite)

	adminID := userIDFromAuthResult(t, adminUser)
	memberID := userIDFromAuthResult(t, memberUser)

	promoteResp, _ := teamJSON(t, http.MethodPatch, "/api/teams/"+teamID+"/members/"+adminID, map[string]string{"role": "admin"}, ownerSess, nil)
	if promoteResp.StatusCode != http.StatusNoContent {
		t.Fatalf("owner promote expected 204, got %d", promoteResp.StatusCode)
	}

	denyResp, _ := teamJSON(t, http.MethodPatch, "/api/teams/"+teamID+"/members/"+memberID, map[string]string{"role": "admin"}, memberSess, nil)
	if denyResp.StatusCode != http.StatusForbidden {
		t.Fatalf("member change role expected 403, got %d", denyResp.StatusCode)
	}

	denyAdminResp, _ := teamJSON(t, http.MethodDelete, "/api/teams/"+teamID+"/members/"+adminID, nil, adminSess, nil)
	if denyAdminResp.StatusCode != http.StatusForbidden {
		t.Fatalf("admin remove admin expected 403, got %d", denyAdminResp.StatusCode)
	}

	removeResp, _ := teamJSON(t, http.MethodDelete, "/api/teams/"+teamID+"/members/"+memberID, nil, adminSess, nil)
	if removeResp.StatusCode != http.StatusNoContent {
		t.Fatalf("admin remove member expected 204, got %d", removeResp.StatusCode)
	}

	demoteResp, _ := teamJSON(t, http.MethodPatch, "/api/teams/"+teamID+"/members/"+adminID, map[string]string{"role": "member"}, ownerSess, nil)
	if demoteResp.StatusCode != http.StatusNoContent {
		t.Fatalf("owner demote expected 204, got %d", demoteResp.StatusCode)
	}
}

func TestTeamTransferOwner(t *testing.T) {
	ensureTeamTestRoutes(t)
	cleanupAuthV2Keys(t)

	ownerSess, ownerUser := registerTeamUser(t, "xfer")
	team, invite := createTeamHTTP(t, ownerSess, "Transfer Team")
	teamID, _ := team["id"].(string)
	ownerID := userIDFromAuthResult(t, ownerUser)

	successorSess, _ := registerTeamUser(t, "succ")
	joinTeamHTTP(t, successorSess, teamID, invite)

	members := listTeamMembersHTTP(t, ownerSess, teamID)
	successorID := memberIDByRole(t, members, "member", ownerID)

	xferResp, xferBody := teamJSON(t, http.MethodPost, "/api/teams/"+teamID+"/transfer-owner", map[string]string{"user_id": successorID}, ownerSess, nil)
	if xferResp.StatusCode != http.StatusNoContent {
		t.Fatalf("transfer expected 204, got %d body=%s", xferResp.StatusCode, xferBody)
	}

	after := listTeamMembersHTTP(t, successorSess, teamID)
	roles := map[string]string{}
	for _, m := range after {
		roles[m["user_id"].(string)] = m["role"].(string)
	}
	if roles[successorID] != "owner" {
		t.Fatalf("successor role = %q; want owner", roles[successorID])
	}
	if roles[ownerID] != "admin" {
		t.Fatalf("old owner role = %q; want admin", roles[ownerID])
	}
}

func TestTeamMemberCanLeaveAndOwnerCannot(t *testing.T) {
	ensureTeamTestRoutes(t)
	cleanupAuthV2Keys(t)

	ownerSess, _ := registerTeamUser(t, "leaveown")
	team, invite := createTeamHTTP(t, ownerSess, "Leave Team")
	teamID, _ := team["id"].(string)

	memberSess, _ := registerTeamUser(t, "leavemem")
	joinTeamHTTP(t, memberSess, teamID, invite)

	ownerLeave, ownerBody := teamJSON(t, http.MethodPost, "/api/teams/"+teamID+"/leave", nil, ownerSess, nil)
	if ownerLeave.StatusCode != http.StatusForbidden {
		t.Fatalf("owner leave expected 403, got %d body=%s", ownerLeave.StatusCode, ownerBody)
	}

	memberLeave, _ := teamJSON(t, http.MethodPost, "/api/teams/"+teamID+"/leave", nil, memberSess, nil)
	if memberLeave.StatusCode != http.StatusNoContent {
		t.Fatalf("member leave expected 204, got %d", memberLeave.StatusCode)
	}

	probe, probeBody := teamJSON(t, http.MethodGet, "/api/teams/"+teamID+"/test-actor", nil, memberSess, nil)
	if probe.StatusCode != http.StatusForbidden {
		t.Fatalf("left member probe expected 403, got %d body=%s", probe.StatusCode, probeBody)
	}
}

func TestTeamMiddlewareRejectsNonMember(t *testing.T) {
	ensureTeamTestRoutes(t)
	cleanupAuthV2Keys(t)

	ownerSess, _ := registerTeamUser(t, "nonmem")
	team, _ := createTeamHTTP(t, ownerSess, "Private Team")
	teamID, _ := team["id"].(string)

	outsiderSess, _ := registerTeamUser(t, "outsider")
	resp, body := teamJSON(t, http.MethodGet, "/api/teams/"+teamID, nil, outsiderSess, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-member get team expected 403, got %d body=%s", resp.StatusCode, body)
	}
}

func TestTeamMiddlewareUsesURLTeamNotClientPreference(t *testing.T) {
	ensureTeamTestRoutes(t)
	cleanupAuthV2Keys(t)

	userSess, _ := registerTeamUser(t, "urlpref")
	teamA, _ := createTeamHTTP(t, userSess, "Team A")
	teamB, _ := createTeamHTTP(t, userSess, "Team B")
	teamAID, _ := teamA["id"].(string)
	teamBID, _ := teamB["id"].(string)

	resp, body := teamJSON(t, http.MethodGet, "/api/teams/"+teamAID+"/test-actor?team_id="+teamBID, nil, userSess, map[string]string{
		"X-Team-ID": teamBID,
		"X-Role":    "owner",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("test-actor expected 200, got %d body=%s", resp.StatusCode, body)
	}
	var actor map[string]string
	json.Unmarshal(body, &actor)
	if actor["team_id"] != teamAID {
		t.Fatalf("actor team_id = %q; want URL team %q", actor["team_id"], teamAID)
	}
	if actor["role"] != "owner" {
		t.Fatalf("actor role = %q; want owner from store", actor["role"])
	}
}

func TestTeamCookieCSRFRequired(t *testing.T) {
	ensureTeamTestRoutes(t)
	cleanupAuthV2Keys(t)

	sess, _ := registerTeamUser(t, "csrfreq")
	req, _ := http.NewRequest(http.MethodPost, authV2TS.URL+"/api/teams", strings.NewReader(`{"name":"No CSRF"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", withCookies(sess))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("create without csrf expected 403, got %d body=%s", resp.StatusCode, body)
	}
}

func TestTeamDeviceNoCSRFRequired(t *testing.T) {
	ensureTeamTestRoutes(t)
	cleanupAuthV2Keys(t)

	_, userResult := registerTeamUser(t, "devcsrf")
	username := userResult["user"].(map[string]any)["username"].(string)

	devResp, devBody := authJSON(t, http.MethodPost, "/api/auth/device-login", map[string]string{
		"username":    username,
		"password":    "correct horse battery staple",
		"device_name": "team-device",
	}, nil)
	if devResp.StatusCode != http.StatusOK {
		t.Fatal(devBody)
	}
	var devResult map[string]any
	json.Unmarshal(devBody, &devResult)
	credential := devResult["device_credential"].(string)

	resp, body := authJSON(t, http.MethodPost, "/api/teams", map[string]string{"name": "Device Team"}, map[string]string{
		"Authorization": "Device " + credential,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("device create team expected 201, got %d body=%s", resp.StatusCode, body)
	}
}

func TestTeamResponseNoInviteHash(t *testing.T) {
	ensureTeamTestRoutes(t)
	cleanupAuthV2Keys(t)

	sess, _ := registerTeamUser(t, "nohash")
	resp, body := teamJSON(t, http.MethodPost, "/api/teams", map[string]string{"name": "Hash Check"}, sess, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatal(body)
	}
	assertNoInviteHash(t, body)

	team, invite := createTeamHTTP(t, sess, "Second")
	teamID := team["id"].(string)
	getResp, getBody := teamJSON(t, http.MethodGet, "/api/teams/"+teamID, nil, sess, nil)
	if getResp.StatusCode != http.StatusOK {
		t.Fatal(getBody)
	}
	assertNoInviteHash(t, getBody)

	joinerSess, _ := registerTeamUser(t, "hashjoin")
	joinBody, _ := json.Marshal(map[string]string{"team_id": teamID, "invite_code": invite})
	joinReq, _ := http.NewRequest(http.MethodPost, authV2TS.URL+"/api/teams/join", bytes.NewReader(joinBody))
	joinReq.Header.Set("Content-Type", "application/json")
	joinReq.Header.Set("Cookie", withCookies(joinerSess))
	joinReq.Header.Set("Origin", authTestOrigin)
	joinReq.Header.Set("X-CSRF-Token", cookieByName(joinerSess.Cookies(), csrfCookieName).Value)
	joinResp, err := http.DefaultClient.Do(joinReq)
	if err != nil {
		t.Fatal(err)
	}
	joinPayload, _ := io.ReadAll(joinResp.Body)
	joinResp.Body.Close()
	assertNoInviteHash(t, joinPayload)
	_ = resp
}

func TestTeamInvalidInviteGenericError(t *testing.T) {
	ensureTeamTestRoutes(t)
	cleanupAuthV2Keys(t)

	sess, _ := registerTeamUser(t, "badinv")
	resp, body := teamJSON(t, http.MethodPost, "/api/teams/join", map[string]string{
		"team_id":     "team_nonexistent",
		"invite_code": "inv_badcode",
	}, sess, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad invite expected 400, got %d body=%s", resp.StatusCode, body)
	}
	var errResp map[string]string
	json.Unmarshal(body, &errResp)
	if errResp["error"] != "invalid team or invite code" {
		t.Fatalf("error = %q; want generic invite message", errResp["error"])
	}
	if strings.Contains(string(body), "nonexistent") {
		t.Fatal("error must not reveal team id")
	}
}

func TestTeamMalformedAndUnknownJSON(t *testing.T) {
	ensureTeamTestRoutes(t)
	cleanupAuthV2Keys(t)

	sess, _ := registerTeamUser(t, "jsonbad")

	malformedReq, _ := http.NewRequest(http.MethodPost, authV2TS.URL+"/api/teams", strings.NewReader(`{`))
	malformedReq.Header.Set("Content-Type", "application/json")
	malformedReq.Header.Set("Cookie", withCookies(sess))
	malformedReq.Header.Set("Origin", authTestOrigin)
	malformedReq.Header.Set("X-CSRF-Token", cookieByName(sess.Cookies(), csrfCookieName).Value)
	malformedResp, _ := http.DefaultClient.Do(malformedReq)
	malformedBody, _ := io.ReadAll(malformedResp.Body)
	malformedResp.Body.Close()
	if malformedResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed json expected 400, got %d body=%s", malformedResp.StatusCode, malformedBody)
	}

	unknownResp, unknownBody := teamJSON(t, http.MethodPost, "/api/teams", map[string]any{
		"name":        "OK",
		"extra_field": "nope",
	}, sess, nil)
	if unknownResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown fields expected 400, got %d body=%s", unknownResp.StatusCode, unknownBody)
	}
}

func TestTeamMustChangeBlocksTeamRoutes(t *testing.T) {
	ensureTeamTestRoutes(t)
	cleanupAuthV2Keys(t)

	username := "mustteam" + strings.Repeat("x", 5)
	registerAuthUser(t, username, "correct horse battery staple")
	tempPassword, err := authV2Srv.authService.ResetPassword(context.Background(), username)
	if err != nil {
		t.Fatal(err)
	}
	loginResp, loginBody := loginAuthUser(t, username, tempPassword, nil)
	if loginResp.StatusCode != http.StatusOK {
		t.Fatal(loginBody)
	}

	resp, body := teamJSON(t, http.MethodGet, "/api/teams", nil, loginResp, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("must-change list teams expected 403, got %d body=%s", resp.StatusCode, body)
	}
}

func TestTeamRemovedMemberImmediately403(t *testing.T) {
	ensureTeamTestRoutes(t)
	cleanupAuthV2Keys(t)

	ownerSess, ownerUser := registerTeamUser(t, "rmowner")
	team, invite := createTeamHTTP(t, ownerSess, "Remove Team")
	teamID, _ := team["id"].(string)
	ownerID := userIDFromAuthResult(t, ownerUser)

	memberSess, _ := registerTeamUser(t, "rmember")
	joinTeamHTTP(t, memberSess, teamID, invite)

	members := listTeamMembersHTTP(t, ownerSess, teamID)
	memberID := memberIDByRole(t, members, "member", ownerID)

	removeResp, _ := teamJSON(t, http.MethodDelete, "/api/teams/"+teamID+"/members/"+memberID, nil, ownerSess, nil)
	if removeResp.StatusCode != http.StatusNoContent {
		t.Fatalf("remove member expected 204, got %d", removeResp.StatusCode)
	}

	probe, probeBody := teamJSON(t, http.MethodGet, "/api/teams/"+teamID+"/members", nil, memberSess, nil)
	if probe.StatusCode != http.StatusForbidden {
		t.Fatalf("removed member expected 403, got %d body=%s", probe.StatusCode, probeBody)
	}
}

func TestTeamCreateIDExhaustionReturns500(t *testing.T) {
	ensureTeamTestRoutes(t)
	cleanupAuthV2Keys(t)

	collisionID, err := auth.NewTeamID()
	if err != nil {
		t.Fatal(err)
	}

	ownerSess, ownerUser := registerTeamUser(t, "exhaust")
	ownerID := userIDFromAuthResult(t, ownerUser)
	store := auth.NewStore(authV2Rdb)
	ctx := context.Background()
	if err := store.CreateTeam(ctx, auth.Team{
		ID: collisionID, Name: "occupied", OwnerUserID: ownerID, CreatedAt: time.Now().UTC(),
	}, "occupied-hash"); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	svc := auth.NewService(store, realClock{})
	svc.SetTeamIDGenerator(func() (string, error) {
		calls.Add(1)
		return collisionID, nil
	})
	authV2Srv.authService = svc

	resp, body := teamJSON(t, http.MethodPost, "/api/teams", map[string]string{"name": "Exhausted"}, ownerSess, nil)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("ID exhaustion expected 500, got %d body=%s", resp.StatusCode, body)
	}
	var errResp map[string]string
	json.Unmarshal(body, &errResp)
	if errResp["error"] != "internal error" {
		t.Fatalf("error = %q; want internal error", errResp["error"])
	}
	if calls.Load() != 5 {
		t.Fatalf("generator calls = %d; want 5", calls.Load())
	}

	authV2Srv.authService = auth.NewService(store, realClock{})
}

func TestProductionMuxOmitsTeamTestRoutes(t *testing.T) {
	srv := NewWithOptions(ServerOptions{
		DataDir:   t.TempDir(),
		PublicURL: "http://localhost:8080",
	})
	handler := srv.authMiddleware(srv.mux)
	req := httptest.NewRequest(http.MethodGet, "/api/teams/team_abc/test-actor", nil)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusNotFound && resp.Code != http.StatusMethodNotAllowed {
		t.Fatalf("production team test route returned %d; want 404 or 405", resp.Code)
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
