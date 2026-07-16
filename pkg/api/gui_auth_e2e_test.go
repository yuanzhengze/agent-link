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
