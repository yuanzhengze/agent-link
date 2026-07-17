package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// deviceLoginV2 registers nothing; it logs an existing user in on a new device
// and returns the device credential plus the resolved device id.
func deviceLoginV2(t *testing.T, username, password, deviceName string) (credential, deviceID string) {
	t.Helper()
	resp, body := authJSON(t, http.MethodPost, "/api/auth/device-login", map[string]string{
		"username":    username,
		"password":    password,
		"device_name": deviceName,
	}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("device-login expected 200, got %d body=%s", resp.StatusCode, body)
	}
	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	credential, _ = result["device_credential"].(string)
	deviceID, _ = result["device_id"].(string)
	if credential == "" || deviceID == "" {
		t.Fatalf("device-login response missing credential/device_id: %s", body)
	}
	return credential, deviceID
}

// acquireLockV2 issues a device/explicit-header acquire (no cookie/CSRF).
func acquireLockV2(t *testing.T, headers map[string]string, teamID string, body map[string]any) (*http.Response, []byte) {
	t.Helper()
	return authJSON(t, http.MethodPost, "/api/teams/"+teamID+"/locks/acquire", body, headers)
}

// acquireLockWebV2 issues a Web GUI acquire carrying the session cookie + CSRF.
func acquireLockWebV2(t *testing.T, session *http.Response, teamID string, body map[string]any) (*http.Response, []byte) {
	t.Helper()
	return teamJSON(t, http.MethodPost, "/api/teams/"+teamID+"/locks/acquire", body, session, nil)
}

func TestV2LocksSamePathAreIndependentAcrossTeams(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	session, _ := registerTeamUser(t, "lockindepteams")
	teamA, _ := createTeamHTTP(t, session, "Lock Team A")
	teamB, _ := createTeamHTTP(t, session, "Lock Team B")
	teamAID := teamA["id"].(string)
	teamBID := teamB["id"].(string)
	projectA := createProjectV2HTTP(t, session, teamAID, "Project A")
	projectB := createProjectV2HTTP(t, session, teamBID, "Project B")

	respA, bodyA := acquireLockWebV2(t, session, teamAID, map[string]any{
		"project_id": projectA.ID,
		"path":       "index.html",
	})
	if respA.StatusCode != http.StatusOK {
		t.Fatalf("team A acquire expected 200, got %d body=%s", respA.StatusCode, bodyA)
	}
	respB, bodyB := acquireLockWebV2(t, session, teamBID, map[string]any{
		"project_id": projectB.ID,
		"path":       "index.html",
	})
	if respB.StatusCode != http.StatusOK {
		t.Fatalf("team B acquire expected 200 (independent), got %d body=%s", respB.StatusCode, bodyB)
	}

	// The two locks are stored under team-scoped keys and do not collide.
	existsA, err := authV2Rdb.Exists(context.Background(), lockV2Key(teamAID, projectA.ID, "index.html")).Result()
	if err != nil || existsA != 1 {
		t.Fatalf("team A lock key exists=%d err=%v; want 1", existsA, err)
	}
	existsB, err := authV2Rdb.Exists(context.Background(), lockV2Key(teamBID, projectB.ID, "index.html")).Result()
	if err != nil || existsB != 1 {
		t.Fatalf("team B lock key exists=%d err=%v; want 1", existsB, err)
	}
}

func TestV2LockOwnerUsesAuthenticatedActor(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	session, userResult := registerTeamUser(t, "lockowneractor")
	team, _ := createTeamHTTP(t, session, "Lock Owner Team")
	teamID := team["id"].(string)
	project := createProjectV2HTTP(t, session, teamID, "Owner Project")
	username := userResult["user"].(map[string]any)["username"].(string)

	// A malicious owner/user_id in the body must be ignored: ownership comes
	// solely from the authenticated actor.
	resp, body := acquireLockWebV2(t, session, teamID, map[string]any{
		"project_id":   project.ID,
		"path":         "index.html",
		"owner":        "attacker:evil",
		"user_id":      "u_attacker",
		"username":     "attacker",
		"session_name": "evil",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("acquire expected 200, got %d body=%s", resp.StatusCode, body)
	}
	var acquired LockAcquireResponseV2
	if err := json.Unmarshal(body, &acquired); err != nil {
		t.Fatal(err)
	}
	if acquired.Owner.Username != username {
		t.Fatalf("owner username = %q; want authenticated %q", acquired.Owner.Username, username)
	}
	if acquired.Owner.DeviceID != "web" || acquired.Owner.SessionName != "gui" {
		t.Fatalf("web owner = %+v; want device web / session gui", acquired.Owner)
	}

	stored, err := authV2Rdb.HGetAll(context.Background(), lockV2Key(teamID, project.ID, "index.html")).Result()
	if err != nil {
		t.Fatal(err)
	}
	if stored["username"] != username || stored["session_name"] != "gui" {
		t.Fatalf("stored owner ignored actor: %v", stored)
	}
	if stored["username"] == "attacker" || stored["session_name"] == "evil" {
		t.Fatalf("stored owner honored malicious body: %v", stored)
	}
}

func TestV2LockRejectsProjectFromOtherTeam(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	owner, _ := registerTeamUser(t, "lockotherowner")
	teamA, _ := createTeamHTTP(t, owner, "Lock Cross A")
	teamAID := teamA["id"].(string)
	project := createProjectV2HTTP(t, owner, teamAID, "Cross Project")

	outsider, _ := registerTeamUser(t, "lockotherout")
	teamB, _ := createTeamHTTP(t, outsider, "Lock Cross B")
	teamBID := teamB["id"].(string)

	// Outsider tries to lock team A's project through their own team route.
	resp, body := acquireLockWebV2(t, outsider, teamBID, map[string]any{
		"project_id": project.ID,
		"path":       "index.html",
	})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-team lock expected 404, got %d body=%s", resp.StatusCode, body)
	}
	exists, err := authV2Rdb.Exists(context.Background(), lockV2Key(teamBID, project.ID, "index.html")).Result()
	if err != nil || exists != 0 {
		t.Fatalf("cross-team lock must not be created: exists=%d err=%v", exists, err)
	}
}

func TestV2WebLockUsesWebGUISession(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	session, _ := registerTeamUser(t, "lockwebgui")
	team, _ := createTeamHTTP(t, session, "Web GUI Lock")
	teamID := team["id"].(string)
	project := createProjectV2HTTP(t, session, teamID, "Web GUI Project")

	resp, body := acquireLockWebV2(t, session, teamID, map[string]any{
		"project_id": project.ID,
		"path":       "index.html",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("web acquire expected 200, got %d body=%s", resp.StatusCode, body)
	}
	var acquired LockAcquireResponseV2
	if err := json.Unmarshal(body, &acquired); err != nil {
		t.Fatal(err)
	}
	if acquired.Owner.DeviceID != "web" || acquired.Owner.DeviceName != "web" || acquired.Owner.SessionName != "gui" {
		t.Fatalf("web owner = %+v; want web/web/gui", acquired.Owner)
	}

	// A second web acquire from the same session is an idempotent refresh.
	refresh, refreshBody := acquireLockWebV2(t, session, teamID, map[string]any{
		"project_id": project.ID,
		"path":       "index.html",
	})
	if refresh.StatusCode != http.StatusOK {
		t.Fatalf("web refresh expected 200, got %d body=%s", refresh.StatusCode, refreshBody)
	}
}

func TestV2DeviceLockRequiresValidSessionHeader(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	session, userResult := registerTeamUser(t, "lockdevsession")
	team, _ := createTeamHTTP(t, session, "Device Lock Team")
	teamID := team["id"].(string)
	project := createProjectV2HTTP(t, session, teamID, "Device Lock Project")
	username := userResult["user"].(map[string]any)["username"].(string)
	credential, deviceID := deviceLoginV2(t, username, "correct horse battery staple", "lock-device")

	// Missing session header -> 400.
	missingResp, missingBody := acquireLockV2(t, map[string]string{
		"Authorization": "Device " + credential,
	}, teamID, map[string]any{
		"project_id": project.ID,
		"path":       "index.html",
	})
	if missingResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing session expected 400, got %d body=%s", missingResp.StatusCode, missingBody)
	}

	// Invalid session header -> 400.
	invalidResp, invalidBody := acquireLockV2(t, map[string]string{
		"Authorization":       "Device " + credential,
		"X-Agentlink-Session": "Bad Session!",
	}, teamID, map[string]any{
		"project_id": project.ID,
		"path":       "index.html",
	})
	if invalidResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid session expected 400, got %d body=%s", invalidResp.StatusCode, invalidBody)
	}

	// Valid session header -> 200, owner reflects device + session.
	validResp, validBody := acquireLockV2(t, map[string]string{
		"Authorization":       "Device " + credential,
		"X-Agentlink-Session": "agent1",
	}, teamID, map[string]any{
		"project_id": project.ID,
		"path":       "index.html",
	})
	if validResp.StatusCode != http.StatusOK {
		t.Fatalf("valid session expected 200, got %d body=%s", validResp.StatusCode, validBody)
	}
	var acquired LockAcquireResponseV2
	if err := json.Unmarshal(validBody, &acquired); err != nil {
		t.Fatal(err)
	}
	if acquired.Owner.DeviceID != deviceID || acquired.Owner.SessionName != "agent1" {
		t.Fatalf("device owner = %+v; want device %q session agent1", acquired.Owner, deviceID)
	}
}

func TestV2LockReleaseAndConflict(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	holder, holderResult := registerTeamUser(t, "lockrelholder")
	team, invite := createTeamHTTP(t, holder, "Lock Release Team")
	teamID := team["id"].(string)
	project := createProjectV2HTTP(t, holder, teamID, "Release Project")

	other, otherResult := registerTeamUser(t, "lockrelother")
	joinTeamHTTP(t, other, teamID, invite)
	_ = holderResult
	_ = otherResult

	// Holder acquires.
	resp, body := acquireLockWebV2(t, holder, teamID, map[string]any{
		"project_id": project.ID,
		"path":       "index.html",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("holder acquire expected 200, got %d body=%s", resp.StatusCode, body)
	}

	// Another member cannot acquire -> 409 with holder shown.
	confResp, confBody := authJSON(t, http.MethodPost, "/api/teams/"+teamID+"/locks/acquire", map[string]any{
		"project_id": project.ID,
		"path":       "index.html",
	}, map[string]string{
		"Cookie":       withCookies(other),
		"Origin":       authTestOrigin,
		"X-CSRF-Token": cookieByName(other.Cookies(), csrfCookieName).Value,
	})
	if confResp.StatusCode != http.StatusConflict {
		t.Fatalf("conflict acquire expected 409, got %d body=%s", confResp.StatusCode, confBody)
	}
	var conflict LockConflictResponseV2
	if err := json.Unmarshal(confBody, &conflict); err != nil {
		t.Fatal(err)
	}
	if conflict.Owner.Username == "" {
		t.Fatalf("conflict must expose current holder: %s", confBody)
	}

	// Other member's normal release is rejected (not the holder).
	relResp, relBody := authJSON(t, http.MethodPost, "/api/teams/"+teamID+"/locks/release", map[string]any{
		"project_id": project.ID,
		"path":       "index.html",
	}, map[string]string{
		"Cookie":       withCookies(other),
		"Origin":       authTestOrigin,
		"X-CSRF-Token": cookieByName(other.Cookies(), csrfCookieName).Value,
	})
	if relResp.StatusCode != http.StatusConflict {
		t.Fatalf("non-holder release expected 409, got %d body=%s", relResp.StatusCode, relBody)
	}

	// Holder releases successfully.
	okResp, okBody := authJSON(t, http.MethodPost, "/api/teams/"+teamID+"/locks/release", map[string]any{
		"project_id": project.ID,
		"path":       "index.html",
	}, map[string]string{
		"Cookie":       withCookies(holder),
		"Origin":       authTestOrigin,
		"X-CSRF-Token": cookieByName(holder.Cookies(), csrfCookieName).Value,
	})
	if okResp.StatusCode != http.StatusOK {
		t.Fatalf("holder release expected 200, got %d body=%s", okResp.StatusCode, okBody)
	}
	exists, err := authV2Rdb.Exists(context.Background(), lockV2Key(teamID, project.ID, "index.html")).Result()
	if err != nil || exists != 0 {
		t.Fatalf("released lock should be gone: exists=%d err=%v", exists, err)
	}
}

func TestV2LockExpiredLeaseIsReclaimedAndOldOwnerSetCleaned(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	holder, holderResult := registerTeamUser(t, "lockreclaimA")
	team, invite := createTeamHTTP(t, holder, "Reclaim Team")
	teamID := team["id"].(string)
	project := createProjectV2HTTP(t, holder, teamID, "Reclaim Project")
	holderUsername := holderResult["user"].(map[string]any)["username"].(string)

	reclaimer, reclaimerResult := registerTeamUser(t, "lockreclaimB")
	joinTeamHTTP(t, reclaimer, teamID, invite)
	reclaimerUsername := reclaimerResult["user"].(map[string]any)["username"].(string)

	resp, body := acquireLockWebV2(t, holder, teamID, map[string]any{
		"project_id": project.ID,
		"path":       "index.html",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("holder acquire expected 200, got %d body=%s", resp.StatusCode, body)
	}

	// Force the lease to have already expired so it can be reclaimed.
	key := lockV2Key(teamID, project.ID, "index.html")
	holderSet := authV2Rdb.HGet(context.Background(), key, "owner_set").Val()
	if err := authV2Rdb.HSet(context.Background(), key, "lease_expires_at", "1").Err(); err != nil {
		t.Fatal(err)
	}

	reResp, reBody := acquireLockWebV2(t, reclaimer, teamID, map[string]any{
		"project_id": project.ID,
		"path":       "index.html",
	})
	if reResp.StatusCode != http.StatusOK {
		t.Fatalf("expired lease reclaim expected 200, got %d body=%s", reResp.StatusCode, reBody)
	}
	var acquired LockAcquireResponseV2
	if err := json.Unmarshal(reBody, &acquired); err != nil {
		t.Fatal(err)
	}
	if acquired.Owner.Username != reclaimerUsername {
		t.Fatalf("reclaimed owner = %q; want %q", acquired.Owner.Username, reclaimerUsername)
	}

	// The previous holder's locks set must no longer reference the member.
	member := lockMember(project.ID, "index.html")
	stillMember, err := authV2Rdb.SIsMember(context.Background(), holderSet, member).Result()
	if err != nil {
		t.Fatal(err)
	}
	if stillMember {
		t.Fatalf("old owner set %q still holds member %q after reclaim", holderSet, member)
	}
	if stored := authV2Rdb.HGet(context.Background(), key, "username").Val(); stored == holderUsername {
		t.Fatalf("lock hash still owned by old holder %q", holderUsername)
	}
}

func TestV2LockForceReleaseByAnotherMember(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	holder, _ := registerTeamUser(t, "lockforceA")
	team, invite := createTeamHTTP(t, holder, "Force Team")
	teamID := team["id"].(string)
	project := createProjectV2HTTP(t, holder, teamID, "Force Project")

	other, _ := registerTeamUser(t, "lockforceB")
	joinTeamHTTP(t, other, teamID, invite)

	resp, body := acquireLockWebV2(t, holder, teamID, map[string]any{
		"project_id": project.ID,
		"path":       "index.html",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("holder acquire expected 200, got %d body=%s", resp.StatusCode, body)
	}
	holderSet := authV2Rdb.HGet(context.Background(), lockV2Key(teamID, project.ID, "index.html"), "owner_set").Val()

	// Non-holder force release succeeds and cleans the actual holder's set.
	forceResp, forceBody := teamJSON(t, http.MethodPost, "/api/teams/"+teamID+"/locks/release", map[string]any{
		"project_id": project.ID,
		"path":       "index.html",
		"force":      true,
	}, other, nil)
	if forceResp.StatusCode != http.StatusOK {
		t.Fatalf("force release expected 200, got %d body=%s", forceResp.StatusCode, forceBody)
	}
	exists, err := authV2Rdb.Exists(context.Background(), lockV2Key(teamID, project.ID, "index.html")).Result()
	if err != nil || exists != 0 {
		t.Fatalf("force-released lock should be gone: exists=%d err=%v", exists, err)
	}
	member := lockMember(project.ID, "index.html")
	stillMember, err := authV2Rdb.SIsMember(context.Background(), holderSet, member).Result()
	if err != nil {
		t.Fatal(err)
	}
	if stillMember {
		t.Fatalf("holder set %q still holds member after force release", holderSet)
	}
}

func TestV2LockListIsTeamScoped(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	session, _ := registerTeamUser(t, "locklist")
	team, _ := createTeamHTTP(t, session, "Lock List Team")
	teamID := team["id"].(string)
	project := createProjectV2HTTP(t, session, teamID, "List Project")

	for _, path := range []string{"index.html", "styles/site.css"} {
		resp, body := acquireLockWebV2(t, session, teamID, map[string]any{
			"project_id": project.ID,
			"path":       path,
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("acquire %s expected 200, got %d body=%s", path, resp.StatusCode, body)
		}
	}

	resp, body := authJSON(t, http.MethodGet, "/api/teams/"+teamID+"/locks?project_id="+project.ID, nil, map[string]string{
		"Cookie": withCookies(session),
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list locks expected 200, got %d body=%s", resp.StatusCode, body)
	}
	var list LockListResponseV2
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Locks) != 2 {
		t.Fatalf("expected 2 locks, got %d: %s", len(list.Locks), body)
	}
	for _, lock := range list.Locks {
		if lock.Owner.SessionName != "gui" || lock.Path == "" {
			t.Fatalf("lock info missing owner/path: %+v", lock)
		}
	}
}
