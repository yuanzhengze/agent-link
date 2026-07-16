package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func heartbeatWebV2(t *testing.T, session *http.Response, teamID string) (*http.Response, []byte) {
	t.Helper()
	return teamJSON(t, http.MethodPost, "/api/teams/"+teamID+"/agents/heartbeat", nil, session, nil)
}

func heartbeatDeviceV2(t *testing.T, credential, teamID string) (*http.Response, []byte) {
	t.Helper()
	return authJSON(t, http.MethodPost, "/api/teams/"+teamID+"/agents/heartbeat", nil, map[string]string{
		"Authorization": "Device " + credential,
	})
}

func listAgentsV2(t *testing.T, session *http.Response, teamID string) AgentListResponseV2 {
	t.Helper()
	resp, body := teamJSON(t, http.MethodGet, "/api/teams/"+teamID+"/agents", nil, session, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list agents expected 200, got %d body=%s", resp.StatusCode, body)
	}
	var list AgentListResponseV2
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatal(err)
	}
	return list
}

func TestV2HeartbeatRecordsActorDeviceInTeam(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	session, result := registerTeamUser(t, "hbrec")
	userID := userIDFromAuthResult(t, result)
	team, _ := createTeamHTTP(t, session, "HB Rec Team")
	teamID := team["id"].(string)

	resp, body := heartbeatWebV2(t, session, teamID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("heartbeat expected 200, got %d body=%s", resp.StatusCode, body)
	}

	ctx := context.Background()
	member := teamDeviceMember(userID, "web")
	if isMem, err := authV2Rdb.SIsMember(ctx, teamDevicesV2Key(teamID), member).Result(); err != nil || !isMem {
		t.Fatalf("device not indexed for team: isMember=%v err=%v", isMem, err)
	}
	data, err := authV2Rdb.HGetAll(ctx, deviceV2Key(teamID, userID, "web")).Result()
	if err != nil {
		t.Fatal(err)
	}
	if data["user_id"] != userID || data["last_seen"] == "" || data["client_type"] != "web" {
		t.Fatalf("presence hash missing/incorrect: %v", data)
	}
}

func TestV2BrowserActorAppearsAsWebGUI(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	session, result := registerTeamUser(t, "hbweb")
	userID := userIDFromAuthResult(t, result)
	team, _ := createTeamHTTP(t, session, "HB Web Team")
	teamID := team["id"].(string)

	if resp, body := heartbeatWebV2(t, session, teamID); resp.StatusCode != http.StatusOK {
		t.Fatalf("web heartbeat expected 200, got %d body=%s", resp.StatusCode, body)
	}

	list := listAgentsV2(t, session, teamID)
	var found *AgentInfoV2
	for i := range list.Agents {
		if list.Agents[i].DeviceID == "web" && list.Agents[i].UserID == userID {
			found = &list.Agents[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("web GUI agent not listed: %+v", list.Agents)
	}
	if found.ClientType != "web" || found.DeviceName != "web" {
		t.Fatalf("web agent identity wrong: %+v", found)
	}
	if !found.Online {
		t.Fatalf("just-heartbeated web agent should be online: %+v", found)
	}
	if len(found.Sessions) != 1 || found.Sessions[0] != "gui" {
		t.Fatalf("web agent sessions = %v; want [gui]", found.Sessions)
	}
}

func TestV2AgentListOnlyShowsTeamMembersDevices(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	owner, ownerResult := registerTeamUser(t, "hblistA")
	teamA, inviteA := createTeamHTTP(t, owner, "List Team A")
	teamAID := teamA["id"].(string)
	ownerID := userIDFromAuthResult(t, ownerResult)

	member, memberResult := registerTeamUser(t, "hblistM")
	joinTeamHTTP(t, member, teamAID, inviteA)
	memberID := userIDFromAuthResult(t, memberResult)

	if resp, body := heartbeatWebV2(t, owner, teamAID); resp.StatusCode != http.StatusOK {
		t.Fatalf("owner heartbeat expected 200, got %d body=%s", resp.StatusCode, body)
	}
	if resp, body := heartbeatWebV2(t, member, teamAID); resp.StatusCode != http.StatusOK {
		t.Fatalf("member heartbeat expected 200, got %d body=%s", resp.StatusCode, body)
	}

	// A separate team B whose device must never appear in team A's list.
	outsider, outsiderResult := registerTeamUser(t, "hblistB")
	teamB, _ := createTeamHTTP(t, outsider, "List Team B")
	teamBID := teamB["id"].(string)
	outsiderID := userIDFromAuthResult(t, outsiderResult)
	if resp, body := heartbeatWebV2(t, outsider, teamBID); resp.StatusCode != http.StatusOK {
		t.Fatalf("outsider heartbeat expected 200, got %d body=%s", resp.StatusCode, body)
	}

	list := listAgentsV2(t, owner, teamAID)
	ids := map[string]bool{}
	for _, a := range list.Agents {
		ids[a.UserID] = true
	}
	if !ids[ownerID] || !ids[memberID] {
		t.Fatalf("team A list must include owner+member: %+v", list.Agents)
	}
	if ids[outsiderID] {
		t.Fatalf("team A list must not include team B device: %+v", list.Agents)
	}
	if len(list.Agents) != 2 {
		t.Fatalf("team A list expected 2 agents, got %d: %+v", len(list.Agents), list.Agents)
	}
}

func TestV2AgentCannotMutateOtherUsersDevice(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	a, aResult := registerTeamUser(t, "hbmutA")
	team, invite := createTeamHTTP(t, a, "Mutate Team")
	teamID := team["id"].(string)
	aUserID := userIDFromAuthResult(t, aResult)
	aUsername := aResult["user"].(map[string]any)["username"].(string)

	b, bResult := registerTeamUser(t, "hbmutB")
	joinTeamHTTP(t, b, teamID, invite)
	bUserID := userIDFromAuthResult(t, bResult)
	bUsername := bResult["user"].(map[string]any)["username"].(string)

	credA, devA := deviceLoginV2(t, aUsername, "correct horse battery staple", "mut-a")
	credB, devB := deviceLoginV2(t, bUsername, "correct horse battery staple", "mut-b")

	if resp, body := heartbeatDeviceV2(t, credA, teamID); resp.StatusCode != http.StatusOK {
		t.Fatalf("A heartbeat expected 200, got %d body=%s", resp.StatusCode, body)
	}
	if resp, body := heartbeatDeviceV2(t, credB, teamID); resp.StatusCode != http.StatusOK {
		t.Fatalf("B heartbeat expected 200, got %d body=%s", resp.StatusCode, body)
	}

	// B sets its own sessions first.
	if resp, body := authJSON(t, http.MethodPatch, "/api/teams/"+teamID+"/agents/sessions", map[string]any{
		"sessions": []string{"solo"},
	}, map[string]string{"Authorization": "Device " + credB}); resp.StatusCode != http.StatusOK {
		t.Fatalf("B patch expected 200, got %d body=%s", resp.StatusCode, body)
	}

	// A patches its own device but smuggles B's device_id in the body; it must
	// have no effect on B's device.
	if resp, body := authJSON(t, http.MethodPatch, "/api/teams/"+teamID+"/agents/sessions", map[string]any{
		"sessions":  []string{"main", "worker"},
		"device_id": devB,
		"user_id":   bUserID,
	}, map[string]string{"Authorization": "Device " + credA}); resp.StatusCode != http.StatusOK {
		t.Fatalf("A patch expected 200, got %d body=%s", resp.StatusCode, body)
	}

	ctx := context.Background()
	aSessions, _ := authV2Rdb.SMembers(ctx, deviceSessionsV2Key(teamID, aUserID, devA)).Result()
	bSessions, _ := authV2Rdb.SMembers(ctx, deviceSessionsV2Key(teamID, bUserID, devB)).Result()
	if !equalStringSet(aSessions, []string{"main", "worker"}) {
		t.Fatalf("A sessions = %v; want [main worker]", aSessions)
	}
	if !equalStringSet(bSessions, []string{"solo"}) {
		t.Fatalf("B sessions were mutated by A: %v; want [solo]", bSessions)
	}
}

func TestV2SessionPatchAndDeleteFlow(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	user, result := registerTeamUser(t, "hbflow")
	team, _ := createTeamHTTP(t, user, "Flow Team")
	teamID := team["id"].(string)
	username := result["user"].(map[string]any)["username"].(string)
	credential, deviceID := deviceLoginV2(t, username, "correct horse battery staple", "flow-dev")
	userID := userIDFromAuthResult(t, result)
	deviceHdr := map[string]string{"Authorization": "Device " + credential}

	// PATCH before heartbeat -> 404.
	if resp, _ := authJSON(t, http.MethodPatch, "/api/teams/"+teamID+"/agents/sessions", map[string]any{
		"sessions": []string{"main"},
	}, deviceHdr); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("patch before heartbeat expected 404, got %d", resp.StatusCode)
	}

	if resp, body := heartbeatDeviceV2(t, credential, teamID); resp.StatusCode != http.StatusOK {
		t.Fatalf("heartbeat expected 200, got %d body=%s", resp.StatusCode, body)
	}

	// PATCH sets sessions.
	if resp, body := authJSON(t, http.MethodPatch, "/api/teams/"+teamID+"/agents/sessions", map[string]any{
		"sessions": []string{"main", "worker", "reviewer"},
	}, deviceHdr); resp.StatusCode != http.StatusOK {
		t.Fatalf("patch expected 200, got %d body=%s", resp.StatusCode, body)
	}
	ctx := context.Background()
	if got, _ := authV2Rdb.SMembers(ctx, deviceSessionsV2Key(teamID, userID, deviceID)).Result(); !equalStringSet(got, []string{"main", "worker", "reviewer"}) {
		t.Fatalf("sessions after patch = %v", got)
	}

	// DELETE one session.
	if resp, body := authJSON(t, http.MethodDelete, "/api/teams/"+teamID+"/agents/sessions?name=reviewer", nil, deviceHdr); resp.StatusCode != http.StatusOK {
		t.Fatalf("delete expected 200, got %d body=%s", resp.StatusCode, body)
	}
	if got, _ := authV2Rdb.SMembers(ctx, deviceSessionsV2Key(teamID, userID, deviceID)).Result(); !equalStringSet(got, []string{"main", "worker"}) {
		t.Fatalf("sessions after delete = %v", got)
	}

	// DELETE unknown session -> 404.
	if resp, _ := authJSON(t, http.MethodDelete, "/api/teams/"+teamID+"/agents/sessions?name=ghost", nil, deviceHdr); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("delete unknown expected 404, got %d", resp.StatusCode)
	}

	// Reduce to a single session, then deleting the last must fail with 400.
	if resp, _ := authJSON(t, http.MethodDelete, "/api/teams/"+teamID+"/agents/sessions?name=worker", nil, deviceHdr); resp.StatusCode != http.StatusOK {
		t.Fatalf("delete worker expected 200, got %d", resp.StatusCode)
	}
	if resp, _ := authJSON(t, http.MethodDelete, "/api/teams/"+teamID+"/agents/sessions?name=main", nil, deviceHdr); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("delete last session expected 400, got %d", resp.StatusCode)
	}
}

func TestV2WebActorCannotPatchSessions(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	session, _ := registerTeamUser(t, "hbwebpatch")
	team, _ := createTeamHTTP(t, session, "Web Patch Team")
	teamID := team["id"].(string)
	if resp, body := heartbeatWebV2(t, session, teamID); resp.StatusCode != http.StatusOK {
		t.Fatalf("web heartbeat expected 200, got %d body=%s", resp.StatusCode, body)
	}

	resp, body := teamJSON(t, http.MethodPatch, "/api/teams/"+teamID+"/agents/sessions", map[string]any{
		"sessions": []string{"main"},
	}, session, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("web patch sessions expected 400, got %d body=%s", resp.StatusCode, body)
	}
}

func equalStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, v := range a {
		seen[v]++
	}
	for _, v := range b {
		seen[v]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}
