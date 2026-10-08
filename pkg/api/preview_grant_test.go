package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestPreviewOriginMustDiffer(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic when preview origin equals the public origin")
		}
	}()
	NewWithOptions(ServerOptions{
		DataDir:          t.TempDir(),
		PublicURL:        "http://localhost:8080",
		PreviewPublicURL: "http://localhost:8080/",
	})
}

func TestPreviewGrantSameOriginFallback(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	owner, _ := registerTeamUser(t, "prevfallback")
	team, _ := createTeamHTTP(t, owner, "Preview Fallback Team")
	teamID := team["id"].(string)
	project := createProjectV2HTTP(t, owner, teamID, "Preview Fallback Project")
	seedFileV2(t, owner, teamID, project.ID, "index.html", "<html><body>fallback-ok</body></html>")

	resp, body := teamJSON(t, http.MethodPost, "/api/teams/"+teamID+"/projects/"+project.ID+"/preview-grant", map[string]any{}, owner, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("grant expected 200, got %d body=%s", resp.StatusCode, body)
	}
	var grant previewGrantResponse
	if err := json.Unmarshal(body, &grant); err != nil {
		t.Fatal(err)
	}
	if grant.Isolated {
		t.Fatalf("same-origin server reported isolated preview: %+v", grant)
	}
	want := "/preview/" + teamID + "/" + project.ID + "/"
	if grant.BootstrapURL != want {
		t.Fatalf("bootstrap_url = %q; want %q", grant.BootstrapURL, want)
	}

	page, pageBody := authJSON(t, http.MethodGet, grant.BootstrapURL+"index.html", nil, map[string]string{
		"Cookie": withCookies(owner),
	})
	if page.StatusCode != http.StatusOK || !strings.Contains(string(pageBody), "fallback-ok") {
		t.Fatalf("cookie preview expected the file, got %d body=%s", page.StatusCode, pageBody)
	}
}

func TestIsolatedPreviewGrant(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	const (
		publicOrigin  = "http://localhost:8080"
		previewOrigin = "http://127.0.0.1:8080"
	)
	srv := NewWithOptions(ServerOptions{
		DataDir:          t.TempDir(),
		Redis:            authV2Rdb,
		CookieSecure:     false,
		PublicURL:        publicOrigin,
		PreviewPublicURL: previewOrigin,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	owner := isoRegister(t, ts, publicOrigin, "previsoown")
	teamID, invite := isoCreateTeam(t, ts, publicOrigin, owner, "Preview Isolated Team")
	projectID := isoCreateProject(t, ts, publicOrigin, owner, teamID, "Preview Isolated Project")
	isoSeed(t, ts, publicOrigin, owner, teamID, projectID, "index.html", "<html><body><p id=\"marker\">isolated-ok</p></body></html>")
	isoSeed(t, ts, publicOrigin, owner, teamID, projectID, "styles/site.css", "body{color:red}")

	// Cookie-authenticated preview on the application host must not render.
	cookiePage := isoDo(t, ts, http.MethodGet, "/preview/"+teamID+"/"+projectID+"/index.html", "localhost:8080", nil, owner.cookie, publicOrigin, "")
	if cookiePage.status != http.StatusNotFound {
		t.Fatalf("app-host cookie preview expected 404, got %d body=%s", cookiePage.status, cookiePage.body)
	}

	grant := isoIssueGrant(t, ts, publicOrigin, owner, teamID, projectID)
	if !grant.Isolated || grant.ExpiresIn != int(previewGrantTTL.Seconds()) {
		t.Fatalf("grant = %+v; want isolated with ttl %d", grant, int(previewGrantTTL.Seconds()))
	}
	grantURL, err := url.Parse(grant.BootstrapURL)
	if err != nil {
		t.Fatal(err)
	}
	if grantURL.Scheme+"://"+grantURL.Host != previewOrigin {
		t.Fatalf("bootstrap host = %s; want %s", grantURL.Host, previewOrigin)
	}
	if !strings.Contains(grantURL.Path, "/g/pg_") {
		t.Fatalf("bootstrap path missing grant: %s", grantURL.Path)
	}

	// The same path on the application host must not render either.
	appHost := isoDo(t, ts, http.MethodGet, grantURL.Path, "localhost:8080", nil, "", "", "")
	if appHost.status != http.StatusNotFound {
		t.Fatalf("app-host grant preview expected 404, got %d body=%s", appHost.status, appHost.body)
	}

	page := isoDo(t, ts, http.MethodGet, grantURL.Path, grantURL.Host, nil, "", "", "")
	if page.status != http.StatusOK || !strings.Contains(page.body, "isolated-ok") {
		t.Fatalf("preview grant expected the file, got %d body=%s", page.status, page.body)
	}
	if !strings.Contains(page.body, "/g/") || !strings.Contains(page.body, "/ws") {
		t.Fatalf("live-reload script missing grant websocket: %s", page.body)
	}
	if strings.Contains(page.body, "/api/teams/") || strings.Contains(page.body, "token=") {
		t.Fatalf("isolated preview must not target the app API or a token query: %s", page.body)
	}
	if page.header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("nosniff = %q", page.header.Get("X-Content-Type-Options"))
	}
	if page.header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("referrer policy = %q", page.header.Get("Referrer-Policy"))
	}
	if csp := page.header.Get("Content-Security-Policy"); csp != "frame-ancestors "+publicOrigin {
		t.Fatalf("csp = %q", csp)
	}
	if !strings.Contains(page.header.Get("Cache-Control"), "no-store") {
		t.Fatalf("cache-control = %q", page.header.Get("Cache-Control"))
	}

	css := isoDo(t, ts, http.MethodGet, strings.TrimRight(grantURL.Path, "/")+"/styles/site.css", grantURL.Host, nil, "", "", "")
	if css.status != http.StatusOK || css.body != "body{color:red}" {
		t.Fatalf("css via grant = %d %q", css.status, css.body)
	}
	if !strings.Contains(css.header.Get("Content-Type"), "text/css") {
		t.Fatalf("css content-type = %q", css.header.Get("Content-Type"))
	}

	bad := isoDo(t, ts, http.MethodGet, "/preview/"+teamID+"/"+projectID+"/g/pg_not-a-real-grant/index.html", grantURL.Host, nil, "", "", "")
	if bad.status != http.StatusUnauthorized {
		t.Fatalf("bad grant expected 401, got %d body=%s", bad.status, bad.body)
	}

	other := isoCreateProject(t, ts, publicOrigin, owner, teamID, "Other Isolated Project")
	swapped := strings.Replace(grantURL.Path, "/"+projectID+"/", "/"+other+"/", 1)
	cross := isoDo(t, ts, http.MethodGet, swapped, grantURL.Host, nil, "", "", "")
	if cross.status != http.StatusNotFound {
		t.Fatalf("grant used on another project expected 404, got %d body=%s", cross.status, cross.body)
	}

	outsider := isoRegister(t, ts, publicOrigin, "previsoout")
	denied := isoDo(t, ts, http.MethodPost, "/api/teams/"+teamID+"/projects/"+projectID+"/preview-grant", "", []byte("{}"), outsider.cookie, publicOrigin, outsider.csrf)
	if denied.status != http.StatusForbidden {
		t.Fatalf("non-member grant expected 403, got %d body=%s", denied.status, denied.body)
	}

	member := isoRegister(t, ts, publicOrigin, "previsomem")
	isoJoin(t, ts, publicOrigin, member, teamID, invite)
	memberGrant := isoIssueGrant(t, ts, publicOrigin, member, teamID, projectID)
	memberURL, err := url.Parse(memberGrant.BootstrapURL)
	if err != nil {
		t.Fatal(err)
	}
	removed := isoDo(t, ts, http.MethodDelete, "/api/teams/"+teamID+"/members/"+member.userID, "", nil, owner.cookie, publicOrigin, owner.csrf)
	if removed.status != http.StatusNoContent {
		t.Fatalf("remove member expected 204, got %d body=%s", removed.status, removed.body)
	}
	after := isoDo(t, ts, http.MethodGet, memberURL.Path, memberURL.Host, nil, "", "", "")
	if after.status != http.StatusUnauthorized {
		t.Fatalf("removed member grant expected 401, got %d body=%s", after.status, after.body)
	}

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + strings.TrimRight(grantURL.Path, "/") + "/ws"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{Host: grantURL.Host})
	if err != nil {
		t.Fatalf("preview websocket dial: %v", err)
	}
	defer conn.CloseNow()
	waitForHubSubscriber(t, srv, subscriptionKeyV2{TeamID: teamID, ProjectID: projectID}, 1)
	srv.hubV2.Broadcast(teamID, projectID, EventV2{
		Type:      "file_changed",
		TeamID:    teamID,
		ProjectID: projectID,
		Path:      "index.html",
		At:        time.Now().UTC().Format(time.RFC3339),
	})
	ev := readEventV2(t, conn)
	if ev.Type != "file_changed" || ev.ProjectID != projectID {
		t.Fatalf("preview websocket event = %+v", ev)
	}
}

type isoSession struct {
	cookie string
	csrf   string
	userID string
}

type isoResult struct {
	status int
	header http.Header
	body   string
	cookie string
	csrf   string
}

func isoRegister(t *testing.T, ts *httptest.Server, origin, suffix string) isoSession {
	t.Helper()
	username := "iso" + suffix + "user"
	res := isoDo(t, ts, http.MethodPost, "/api/auth/register", "", []byte(`{"username":"`+username+`","password":"correct horse battery staple"}`), "", "", "")
	if res.status != http.StatusOK {
		t.Fatalf("register %s: %d %s", username, res.status, res.body)
	}
	var payload struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if err := json.Unmarshal([]byte(res.body), &payload); err != nil {
		t.Fatal(err)
	}
	return isoSession{cookie: res.cookie, csrf: res.csrf, userID: payload.User.ID}
}

func isoCreateTeam(t *testing.T, ts *httptest.Server, origin string, sess isoSession, name string) (string, string) {
	t.Helper()
	res := isoDo(t, ts, http.MethodPost, "/api/teams", "", []byte(`{"name":"`+name+`"}`), sess.cookie, origin, sess.csrf)
	if res.status != http.StatusCreated {
		t.Fatalf("create team: %d %s", res.status, res.body)
	}
	var payload struct {
		InviteCode string `json:"invite_code"`
		Team       struct {
			ID string `json:"id"`
		} `json:"team"`
	}
	if err := json.Unmarshal([]byte(res.body), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Team.ID == "" || payload.InviteCode == "" {
		t.Fatalf("create team payload missing fields: %s", res.body)
	}
	return payload.Team.ID, payload.InviteCode
}

func isoJoin(t *testing.T, ts *httptest.Server, origin string, sess isoSession, teamID, invite string) {
	t.Helper()
	body := `{"team_id":"` + teamID + `","invite_code":"` + invite + `"}`
	res := isoDo(t, ts, http.MethodPost, "/api/teams/join", "", []byte(body), sess.cookie, origin, sess.csrf)
	if res.status != http.StatusOK {
		t.Fatalf("join team: %d %s", res.status, res.body)
	}
}

func isoCreateProject(t *testing.T, ts *httptest.Server, origin string, sess isoSession, teamID, name string) string {
	t.Helper()
	res := isoDo(t, ts, http.MethodPost, "/api/teams/"+teamID+"/projects", "", []byte(`{"name":"`+name+`"}`), sess.cookie, origin, sess.csrf)
	if res.status != http.StatusCreated {
		t.Fatalf("create project: %d %s", res.status, res.body)
	}
	var project TeamProject
	if err := json.Unmarshal([]byte(res.body), &project); err != nil {
		t.Fatal(err)
	}
	if project.ID == "" {
		t.Fatalf("create project missing id: %s", res.body)
	}
	return project.ID
}

func isoSeed(t *testing.T, ts *httptest.Server, origin string, sess isoSession, teamID, projectID, path, content string) {
	t.Helper()
	lockBody, _ := json.Marshal(map[string]string{"project_id": projectID, "path": path})
	lock := isoDo(t, ts, http.MethodPost, "/api/teams/"+teamID+"/locks/acquire", "", lockBody, sess.cookie, origin, sess.csrf)
	if lock.status != http.StatusOK {
		t.Fatalf("seed lock %s: %d %s", path, lock.status, lock.body)
	}
	applyBody, _ := json.Marshal(map[string]string{"path": path, "content": content})
	applied := isoDo(t, ts, http.MethodPost, "/api/teams/"+teamID+"/projects/"+projectID+"/apply", "", applyBody, sess.cookie, origin, sess.csrf)
	if applied.status != http.StatusOK {
		t.Fatalf("seed apply %s: %d %s", path, applied.status, applied.body)
	}
}

func isoIssueGrant(t *testing.T, ts *httptest.Server, origin string, sess isoSession, teamID, projectID string) previewGrantResponse {
	t.Helper()
	res := isoDo(t, ts, http.MethodPost, "/api/teams/"+teamID+"/projects/"+projectID+"/preview-grant", "", []byte("{}"), sess.cookie, origin, sess.csrf)
	if res.status != http.StatusOK {
		t.Fatalf("issue grant: %d %s", res.status, res.body)
	}
	var grant previewGrantResponse
	if err := json.Unmarshal([]byte(res.body), &grant); err != nil {
		t.Fatal(err)
	}
	return grant
}

func isoDo(t *testing.T, ts *httptest.Server, method, path, host string, body []byte, cookie, origin, csrf string) isoResult {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(string(body))
	}
	req, err := http.NewRequest(method, ts.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if host != "" {
		req.Host = host
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var parts []string
	for _, c := range resp.Cookies() {
		parts = append(parts, c.Name+"="+c.Value)
	}
	csrfValue := ""
	if c := cookieByName(resp.Cookies(), csrfCookieName); c != nil {
		csrfValue = c.Value
	}
	return isoResult{
		status: resp.StatusCode,
		header: resp.Header,
		body:   string(raw),
		cookie: strings.Join(parts, "; "),
		csrf:   csrfValue,
	}
}

func waitForHubSubscriber(t *testing.T, srv *Server, key subscriptionKeyV2, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		srv.hubV2.mu.RLock()
		n := len(srv.hubV2.subs[key])
		srv.hubV2.mu.RUnlock()
		if n >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d preview subscriber(s) on %+v", want, key)
}
