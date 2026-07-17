package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func sendTaskV2(t *testing.T, credential, session, teamID string, body map[string]any) (*http.Response, []byte) {
	t.Helper()
	return authJSON(t, http.MethodPost, "/api/teams/"+teamID+"/tasks", body, deviceMsgHeaders(credential, session))
}

func taskStatusV2(t *testing.T, credential, teamID, taskID string) (*http.Response, TaskStatusResponseV2) {
	t.Helper()
	resp, body := authJSON(t, http.MethodGet, "/api/teams/"+teamID+"/tasks/"+taskID, nil, map[string]string{
		"Authorization": "Device " + credential,
	})
	var out TaskStatusResponseV2
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatal(err)
		}
	}
	return resp, out
}

func taskListV2(t *testing.T, credential, session, teamID string) map[string][]TaskStatusResponseV2 {
	t.Helper()
	resp, body := authJSON(t, http.MethodGet, "/api/teams/"+teamID+"/tasks", nil, deviceMsgHeaders(credential, session))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("task list expected 200, got %d body=%s", resp.StatusCode, body)
	}
	var out map[string][]TaskStatusResponseV2
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestV2TaskLifecycle(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	session, result := registerTeamUser(t, "tasklife")
	username := usernameOf(t, result)
	team, _ := createTeamHTTP(t, session, "Task Life Team")
	teamID := team["id"].(string)

	senderCred, senderDev := deviceLoginV2(t, username, "correct horse battery staple", "tlsender")
	targetCred, targetDev := setupTargetAgentV2(t, username, teamID, "tltarget", []string{"main"})

	resp, body := sendTaskV2(t, senderCred, "boss", teamID, map[string]any{
		"to":      targetDev + ":main",
		"task_id": "tlife",
		"content": "build the thing",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("send task expected 200, got %d body=%s", resp.StatusCode, body)
	}
	var sendResp SendResponseV2
	if err := json.Unmarshal(body, &sendResp); err != nil {
		t.Fatal(err)
	}
	if sendResp.TaskID != "tlife" {
		t.Fatalf("task_id = %q; want tlife", sendResp.TaskID)
	}
	verifyIssuedIndexV2(t, teamID, senderDev, "boss", "tlife")

	// Before pull the task is still 'issued'.
	if _, st := taskStatusV2(t, senderCred, teamID, "tlife"); st.Status != "issued" {
		t.Fatalf("status before pull = %q; want issued", st.Status)
	}

	// Target pulls -> the task moves to in_progress.
	pulled := pullInboxV2(t, targetCred, "main", teamID, 0)
	if len(pulled.Items) != 1 || pulled.Items[0].Type != MsgTypeTask || pulled.Items[0].TaskID != "tlife" {
		t.Fatalf("pull items = %+v; want one task tlife", pulled.Items)
	}
	if _, st := taskStatusV2(t, senderCred, teamID, "tlife"); st.Status != "in_progress" {
		t.Fatalf("status after pull = %q; want in_progress", st.Status)
	}

	// Target reports completion.
	if r, b := authJSON(t, http.MethodPost, "/api/teams/"+teamID+"/tasks/tlife/result", map[string]any{
		"status": "completed",
		"result": "done and dusted",
	}, map[string]string{"Authorization": "Device " + targetCred}); r.StatusCode != http.StatusOK {
		t.Fatalf("result expected 200, got %d body=%s", r.StatusCode, b)
	}
	if _, st := taskStatusV2(t, senderCred, teamID, "tlife"); st.Status != "completed" || st.Result != "done and dusted" {
		t.Fatalf("status after result = %+v; want completed/done", st)
	}

	// The issuer is auto-notified in its own inbox.
	report := pullInboxV2(t, senderCred, "boss", teamID, 0)
	if len(report.Items) != 1 {
		t.Fatalf("issuer expected 1 report, got %+v", report.Items)
	}
	if report.Items[0].Content != "completed: done and dusted" {
		t.Fatalf("report content = %q", report.Items[0].Content)
	}

	// Received/issued indexes are cleared after completion.
	if n, _ := authV2Rdb.SCard(context.Background(), receivedTasksV2Key(teamID, targetDev, "main")).Result(); n != 0 {
		t.Fatalf("received index should be empty after completion, got %d", n)
	}
	if n, _ := authV2Rdb.SCard(context.Background(), issuedTasksV2Key(teamID, senderDev, "boss")).Result(); n != 0 {
		t.Fatalf("issued index should be empty after completion, got %d", n)
	}
}

func TestV2TaskFromIdentityComesFromActor(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	session, result := registerTeamUser(t, "taskfromid")
	username := usernameOf(t, result)
	team, _ := createTeamHTTP(t, session, "Task From Team")
	teamID := team["id"].(string)

	senderCred, senderDev := deviceLoginV2(t, username, "correct horse battery staple", "tfsender")
	_, targetDev := setupTargetAgentV2(t, username, teamID, "tftarget", []string{"main"})

	resp, body := sendTaskV2(t, senderCred, "boss", teamID, map[string]any{
		"to":           targetDev + ":main",
		"task_id":      "tfid",
		"content":      "do it",
		"from_device":  "attacker",
		"from_session": "evil",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("send task expected 200, got %d body=%s", resp.StatusCode, body)
	}

	issuedBy, err := authV2Rdb.HGet(context.Background(), taskV2Key(teamID, "tfid"), "issued_by").Result()
	if err != nil {
		t.Fatal(err)
	}
	if issuedBy != senderDev+":boss" {
		t.Fatalf("issued_by = %q; want %s:boss (from actor)", issuedBy, senderDev)
	}
	// The forged identity must not have created an issued index entry.
	if isMember, _ := authV2Rdb.SIsMember(context.Background(), issuedTasksV2Key(teamID, "attacker", "evil"), "tfid").Result(); isMember {
		t.Fatal("forged issuer index entry must not exist")
	}
	verifyIssuedIndexV2(t, teamID, senderDev, "boss", "tfid")
}

func TestV2TaskCannotTargetSessionOutsideTeam(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	session, result := registerTeamUser(t, "taskxsess")
	username := usernameOf(t, result)
	team, _ := createTeamHTTP(t, session, "Task X Sess Team")
	teamID := team["id"].(string)

	senderCred, _ := deviceLoginV2(t, username, "correct horse battery staple", "txsender")
	_, targetDev := setupTargetAgentV2(t, username, teamID, "txtarget", []string{"main"})

	// A session the target never declared -> 404.
	resp, body := sendTaskV2(t, senderCred, "boss", teamID, map[string]any{
		"to":      targetDev + ":ghost",
		"content": "no such session",
	})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown session expected 404, got %d body=%s", resp.StatusCode, body)
	}

	// A device in another team -> 404, no leak.
	other, otherResult := registerTeamUser(t, "taskxother")
	otherTeam, _ := createTeamHTTP(t, other, "Task X Other Team")
	otherTeamID := otherTeam["id"].(string)
	_, otherDev := setupTargetAgentV2(t, usernameOf(t, otherResult), otherTeamID, "txother", []string{"main"})

	resp2, _ := sendTaskV2(t, senderCred, "boss", teamID, map[string]any{
		"to":      otherDev + ":main",
		"content": "cross team",
	})
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-team task target expected 404, got %d", resp2.StatusCode)
	}
}

func TestV2TaskListsAreIsolatedByTeam(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	session, result := registerTeamUser(t, "tasklistiso")
	username := usernameOf(t, result)
	teamA, _ := createTeamHTTP(t, session, "Task List A")
	teamB, _ := createTeamHTTP(t, session, "Task List B")
	teamAID := teamA["id"].(string)
	teamBID := teamB["id"].(string)

	// A sender device valid in both teams (heartbeat so list role passes) and a
	// target only in team A.
	senderCred, senderDev := deviceLoginV2(t, username, "correct horse battery staple", "tliso-sender")
	_, targetDev := setupTargetAgentV2(t, username, teamAID, "tliso-target", []string{"main"})

	if resp, body := sendTaskV2(t, senderCred, "boss", teamAID, map[string]any{
		"to":      targetDev + ":main",
		"task_id": "tliso1",
		"content": "team A task",
	}); resp.StatusCode != http.StatusOK {
		t.Fatalf("send task into A expected 200, got %d body=%s", resp.StatusCode, body)
	}

	// Team A sent list has the task.
	aList := taskListV2(t, senderCred, "boss", teamAID)
	if len(aList["sent"]) != 1 || aList["sent"][0].TaskID != "tliso1" {
		t.Fatalf("team A sent = %+v; want [tliso1]", aList["sent"])
	}
	// Team B sent list for the same device/session is empty.
	bList := taskListV2(t, senderCred, "boss", teamBID)
	if len(bList["sent"]) != 0 {
		t.Fatalf("team B sent must be empty, got %+v", bList["sent"])
	}

	// Target received list is team-scoped too.
	aReceived := taskListV2(t, senderCred, "boss", teamAID)
	_ = aReceived
	if isMember, _ := authV2Rdb.SIsMember(context.Background(), receivedTasksV2Key(teamAID, targetDev, "main"), "tliso1").Result(); !isMember {
		t.Fatal("team A received index missing task")
	}
	if isMember, _ := authV2Rdb.SIsMember(context.Background(), receivedTasksV2Key(teamBID, targetDev, "main"), "tliso1").Result(); isMember {
		t.Fatal("team B received index must not contain team A task")
	}
	_ = senderDev
}

func TestV2TaskBusyRejectsSecondTask(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	session, result := registerTeamUser(t, "taskbusy")
	username := usernameOf(t, result)
	team, _ := createTeamHTTP(t, session, "Task Busy Team")
	teamID := team["id"].(string)

	senderCred, _ := deviceLoginV2(t, username, "correct horse battery staple", "busysender")
	_, targetDev := setupTargetAgentV2(t, username, teamID, "busytarget", []string{"main"})

	if resp, body := sendTaskV2(t, senderCred, "boss", teamID, map[string]any{
		"to":      targetDev + ":main",
		"task_id": "busy1",
		"content": "first",
	}); resp.StatusCode != http.StatusOK {
		t.Fatalf("first task expected 200, got %d body=%s", resp.StatusCode, body)
	}

	// Second task while the first is still issued -> busy 409.
	resp, body := sendTaskV2(t, senderCred, "boss", teamID, map[string]any{
		"to":      targetDev + ":main",
		"task_id": "busy2",
		"content": "second",
	})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("second task expected 409 busy, got %d body=%s", resp.StatusCode, body)
	}

	// A duplicate task_id -> 409 too.
	respDup, _ := sendTaskV2(t, senderCred, "boss", teamID, map[string]any{
		"to":      targetDev + ":main",
		"task_id": "busy1",
		"content": "dup",
	})
	if respDup.StatusCode != http.StatusConflict {
		t.Fatalf("dup task expected 409, got %d", respDup.StatusCode)
	}
}
