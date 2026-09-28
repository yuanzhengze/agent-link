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
		`至少 10 个字符`,
		`登录 cowork`,
		`创建账号`,
		`已有账号？`,
		`去登录`,
		`选择团队`,
		`加入需要团队 ID 和邀请码`,
		`加入团队`,
		`还没有项目。先创建一个，再用 CLI 同步静态原型。`,
		`还没有可预览的内容`,
		`用 CLI 同步静态原型`,
		`复制命令`,
		`然后刷新。右键文件可加锁，避免两人互相覆盖`,
		`新建项目`,
		`会创建一个空项目，用 CLI 同步静态原型后才能预览`,
		`只显示一次，加入需要这两行`,
		`复制团队 ID 和邀请码`,
		`lang="zh-CN"`,
		`&larr; 项目`,
		`+ 团队`,
		`id="btn-join-team-top" class="btn-secondary btn-small" type="button">加入</button>`,
		`id="btn-members" class="btn-secondary btn-small" type="button">成员</button>`,
		`id="btn-logout" class="btn-secondary btn-small" type="button">退出登录</button>`,
		`<h1>项目</h1>`,
		`id="btn-refresh-projects" class="btn-secondary" type="button">刷新</button>`,
		`<h2>文件</h2>`,
		`id="btn-refresh-tree" class="btn-secondary btn-small" type="button">刷新</button>`,
		`<h2>预览</h2>`,
		`<h2>在线</h2>`,
		`<h2>告警</h2>`,
		`id="btn-clear-alerts" class="btn-secondary btn-small" type="button">清除</button>`,
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("missing %s", marker)
		}
	}
	for _, gone := range []string{
		`lang="en"`,
		`&larr; Projects`,
		`>+ Team<`,
		`>Join</button>`,
		`>Members</button>`,
		`>Sign out</button>`,
		`<h1>Projects</h1>`,
		`>Refresh</button>`,
		`<h2>Files</h2>`,
		`<h2>Preview</h2>`,
		`<h2>Online</h2>`,
		`<h2>Alerts</h2>`,
		`>Clear</button>`,
	} {
		if strings.Contains(body, gone) {
			t.Errorf("GUI still has English chrome %s", gone)
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
	if !strings.Contains(dash, seedIndexHTML) {
		t.Error("dashboard.js starter page no longer matches seedIndexHTML")
	}
	if !strings.Contains(dash, "/snapshot") {
		t.Error("dashboard.js must read the starter page before previewing it")
	}
	for _, marker := range []string{
		"还没有可预览的内容",
		"目前只有起始页",
		"复制命令",
	} {
		if !strings.Contains(dash, marker) {
			t.Errorf("dashboard.js missing empty-state copy %q", marker)
		}
	}
	teamsJS := getGUIAsset(t, "/teams.js")
	for _, marker := range []string{
		"Team ID: ",
		"Invite code: ",
		`团队\\s*id`,
		"邀请码",
		"parseInvitePair",
		"addEventListener(\"paste\"",
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
