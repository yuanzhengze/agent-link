package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
)

func usernameOf(t *testing.T, result map[string]any) string {
	t.Helper()
	user, ok := result["user"].(map[string]any)
	if !ok {
		t.Fatal("auth result missing user")
	}
	name, _ := user["username"].(string)
	if name == "" {
		t.Fatal("auth result missing username")
	}
	return name
}

func deviceMsgHeaders(credential, session string) map[string]string {
	return map[string]string{
		"Authorization":       "Device " + credential,
		"X-Agentlink-Session": session,
	}
}

// patchDeviceSessionsV2 declares a device's agent sessions so it is a valid
// message/task target.
func patchDeviceSessionsV2(t *testing.T, credential, teamID string, sessions []string) {
	t.Helper()
	resp, body := authJSON(t, http.MethodPatch, "/api/teams/"+teamID+"/agents/sessions", map[string]any{
		"sessions": sessions,
	}, map[string]string{"Authorization": "Device " + credential})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch sessions expected 200, got %d body=%s", resp.StatusCode, body)
	}
}

// setupTargetAgentV2 logs a user in on a fresh device, heartbeats it into the
// team, and declares its sessions so it can receive messages/tasks.
func setupTargetAgentV2(t *testing.T, username, teamID, deviceName string, sessions []string) (credential, deviceID string) {
	t.Helper()
	credential, deviceID = deviceLoginV2(t, username, "correct horse battery staple", deviceName)
	if resp, body := heartbeatDeviceV2(t, credential, teamID); resp.StatusCode != http.StatusOK {
		t.Fatalf("target heartbeat expected 200, got %d body=%s", resp.StatusCode, body)
	}
	patchDeviceSessionsV2(t, credential, teamID, sessions)
	return credential, deviceID
}

func sendMessageV2(t *testing.T, credential, session, teamID string, body map[string]any) (*http.Response, []byte) {
	t.Helper()
	return authJSON(t, http.MethodPost, "/api/teams/"+teamID+"/messages", body, deviceMsgHeaders(credential, session))
}

func pullInboxV2(t *testing.T, credential, session, teamID string, limit int) PullResponseV2 {
	t.Helper()
	path := "/api/teams/" + teamID + "/inbox"
	if limit > 0 {
		path += "?limit=" + strconv.Itoa(limit)
	}
	resp, body := authJSON(t, http.MethodGet, path, nil, deviceMsgHeaders(credential, session))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pull inbox expected 200, got %d body=%s", resp.StatusCode, body)
	}
	var out PullResponseV2
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestV2MessageSendAndPull(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	session, result := registerTeamUser(t, "msgsend")
	username := usernameOf(t, result)
	team, _ := createTeamHTTP(t, session, "Msg Send Team")
	teamID := team["id"].(string)

	senderCred, senderDev := deviceLoginV2(t, username, "correct horse battery staple", "sender")
	targetCred, targetDev := setupTargetAgentV2(t, username, teamID, "target", []string{"main"})

	resp, body := sendMessageV2(t, senderCred, "boss", teamID, map[string]any{
		"to":      targetDev + ":main",
		"content": "hello there",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("send expected 200, got %d body=%s", resp.StatusCode, body)
	}

	pulled := pullInboxV2(t, targetCred, "main", teamID, 0)
	if len(pulled.Items) != 1 {
		t.Fatalf("expected 1 inbox item, got %d: %+v", len(pulled.Items), pulled.Items)
	}
	item := pulled.Items[0]
	if item.Content != "hello there" {
		t.Fatalf("content = %q; want %q", item.Content, "hello there")
	}
	if item.FromDevice != senderDev || item.FromSession != "boss" {
		t.Fatalf("from = %s:%s; want %s:boss (from actor, not body)", item.FromDevice, item.FromSession, senderDev)
	}

	// A second pull drains nothing.
	if again := pullInboxV2(t, targetCred, "main", teamID, 0); len(again.Items) != 0 {
		t.Fatalf("second pull expected empty, got %+v", again.Items)
	}
}

func TestV2MessageCannotTargetDeviceOutsideTeam(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	// Team A sender.
	aSession, aResult := registerTeamUser(t, "msgxteamA")
	aUsername := usernameOf(t, aResult)
	teamA, _ := createTeamHTTP(t, aSession, "Msg X Team A")
	teamAID := teamA["id"].(string)
	senderCred, _ := deviceLoginV2(t, aUsername, "correct horse battery staple", "xsender")

	// Team B target (a real device, but in another team).
	bSession, bResult := registerTeamUser(t, "msgxteamB")
	bUsername := usernameOf(t, bResult)
	teamB, _ := createTeamHTTP(t, bSession, "Msg X Team B")
	teamBID := teamB["id"].(string)
	_, targetDev := setupTargetAgentV2(t, bUsername, teamBID, "xtarget", []string{"main"})

	// A device that belongs to another team is indistinguishable from a
	// nonexistent one: both 404, so team membership is never leaked.
	resp, body := sendMessageV2(t, senderCred, "boss", teamAID, map[string]any{
		"to":      targetDev + ":main",
		"content": "leak?",
	})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-team target expected 404, got %d body=%s", resp.StatusCode, body)
	}

	resp2, _ := sendMessageV2(t, senderCred, "boss", teamAID, map[string]any{
		"to":      "d_doesnotexist:main",
		"content": "leak?",
	})
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("nonexistent target expected 404, got %d", resp2.StatusCode)
	}
}

func TestV2InboxIsIsolatedByTeam(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	session, result := registerTeamUser(t, "inboxiso")
	username := usernameOf(t, result)
	teamA, _ := createTeamHTTP(t, session, "Inbox Iso A")
	teamB, _ := createTeamHTTP(t, session, "Inbox Iso B")
	teamAID := teamA["id"].(string)
	teamBID := teamB["id"].(string)

	// Same physical device, present in both teams.
	targetCred, targetDev := deviceLoginV2(t, username, "correct horse battery staple", "isodev")
	for _, teamID := range []string{teamAID, teamBID} {
		if resp, body := heartbeatDeviceV2(t, targetCred, teamID); resp.StatusCode != http.StatusOK {
			t.Fatalf("heartbeat expected 200, got %d body=%s", resp.StatusCode, body)
		}
		patchDeviceSessionsV2(t, targetCred, teamID, []string{"main"})
	}
	senderCred, _ := deviceLoginV2(t, username, "correct horse battery staple", "isosender")

	// Deliver a message only into team A.
	if resp, body := sendMessageV2(t, senderCred, "boss", teamAID, map[string]any{
		"to":      targetDev + ":main",
		"content": "team A only",
	}); resp.StatusCode != http.StatusOK {
		t.Fatalf("send into A expected 200, got %d body=%s", resp.StatusCode, body)
	}

	// Team B inbox for the same device/session must stay empty.
	if b := pullInboxV2(t, targetCred, "main", teamBID, 0); len(b.Items) != 0 {
		t.Fatalf("team B inbox must be empty, got %+v", b.Items)
	}
	// Team A inbox holds the message.
	if a := pullInboxV2(t, targetCred, "main", teamAID, 0); len(a.Items) != 1 {
		t.Fatalf("team A inbox expected 1 item, got %+v", a.Items)
	}
}

func TestV2MessageFromIdentityComesFromActor(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	session, result := registerTeamUser(t, "msgfromid")
	username := usernameOf(t, result)
	team, _ := createTeamHTTP(t, session, "Msg From Team")
	teamID := team["id"].(string)

	senderCred, senderDev := deviceLoginV2(t, username, "correct horse battery staple", "realsender")
	targetCred, targetDev := setupTargetAgentV2(t, username, teamID, "fromtarget", []string{"main"})

	// Smuggle a forged from identity; it must be ignored in favor of the actor.
	resp, body := sendMessageV2(t, senderCred, "boss", teamID, map[string]any{
		"to":           targetDev + ":main",
		"content":      "who am I",
		"from_device":  "attacker",
		"from_session": "evil",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("send expected 200, got %d body=%s", resp.StatusCode, body)
	}

	pulled := pullInboxV2(t, targetCred, "main", teamID, 0)
	if len(pulled.Items) != 1 {
		t.Fatalf("expected 1 item, got %+v", pulled.Items)
	}
	if pulled.Items[0].FromDevice != senderDev || pulled.Items[0].FromSession != "boss" {
		t.Fatalf("from = %s:%s; want %s:boss", pulled.Items[0].FromDevice, pulled.Items[0].FromSession, senderDev)
	}
}

func TestV2MessageRejectsEmptyContent(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	session, result := registerTeamUser(t, "msgempty")
	username := usernameOf(t, result)
	team, _ := createTeamHTTP(t, session, "Msg Empty Team")
	teamID := team["id"].(string)
	senderCred, _ := deviceLoginV2(t, username, "correct horse battery staple", "emptysender")
	_, targetDev := setupTargetAgentV2(t, username, teamID, "emptytarget", []string{"main"})

	resp, _ := sendMessageV2(t, senderCred, "boss", teamID, map[string]any{
		"to":      targetDev + ":main",
		"content": "",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty content expected 400, got %d", resp.StatusCode)
	}
}

// verifyIssuedIndexV2 is a small assertion helper used by task tests.
func verifyIssuedIndexV2(t *testing.T, teamID, deviceID, session, taskID string) {
	t.Helper()
	isMember, err := authV2Rdb.SIsMember(context.Background(), issuedTasksV2Key(teamID, deviceID, session), taskID).Result()
	if err != nil || !isMember {
		t.Fatalf("issued index missing task %s for %s:%s (member=%v err=%v)", taskID, deviceID, session, isMember, err)
	}
}
