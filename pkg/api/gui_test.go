package api

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// getGUIAsset fetches a static GUI asset from the v1 test server (which mounts
// the same embedded web.FS) and returns its body as a string.
func getGUIAsset(t *testing.T, path string) string {
	t.Helper()
	resp, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s expected 200, got %d", path, resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestServeGUIContainsAuthViews(t *testing.T) {
	body := getGUIAsset(t, "/")
	for _, marker := range []string{
		`id="view-auth"`,
		`id="form-login"`,
		`id="form-register"`,
		`id="input-login-username"`,
		`id="input-register-password"`,
		`id="form-change-password"`,
		`id="preview-frame"`,
		`sandbox="allow-scripts allow-forms allow-popups allow-modals"`,
		`referrerpolicy="no-referrer"`,
		`id="form-new-project"`,
		`id="preview-empty"`,
		`id="invite-pair"`,
		`At least 10 characters.`,
		`Joining needs both the team ID and the invite code.`,
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("missing %s", marker)
		}
	}
}

func TestServeGUINoLongerContainsTokenInputs(t *testing.T) {
	body := getGUIAsset(t, "/")
	if strings.Contains(body, "input-token") || strings.Contains(body, "sk_live_") {
		t.Fatal("GUI still exposes legacy token settings")
	}
}

// TestServeGUIUsesV2TeamRoutes locks in that the dashboard talks to the
// team-scoped v2 API (via CoworkAPI's Cookie/CSRF fetch) and never attaches a
// hand-built Authorization header or legacy token query.
func TestServeGUIUsesV2TeamRoutes(t *testing.T) {
	dash := getGUIAsset(t, "/dashboard.js")
	for _, marker := range []string{
		"/api/teams/",
		"/projects",
		"/locks/acquire",
		"/agents",
		"/ws?project=",
		"/preview/",
		"preview-grant",
		"isolated",
	} {
		if !strings.Contains(dash, marker) {
			t.Errorf("dashboard.js missing team-scoped route %q", marker)
		}
	}
	for _, forbidden := range []string{
		"Authorization",
		"cowork_token",
		"token=",
	} {
		if strings.Contains(dash, forbidden) {
			t.Errorf("dashboard.js must not use legacy auth %q", forbidden)
		}
	}
	if strings.Contains(dash, "window.prompt") {
		t.Error("dashboard.js must create projects with the form modal, not window.prompt")
	}
	if !strings.Contains(dash, "agentlink sync ") {
		t.Error("dashboard.js missing sync command guidance")
	}
	teamsJS := getGUIAsset(t, "/teams.js")
	for _, marker := range []string{
		"Team ID: ",
		"Invite code: ",
	} {
		if !strings.Contains(teamsJS, marker) {
			t.Errorf("teams.js missing invite pair marker %q", marker)
		}
	}
}

// TestServeGUI_indexOk verifies the embedded dashboard is served at GET /
// with no Authorization header, and that the response contains the app's
// mount point marker.
func TestServeGUI_indexOk(t *testing.T) {
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)

	if !strings.Contains(body, `id="app"`) {
		t.Errorf("expected body to contain the app mount point id=\"app\", got: %s", body)
	}
	if !strings.Contains(body, "<title>cowork</title>") {
		t.Errorf("expected body to contain the page title, got: %s", body)
	}
}

// TestServeGUI_appJsOk verifies app.js is served with no auth and a
// JavaScript content type.
func TestServeGUI_appJsOk(t *testing.T) {
	resp, err := http.Get(ts.URL + "/app.js")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "javascript") {
		t.Errorf("expected Content-Type to contain javascript, got %q", ct)
	}
}

// TestServeGUI_styleCssOk verifies style.css is served with no auth.
func TestServeGUI_styleCssOk(t *testing.T) {
	resp, err := http.Get(ts.URL + "/style.css")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

// TestServeGUI_apiStillRequiresAuth locks in that serving the static GUI at
// GET / did not over-broaden auth: a team data endpoint like GET
// /api/teams/{team_id}/projects must still 401 without a session/device
// credential.
func TestServeGUI_apiStillRequiresAuth(t *testing.T) {
	resp, err := http.Get(ts.URL + "/api/teams/tm_test/projects")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for GET team projects without credentials, got %d", resp.StatusCode)
	}
}
