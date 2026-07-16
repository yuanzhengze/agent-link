package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// End-to-end multi-team isolation across the whole v2 surface: projects, tree,
// snapshot, locks, apply, preview and WebSocket. These compose the same HTTP
// helpers a real client would use against the shared authV2 test server.

func treeV2Web(t *testing.T, session *http.Response, teamID, projectID string) (*http.Response, TreeResponseV2) {
	t.Helper()
	resp, body := teamJSON(t, http.MethodGet, "/api/teams/"+teamID+"/projects/"+projectID+"/tree", nil, session, nil)
	var tree TreeResponseV2
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(body, &tree); err != nil {
			t.Fatalf("decode tree: %v (raw=%s)", err, body)
		}
	}
	return resp, tree
}

func snapshotV2Web(t *testing.T, session *http.Response, teamID, projectID string) (*http.Response, SnapshotResponseV2) {
	t.Helper()
	resp, body := teamJSON(t, http.MethodGet, "/api/teams/"+teamID+"/projects/"+projectID+"/snapshot", nil, session, nil)
	var snap SnapshotResponseV2
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(body, &snap); err != nil {
			t.Fatalf("decode snapshot: %v (raw=%s)", err, body)
		}
	}
	return resp, snap
}

func previewV2(t *testing.T, session *http.Response, teamID, projectID, path string) (*http.Response, []byte) {
	t.Helper()
	headers := map[string]string{}
	if session != nil {
		headers["Cookie"] = withCookies(session)
	}
	return authJSON(t, http.MethodGet, "/preview/teams/"+teamID+"/"+projectID+"/"+path, nil, headers)
}

func treeFile(tree TreeResponseV2, path string) *TreeFileEntryV2 {
	for i := range tree.Files {
		if tree.Files[i].Path == path {
			return &tree.Files[i]
		}
	}
	return nil
}

func snapshotHasFile(snap SnapshotResponseV2, path, content string) bool {
	for _, f := range snap.Files {
		if f.Path == path {
			return content == "" || f.Content == content
		}
	}
	return false
}

func TestE2EV2TwoUsersSameTeamCollaborate(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	owner, ownerResult := registerTeamUser(t, "e2ecollabown")
	ownerName := usernameOf(t, ownerResult)
	team, invite := createTeamHTTP(t, owner, "Collab Team")
	teamID := team["id"].(string)

	member, memberResult := registerTeamUser(t, "e2ecollabmem")
	memberName := usernameOf(t, memberResult)
	joinTeamHTTP(t, member, teamID, invite)

	project := createProjectV2HTTP(t, owner, teamID, "Collab Project")

	// Both members see the project.
	if got := listProjectsV2HTTP(t, member, teamID); len(got) != 1 || got[0].ID != project.ID {
		t.Fatalf("member project list = %+v; want [%s]", got, project.ID)
	}

	// Owner locks + applies index.html.
	seedFileV2(t, owner, teamID, project.ID, "index.html", "<html><body>owner-home</body></html>")

	// Member cannot apply to the owner-locked path (no lock held).
	if resp, body := applyV2Web(t, member, teamID, project.ID, map[string]any{
		"path":    "index.html",
		"content": "<html><body>member-overwrite</body></html>",
	}); resp.StatusCode != http.StatusConflict {
		t.Fatalf("member apply on locked path expected 409, got %d body=%s", resp.StatusCode, body)
	}

	// Member locks + applies a different path — disjoint work proceeds.
	seedFileV2(t, member, teamID, project.ID, "page.html", "<html><body>member-page</body></html>")

	// The tree shows both files, index.html owned by the owner.
	resp, tree := treeV2Web(t, member, teamID, project.ID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tree expected 200, got %d", resp.StatusCode)
	}
	idx := treeFile(tree, "index.html")
	if idx == nil || !idx.Locked || idx.Owner == nil || idx.Owner.Username != ownerName {
		t.Fatalf("index.html should be locked by %s: %+v", ownerName, idx)
	}
	page := treeFile(tree, "page.html")
	if page == nil || !page.Locked || page.Owner == nil || page.Owner.Username != memberName {
		t.Fatalf("page.html should be locked by %s: %+v", memberName, page)
	}

	// Snapshot is consistent and contains both files.
	sResp, snap := snapshotV2Web(t, member, teamID, project.ID)
	if sResp.StatusCode != http.StatusOK {
		t.Fatalf("snapshot expected 200, got %d", sResp.StatusCode)
	}
	if !validGitObjectID(snap.HeadCommit) {
		t.Fatalf("snapshot head invalid: %q", snap.HeadCommit)
	}
	if !snapshotHasFile(snap, "index.html", "<html><body>owner-home</body></html>") {
		t.Fatalf("snapshot missing owner file: %+v", snap.Files)
	}
	if !snapshotHasFile(snap, "page.html", "<html><body>member-page</body></html>") {
		t.Fatalf("snapshot missing member file: %+v", snap.Files)
	}

	// Both members can preview.
	if pResp, pBody := previewV2(t, member, teamID, project.ID, "index.html"); pResp.StatusCode != http.StatusOK || !strings.Contains(string(pBody), "owner-home") {
		t.Fatalf("member preview = %d body=%s", pResp.StatusCode, pBody)
	}
}

func TestE2EV2TwoTeamsCannotSeeEachOther(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	userA, _ := registerTeamUser(t, "e2exteamA")
	teamA, _ := createTeamHTTP(t, userA, "X Team A")
	teamAID := teamA["id"].(string)
	projectA := createProjectV2HTTP(t, userA, teamAID, "X Project A")
	seedFileV2(t, userA, teamAID, projectA.ID, "index.html", "<html><body>secret-A</body></html>")

	userB, _ := registerTeamUser(t, "e2exteamB")
	teamB, _ := createTeamHTTP(t, userB, "X Team B")
	teamBID := teamB["id"].(string)

	// B is not a member of team A: hitting A's routes is 403.
	if resp, _ := teamJSON(t, http.MethodGet, "/api/teams/"+teamAID+"/projects", nil, userB, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-member list of team A expected 403, got %d", resp.StatusCode)
	}

	// B's own project list never contains team A's project.
	if got := listProjectsV2HTTP(t, userB, teamBID); len(got) != 0 {
		t.Fatalf("team B project list must be empty, got %+v", got)
	}

	// Referencing team A's project id under team B's path is a 404 everywhere.
	if resp, _ := treeV2Web(t, userB, teamBID, projectA.ID); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-team tree expected 404, got %d", resp.StatusCode)
	}
	if resp, _ := snapshotV2Web(t, userB, teamBID, projectA.ID); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-team snapshot expected 404, got %d", resp.StatusCode)
	}
	if resp, body := acquireLockWebV2(t, userB, teamBID, map[string]any{
		"project_id": projectA.ID,
		"path":       "index.html",
	}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-team lock expected 404, got %d body=%s", resp.StatusCode, body)
	}
	if resp, _ := applyV2Web(t, userB, teamBID, projectA.ID, map[string]any{
		"path":    "index.html",
		"content": "<h1>hijack</h1>",
	}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-team apply expected 404, got %d", resp.StatusCode)
	}
	if resp, body := previewV2(t, userB, teamBID, projectA.ID, "index.html"); resp.StatusCode == http.StatusOK {
		t.Fatalf("cross-team preview must not serve content, got 200 body=%s", body)
	}

	// WS: B cannot subscribe to team A (non-member) or to A's project via B.
	if c, resp, err := dialWSV2(t, map[string]string{"Cookie": withCookies(userB)}, teamAID, projectA.ID); err == nil {
		c.CloseNow()
		t.Fatalf("non-member WS to team A should fail; got status %d", resp.StatusCode)
	}
	if c, resp, err := dialWSV2(t, map[string]string{"Cookie": withCookies(userB)}, teamBID, projectA.ID); err == nil {
		c.CloseNow()
		t.Fatalf("cross-team-project WS should fail; got status %d", resp.StatusCode)
	}
}

func TestE2EV2SamePathLocksAreIndependentByTeam(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	user, _ := registerTeamUser(t, "e2esamelock")
	teamA, _ := createTeamHTTP(t, user, "Same Lock A")
	teamB, _ := createTeamHTTP(t, user, "Same Lock B")
	teamAID := teamA["id"].(string)
	teamBID := teamB["id"].(string)
	projectA := createProjectV2HTTP(t, user, teamAID, "Same Lock Project A")
	projectB := createProjectV2HTTP(t, user, teamBID, "Same Lock Project B")

	if resp, body := acquireLockWebV2(t, user, teamAID, map[string]any{
		"project_id": projectA.ID,
		"path":       "index.html",
	}); resp.StatusCode != http.StatusOK {
		t.Fatalf("team A lock expected 200, got %d body=%s", resp.StatusCode, body)
	}
	// The same path in a different team's project is independent.
	if resp, body := acquireLockWebV2(t, user, teamBID, map[string]any{
		"project_id": projectB.ID,
		"path":       "index.html",
	}); resp.StatusCode != http.StatusOK {
		t.Fatalf("team B lock expected 200 (independent), got %d body=%s", resp.StatusCode, body)
	}
}

func TestE2EV2RemovedMemberLosesAccessImmediately(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	owner, _ := registerTeamUser(t, "e2ermvown")
	team, invite := createTeamHTTP(t, owner, "Remove Team")
	teamID := team["id"].(string)
	project := createProjectV2HTTP(t, owner, teamID, "Remove Project")
	seedFileV2(t, owner, teamID, project.ID, "index.html", "<html><body>ok</body></html>")

	member, memberResult := registerTeamUser(t, "e2ermvmem")
	joinTeamHTTP(t, member, teamID, invite)
	memberID := userIDFromAuthResult(t, memberResult)

	// Before removal the member has access.
	if got := listProjectsV2HTTP(t, member, teamID); len(got) != 1 {
		t.Fatalf("member should see 1 project before removal, got %d", len(got))
	}

	// Owner removes the member.
	if resp, body := teamJSON(t, http.MethodDelete, "/api/teams/"+teamID+"/members/"+memberID, nil, owner, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("remove member expected 204, got %d body=%s", resp.StatusCode, body)
	}

	// Access is revoked immediately (role re-read from the store per request).
	if resp, _ := teamJSON(t, http.MethodGet, "/api/teams/"+teamID+"/projects", nil, member, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("removed member list expected 403, got %d", resp.StatusCode)
	}
	if resp, body := previewV2(t, member, teamID, project.ID, "index.html"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("removed member preview expected 403, got %d body=%s", resp.StatusCode, body)
	}
	if c, resp, err := dialWSV2(t, map[string]string{"Cookie": withCookies(member)}, teamID, project.ID); err == nil {
		c.CloseNow()
		t.Fatalf("removed member WS should fail; got status %d", resp.StatusCode)
	}
}

func TestE2EV2WebAndDeviceWSStayInsideTeam(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	owner, ownerResult := registerTeamUser(t, "e2ewsdev")
	ownerName := usernameOf(t, ownerResult)
	team, _ := createTeamHTTP(t, owner, "WS Inside Team")
	teamID := team["id"].(string)
	project := createProjectV2HTTP(t, owner, teamID, "WS Inside Project")

	credential, _ := deviceLoginV2(t, ownerName, "correct horse battery staple", "ws-inside-dev")

	// A web cookie subscriber and a device subscriber, same (team, project).
	webConn, _, err := dialWSV2(t, map[string]string{"Cookie": withCookies(owner)}, teamID, project.ID)
	if err != nil {
		t.Fatalf("web dial failed: %v", err)
	}
	t.Cleanup(func() { webConn.CloseNow() })
	devConn, _, err := dialWSV2(t, map[string]string{"Authorization": "Device " + credential}, teamID, project.ID)
	if err != nil {
		t.Fatalf("device dial failed: %v", err)
	}
	t.Cleanup(func() { devConn.CloseNow() })

	key := subscriptionKeyV2{TeamID: teamID, ProjectID: project.ID}
	waitForSubscriberV2(t, key, 2)

	// A real apply fans a file_changed event out to both subscribers.
	seedFileV2(t, owner, teamID, project.ID, "index.html", "<html><body>broadcast</body></html>")

	if ev := readEventV2(t, webConn); ev.ProjectID != project.ID || ev.Path != "index.html" {
		t.Fatalf("web subscriber event = %+v; want project %s index.html", ev, project.ID)
	}
	if ev := readEventV2(t, devConn); ev.ProjectID != project.ID || ev.Path != "index.html" {
		t.Fatalf("device subscriber event = %+v; want project %s index.html", ev, project.ID)
	}

	// An event for another team/project never reaches these subscribers.
	authV2Srv.hubV2.Broadcast("t_elsewhere", "p_elsewhere", EventV2{
		Type: "file_changed", TeamID: "t_elsewhere", ProjectID: "p_elsewhere", At: nowRFC3339(),
	})
	expectNoMessage(t, webConn, 400*time.Millisecond)
	expectNoMessage(t, devConn, 400*time.Millisecond)
}

// TestE2EV2BrowserJourney mirrors the user-visible browser flow end to end over
// Cookie+CSRF: Alice registers and creates a team, Bob registers and joins with
// the invite, Bob locks and applies a file, and Alice previews the result.
func TestE2EV2BrowserJourney(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	alice := registerBrowserUser(t, "journeyalice", "correct horse 123")
	team, invite := createBrowserTeam(t, alice, "Journey Team")
	bob := registerBrowserUser(t, "journeybob", "correct battery 456")
	joinBrowserTeam(t, bob, team.ID, invite)

	// Alice creates a project; Bob (same team) sees and edits it.
	project := createProjectV2HTTP(t, alice.Session, team.ID, "Prototype")
	if got := listProjectsV2HTTP(t, bob.Session, team.ID); len(got) != 1 || got[0].ID != project.ID {
		t.Fatalf("bob project list = %+v; want [%s]", got, project.ID)
	}

	// Bob locks + applies index.html through the same Cookie/CSRF path the GUI uses.
	seedFileV2(t, bob.Session, team.ID, project.ID, "index.html", "<h1>joined</h1>")

	// Alice previews the committed file with live-reload injected.
	resp, body := previewV2(t, alice.Session, team.ID, project.ID, "index.html")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("alice preview expected 200, got %d body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "<h1>joined</h1>") {
		t.Fatalf("alice preview missing bob's content: %s", body)
	}

	// A different team cannot reach the project or its preview.
	carol := registerBrowserUser(t, "journeycarol", "correct staple 789")
	otherTeam, _ := createBrowserTeam(t, carol, "Other Journey Team")
	if r, tree := treeV2Web(t, carol.Session, otherTeam.ID, project.ID); r.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-team tree expected 404, got %d (%+v)", r.StatusCode, tree)
	}
	if r, b := previewV2(t, carol.Session, otherTeam.ID, project.ID, "index.html"); r.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-team preview expected 404, got %d body=%s", r.StatusCode, b)
	}
}
