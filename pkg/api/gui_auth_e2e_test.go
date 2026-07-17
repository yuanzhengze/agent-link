package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// Browser-flavored fixtures: real Cookie + CSRF requests against the shared
// authV2 test server, mirroring what the GUI does through fetch().

type browserUser struct {
	Session  *http.Response
	UserID   string
	Username string
}

type browserTeam struct {
	ID   string
	Name string
}

func registerBrowserUser(t *testing.T, username, password string) browserUser {
	t.Helper()
	resp, body := authJSON(t, http.MethodPost, "/api/auth/register", map[string]string{
		"username": username,
		"password": password,
	}, map[string]string{"Origin": authTestOrigin})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register %s expected 200, got %d body=%s", username, resp.StatusCode, body)
	}
	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	return browserUser{Session: resp, UserID: userIDFromAuthResult(t, result), Username: username}
}

func createBrowserTeam(t *testing.T, u browserUser, name string) (browserTeam, string) {
	t.Helper()
	team, invite := createTeamHTTP(t, u.Session, name)
	return browserTeam{ID: team["id"].(string), Name: name}, invite
}

func joinBrowserTeam(t *testing.T, u browserUser, teamID, invite string) {
	t.Helper()
	joinTeamHTTP(t, u.Session, teamID, invite)
}

func assertTeamList(t *testing.T, u browserUser, wantTeamID string) {
	t.Helper()
	resp, body := teamJSON(t, http.MethodGet, "/api/teams", nil, u.Session, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list teams expected 200, got %d body=%s", resp.StatusCode, body)
	}
	var result struct {
		Teams []map[string]any `json:"teams"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	for _, team := range result.Teams {
		if team["id"] == wantTeamID {
			return
		}
	}
	t.Fatalf("team %s not in %s's team list: %s", wantTeamID, u.Username, body)
}

func TestGUIAuthTeamOnboardingFlow(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	alice := registerBrowserUser(t, "guialice", "long-enough-password")
	team, invite := createBrowserTeam(t, alice, "Product")
	bob := registerBrowserUser(t, "guibob", "another-long-password")
	joinBrowserTeam(t, bob, team.ID, invite)

	assertTeamList(t, alice, team.ID)
	assertTeamList(t, bob, team.ID)
}

func setupThreeMemberTeam(t *testing.T) (owner, admin, member browserUser, team browserTeam) {
	t.Helper()
	owner = registerBrowserUser(t, "guiowner", "long-enough-password")
	team, invite := createBrowserTeam(t, owner, "Roles")
	admin = registerBrowserUser(t, "guiadmin", "long-enough-password")
	joinBrowserTeam(t, admin, team.ID, invite)
	member = registerBrowserUser(t, "guimember", "long-enough-password")
	joinBrowserTeam(t, member, team.ID, invite)
	return owner, admin, member, team
}

func changeRoleAs(t *testing.T, actor browserUser, teamID, targetID, role string, want int) {
	t.Helper()
	resp, body := teamJSON(t, http.MethodPatch, "/api/teams/"+teamID+"/members/"+targetID, map[string]string{"role": role}, actor.Session, nil)
	if resp.StatusCode != want {
		t.Fatalf("changeRole by %s -> %s expected %d, got %d body=%s", actor.Username, role, want, resp.StatusCode, body)
	}
}

func removeMemberAs(t *testing.T, actor browserUser, teamID, targetID string, want int) {
	t.Helper()
	resp, body := teamJSON(t, http.MethodDelete, "/api/teams/"+teamID+"/members/"+targetID, nil, actor.Session, nil)
	if resp.StatusCode != want {
		t.Fatalf("removeMember by %s expected %d, got %d body=%s", actor.Username, want, resp.StatusCode, body)
	}
}

func transferOwnerAs(t *testing.T, actor browserUser, teamID, targetID string, want int) {
	t.Helper()
	resp, body := teamJSON(t, http.MethodPost, "/api/teams/"+teamID+"/transfer-owner", map[string]string{"user_id": targetID}, actor.Session, nil)
	if resp.StatusCode != want {
		t.Fatalf("transferOwner by %s expected %d, got %d body=%s", actor.Username, want, resp.StatusCode, body)
	}
}

func leaveTeamAs(t *testing.T, actor browserUser, teamID string) (int, []byte) {
	t.Helper()
	resp, body := teamJSON(t, http.MethodPost, "/api/teams/"+teamID+"/leave", nil, actor.Session, nil)
	return resp.StatusCode, body
}

func TestGUIRoleAdministrationFlow(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	owner, admin, member, team := setupThreeMemberTeam(t)

	// Owner promotes admin; admin cannot change roles; admin can remove a
	// member; owner transfers ownership.
	changeRoleAs(t, owner, team.ID, admin.UserID, "admin", http.StatusNoContent)
	changeRoleAs(t, admin, team.ID, member.UserID, "admin", http.StatusForbidden)
	removeMemberAs(t, admin, team.ID, member.UserID, http.StatusNoContent)
	transferOwnerAs(t, owner, team.ID, admin.UserID, http.StatusNoContent)
}

func TestGUIAdminAndMemberCanLeaveTeam(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	owner, admin, member, team := setupThreeMemberTeam(t)
	changeRoleAs(t, owner, team.ID, admin.UserID, "admin", http.StatusNoContent)

	// Admin and member can leave on their own.
	if status, body := leaveTeamAs(t, member, team.ID); status != http.StatusNoContent {
		t.Fatalf("member leave expected 204, got %d body=%s", status, body)
	}
	if status, body := leaveTeamAs(t, admin, team.ID); status != http.StatusNoContent {
		t.Fatalf("admin leave expected 204, got %d body=%s", status, body)
	}

	// The owner cannot leave until ownership is transferred.
	if status, _ := leaveTeamAs(t, owner, team.ID); status != http.StatusForbidden && status != http.StatusConflict {
		t.Fatalf("owner leave expected 403/409, got %d", status)
	}
}
