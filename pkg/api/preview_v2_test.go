package api

import (
	"net/http"
	"strings"
	"testing"
)

// seedFileV2 acquires the file lock and applies content so the work tree has a
// committed file for preview tests to serve.
func seedFileV2(t *testing.T, session *http.Response, teamID, projectID, path, content string) {
	t.Helper()
	if r, b := acquireLockWebV2(t, session, teamID, map[string]any{
		"project_id": projectID,
		"path":       path,
	}); r.StatusCode != http.StatusOK {
		t.Fatalf("seed acquire %s expected 200, got %d body=%s", path, r.StatusCode, b)
	}
	if r, b := applyV2Web(t, session, teamID, projectID, map[string]any{
		"path":    path,
		"content": content,
	}); r.StatusCode != http.StatusOK {
		t.Fatalf("seed apply %s expected 200, got %d body=%s", path, r.StatusCode, b)
	}
}

func TestV2PreviewRequiresMembership(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	owner, _ := registerTeamUser(t, "prevmemown")
	team, _ := createTeamHTTP(t, owner, "Preview Mem Team")
	teamID := team["id"].(string)
	project := createProjectV2HTTP(t, owner, teamID, "Preview Mem Project")
	seedFileV2(t, owner, teamID, project.ID, "index.html", "<html><body>preview-ok</body></html>")

	previewPath := "/preview/teams/" + teamID + "/" + project.ID + "/index.html"

	// Member can view.
	resp, body := authJSON(t, http.MethodGet, previewPath, nil, map[string]string{"Cookie": withCookies(owner)})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("member preview expected 200, got %d body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "preview-ok") {
		t.Fatalf("member preview missing content: %s", body)
	}

	// Non-member is forbidden.
	outsider, _ := registerTeamUser(t, "prevmemout")
	resp2, body2 := authJSON(t, http.MethodGet, previewPath, nil, map[string]string{"Cookie": withCookies(outsider)})
	if resp2.StatusCode != http.StatusForbidden {
		t.Fatalf("non-member preview expected 403, got %d body=%s", resp2.StatusCode, body2)
	}

	// Unauthenticated is rejected.
	resp3, body3 := authJSON(t, http.MethodGet, previewPath, nil, nil)
	if resp3.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated preview expected 401, got %d body=%s", resp3.StatusCode, body3)
	}
}

func TestV2PreviewLiveReloadUsesCookieWSWithoutTokenQuery(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	owner, _ := registerTeamUser(t, "prevreload")
	team, _ := createTeamHTTP(t, owner, "Preview Reload Team")
	teamID := team["id"].(string)
	project := createProjectV2HTTP(t, owner, teamID, "Preview Reload Project")
	seedFileV2(t, owner, teamID, project.ID, "index.html", "<html><body>hi</body></html>")

	resp, body := authJSON(t, http.MethodGet, "/preview/teams/"+teamID+"/"+project.ID+"/index.html", nil, map[string]string{
		"Cookie": withCookies(owner),
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("preview expected 200, got %d body=%s", resp.StatusCode, body)
	}
	html := string(body)
	if !strings.Contains(html, "/api/teams/") || !strings.Contains(html, "/ws?project=") {
		t.Fatalf("live-reload script must target the team WS route: %s", html)
	}
	if !strings.Contains(html, teamID) || !strings.Contains(html, project.ID) {
		t.Fatalf("live-reload script must embed team/project ids: %s", html)
	}
	if strings.Contains(html, "token=") {
		t.Fatalf("live-reload script must not carry a token in the URL: %s", html)
	}
}

// TestV2PreviewNonHTMLNotInjected verifies non-HTML assets are served verbatim
// with a derived Content-Type and no live-reload script injection.
func TestV2PreviewNonHTMLNotInjected(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	owner, _ := registerTeamUser(t, "prevasset")
	team, _ := createTeamHTTP(t, owner, "Preview Asset Team")
	teamID := team["id"].(string)
	project := createProjectV2HTTP(t, owner, teamID, "Preview Asset Project")
	seedFileV2(t, owner, teamID, project.ID, "styles/site.css", "body{color:red}")

	resp, body := authJSON(t, http.MethodGet, "/preview/teams/"+teamID+"/"+project.ID+"/styles/site.css", nil, map[string]string{
		"Cookie": withCookies(owner),
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("css preview expected 200, got %d body=%s", resp.StatusCode, body)
	}
	if got := string(body); got != "body{color:red}" {
		t.Fatalf("css served altered: %q", got)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/css") {
		t.Fatalf("css Content-Type = %q; want text/css", ct)
	}
	if nosniff := resp.Header.Get("X-Content-Type-Options"); nosniff != "nosniff" {
		t.Fatalf("preview must set X-Content-Type-Options: nosniff, got %q", nosniff)
	}
}

// TestV2PreviewDirectoryIs404 verifies a path that resolves to a directory
// returns 404 (not a 500 that leaks the target is a directory).
func TestV2PreviewDirectoryIs404(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	owner, _ := registerTeamUser(t, "prevdir")
	team, _ := createTeamHTTP(t, owner, "Preview Dir Team")
	teamID := team["id"].(string)
	project := createProjectV2HTTP(t, owner, teamID, "Preview Dir Project")
	seedFileV2(t, owner, teamID, project.ID, "assets/logo.txt", "logo")

	// "assets" is a directory, not a file.
	resp, body := authJSON(t, http.MethodGet, "/preview/teams/"+teamID+"/"+project.ID+"/assets", nil, map[string]string{
		"Cookie": withCookies(owner),
	})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("directory preview expected 404, got %d body=%s", resp.StatusCode, body)
	}
}

// TestV2PreviewRejectsTraversal verifies path traversal / .git access is mapped
// to 404 for an authenticated member (no leak of why the path is unreachable).
func TestV2PreviewRejectsTraversal(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	owner, _ := registerTeamUser(t, "prevtrav")
	team, _ := createTeamHTTP(t, owner, "Preview Traversal Team")
	teamID := team["id"].(string)
	project := createProjectV2HTTP(t, owner, teamID, "Preview Traversal Project")
	seedFileV2(t, owner, teamID, project.ID, "index.html", "<html><body>ok</body></html>")

	// Note: "../" segments are cleaned by the HTTP layer before routing, so
	// they never reach the handler; the .git guard is what safeApplyPath adds.
	for _, p := range []string{".git/config", ".GIT/config", "sub/.git/x"} {
		resp, body := authJSON(t, http.MethodGet, "/preview/teams/"+teamID+"/"+project.ID+"/"+p, nil, map[string]string{
			"Cookie": withCookies(owner),
		})
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("preview %q expected 404, got %d body=%s", p, resp.StatusCode, body)
		}
	}
}
