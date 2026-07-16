# Account and Multi-Team Web GUI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the manual API token/session settings screen with username/password authentication, team onboarding/switching, role-aware member management, and team-scoped cowork dashboard requests.

**Architecture:** The GUI remains build-free vanilla HTML/CSS/JavaScript. Small focused modules own Cookie/CSRF API access, auth screens, team screens, member administration, and the existing project dashboard. All server-controlled strings enter the DOM through `textContent`.

**Tech Stack:** Embedded static HTML/CSS/ES2018 JavaScript, same-origin Cookie sessions, Fetch API, native WebSocket, Go static asset tests, browser E2E.

## Global Constraints

- Requires Plans 1 and 2 completed.
- No frontend build system or package manager.
- No auth credential in localStorage, DOM, or URL.
- Only non-sensitive `current_team_id` may be stored in localStorage.
- All mutating Cookie requests send `X-CSRF-Token`.
- All server/user strings use `textContent`; never concatenate them into `innerHTML`.
- WebSocket uses Cookie auth and contains no `token=` query.
- Preview URL is `/preview/{team_id}/{project_id}/`.
- Every task ends with Go asset/API tests and a focused commit.

---

## File Structure

- `web/index.html`: auth, onboarding, team switcher, member panel, dashboard markup.
- `web/style.css`: states and responsive layout.
- `web/api.js`: same-origin fetch, CSRF cookie, 401 handling.
- `web/auth.js`: login/register/logout/me state.
- `web/teams.js`: list/create/join/switch and member administration.
- `web/dashboard.js`: team-scoped projects/tree/locks/agents/WS.
- `web/app.js`: page state and module orchestration only.
- `web/embed.go`: embed every module.
- `pkg/api/gui_test.go`: static marker and protected API regression tests.
- `pkg/api/gui_auth_e2e_test.go`: server-side GUI auth/team flow fixtures.

---

### Task 1: API module and login/register shell

**Files:**
- Create: `web/api.js`
- Create: `web/auth.js`
- Modify: `web/index.html`
- Modify: `web/app.js`
- Modify: `web/style.css`
- Modify: `web/embed.go`
- Modify: `pkg/api/gui_test.go`

**Interfaces:**
- Produces:
  - `window.CoworkAPI.request(path, options)`
  - `window.CoworkAuth.bootstrap()`
  - `window.CoworkAuth.login(username, password)`
  - `window.CoworkAuth.register(username, password)`
  - `window.CoworkAuth.changePassword(currentPassword, newPassword)`
  - `window.CoworkAuth.logout()`

- [ ] **Step 1: Write failing static GUI tests**

Add assertions:

```go
func TestServeGUIContainsAuthViews(t *testing.T) {
	body := getGUIAsset(t, "/")
	for _, marker := range []string{
		`id="view-auth"`,
		`id="form-login"`,
		`id="form-register"`,
		`id="input-login-username"`,
		`id="input-register-password"`,
		`id="form-change-password"`,
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
```

- [ ] **Step 2: Run tests and verify RED**

```bash
go test ./pkg/api/... -run 'TestServeGUIContainsAuthViews|TestServeGUINoLongerContainsTokenInputs' -count=1
```

Expected: auth markers are missing and token input still exists.

- [ ] **Step 3: Replace settings markup with auth views**

`index.html` must contain:

```html
<main id="view-auth" class="auth-view">
  <section class="auth-card">
    <h1>Sign in to cowork</h1>
    <form id="form-login">
      <label>Username
        <input id="input-login-username" autocomplete="username" required>
      </label>
      <label>Password
        <input id="input-login-password" type="password"
               autocomplete="current-password" minlength="10" required>
      </label>
      <button type="submit">Sign in</button>
    </form>
    <form id="form-register" class="hidden">
      <label>Username
        <input id="input-register-username" autocomplete="username" required>
      </label>
      <label>Password
        <input id="input-register-password" type="password"
               autocomplete="new-password" minlength="10" required>
      </label>
      <label>Confirm password
        <input id="input-register-confirm" type="password"
               autocomplete="new-password" minlength="10" required>
      </label>
      <button type="submit">Create account</button>
    </form>
    <form id="form-change-password" class="hidden">
      <label>Current or temporary password
        <input id="input-current-password" type="password"
               autocomplete="current-password" required>
      </label>
      <label>New password
        <input id="input-new-password" type="password"
               autocomplete="new-password" minlength="10" required>
      </label>
      <button type="submit">Change password</button>
    </form>
  </section>
</main>
```

Remove token/session settings controls.

- [ ] **Step 4: Implement Cookie/CSRF fetch wrapper**

`web/api.js`:

```js
(function () {
  "use strict";

  function cookie(name) {
    var prefix = name + "=";
    return document.cookie.split(";").map(function (v) { return v.trim(); })
      .filter(function (v) { return v.indexOf(prefix) === 0; })
      .map(function (v) { return decodeURIComponent(v.slice(prefix.length)); })[0] || "";
  }

  function request(path, options) {
    options = options || {};
    var method = (options.method || "GET").toUpperCase();
    var headers = Object.assign({}, options.headers || {});
    if (options.body && !headers["Content-Type"]) headers["Content-Type"] = "application/json";
    if (["POST", "PATCH", "DELETE"].indexOf(method) >= 0) {
      headers["X-CSRF-Token"] = cookie("al_csrf");
    }
    return fetch(path, Object.assign({}, options, {
      method: method,
      headers: headers,
      credentials: "same-origin"
    })).then(function (response) {
      return response.json().catch(function () { return {}; }).then(function (body) {
        if (response.status === 401) {
          window.dispatchEvent(new CustomEvent("cowork:unauthorized"));
        }
        return { ok: response.ok, status: response.status, body: body };
      });
    });
  }

  window.CoworkAPI = { request: request };
})();
```

- [ ] **Step 5: Implement auth module**

`auth.js` calls `/api/auth/me` on bootstrap, rotates CSRF through that endpoint, and exposes events:

```js
window.dispatchEvent(new CustomEvent("cowork:authenticated", { detail: me }));
window.dispatchEvent(new CustomEvent("cowork:signed-out"));
```

Registration and login bodies are exactly:

```js
JSON.stringify({ username: username, password: password })
```

When login/me returns `must_change_password=true`, hide normal app/onboarding views and show only `form-change-password`. On success, the server revokes the current session, so clear state and show the login form again. On logout call `POST /api/auth/logout`; then clear in-memory user/team state.

- [ ] **Step 6: Run tests and commit**

```bash
gofmt -w web/embed.go pkg/api/gui_test.go
go test ./pkg/api/... -run TestServeGUI -count=1 -race
git add web/index.html web/style.css web/app.js web/api.js web/auth.js web/embed.go pkg/api/gui_test.go
git commit -m "feat(web): add account login and registration"
```

Expected: static tests pass and no token UI remains.

---

### Task 2: Team onboarding and persistent team switcher

**Files:**
- Create: `web/teams.js`
- Modify: `web/index.html`
- Modify: `web/app.js`
- Modify: `web/style.css`
- Modify: `web/embed.go`
- Create: `pkg/api/gui_auth_e2e_test.go`

**Interfaces:**
- Consumes: `CoworkAPI`, authenticated user event.
- Produces:
  - `CoworkTeams.load()`
  - `CoworkTeams.create(name)`
  - `CoworkTeams.join(teamID, inviteCode)`
  - `CoworkTeams.select(teamID)`
  - `CoworkTeams.current()`

- [ ] **Step 1: Write failing server-side flow test**

```go
func TestGUIAuthTeamOnboardingFlow(t *testing.T) {
	alice := registerBrowserUser(t, "gui_alice", "long-enough-password")
	team, invite := createBrowserTeam(t, alice, "Product")
	bob := registerBrowserUser(t, "gui_bob", "another-long-password")
	joinBrowserTeam(t, bob, team.ID, invite)

	assertTeamList(t, alice, team.ID)
	assertTeamList(t, bob, team.ID)
}
```

This uses real Cookie + CSRF requests against the test server.

- [ ] **Step 2: Run test and verify backend flow is available**

```bash
go test ./pkg/api/... -run TestGUIAuthTeamOnboardingFlow -count=1 -race
```

Expected: PASS if Plans 1–2 are complete. This establishes the backend contract before UI wiring.

- [ ] **Step 3: Add onboarding markup**

Add:

```html
<main id="view-onboarding" class="view hidden">
  <section class="onboarding-card">
    <h1>Choose a team</h1>
    <form id="form-create-team">
      <input id="input-team-name" placeholder="Team name" required>
      <button type="submit">Create team</button>
    </form>
    <form id="form-join-team">
      <input id="input-team-id" placeholder="tm_xxxxxxxx" required>
      <input id="input-invite-code" type="password" placeholder="Invite code" required>
      <button type="submit">Join team</button>
    </form>
  </section>
</main>
```

Add a top-bar `<select id="team-switcher">` and create/join buttons.

- [ ] **Step 4: Implement team state**

Use only:

```js
var LS_TEAM = "cowork_current_team_id";
```

Never store invite codes. `load()` fetches `/api/teams`, chooses the stored ID only if it is still in the response, otherwise selects the first team. `select()` emits:

```js
window.dispatchEvent(new CustomEvent("cowork:team-changed", {
  detail: { team: selectedTeam }
}));
```

Creating a team shows its invite code in a modal once, with a copy button implemented through `navigator.clipboard.writeText`.

- [ ] **Step 5: Orchestrate view state**

`app.js` rules:

- No user → auth view.
- User with zero teams → onboarding.
- User with teams → dashboard and team switcher.
- `cowork:unauthorized` → call `CoworkAuth.bootstrap`; if still 401 show auth view.
- Team change → disconnect old WS, clear project view, reload dashboard data.

- [ ] **Step 6: Verify and commit**

```bash
go test ./pkg/api/... -run 'TestGUIAuthTeamOnboardingFlow|TestServeGUI' -count=1 -race
git add web/index.html web/style.css web/app.js web/teams.js web/embed.go pkg/api/gui_auth_e2e_test.go
git commit -m "feat(web): add team onboarding and switching"
```

Expected: authenticated users can create/join/switch teams without storing secrets.

---

### Task 3: Role-aware member administration

**Files:**
- Modify: `web/teams.js`
- Modify: `web/index.html`
- Modify: `web/style.css`
- Modify: `pkg/api/gui_auth_e2e_test.go`

**Interfaces:**
- Consumes: current team and Plan 1 member endpoints.
- Produces: member panel, invite rotation, role changes, removal, ownership transfer.

- [ ] **Step 1: Add failing API role-flow test**

```go
func TestGUIRoleAdministrationFlow(t *testing.T) {
	owner, admin, member, team := setupThreeMemberTeam(t)
	changeRoleAs(t, owner, team.ID, admin.UserID, "admin", http.StatusOK)
	changeRoleAs(t, admin, team.ID, member.UserID, "admin", http.StatusForbidden)
	removeMemberAs(t, admin, team.ID, member.UserID, http.StatusOK)
	transferOwnerAs(t, owner, team.ID, admin.UserID, http.StatusOK)
}
```

Also add `TestGUIAdminAndMemberCanLeaveTeam` and assert an Owner receives 409/403 until ownership is transferred.

- [ ] **Step 2: Run test**

```bash
go test ./pkg/api/... -run TestGUIRoleAdministrationFlow -count=1 -race
```

Expected: PASS against completed team APIs.

- [ ] **Step 3: Add safe member rendering**

Create DOM nodes only:

```js
function memberRow(member, currentRole) {
  var row = document.createElement("li");
  var name = document.createElement("span");
  name.textContent = member.username;
  var role = document.createElement("span");
  role.textContent = member.role;
  row.appendChild(name);
  row.appendChild(role);
  appendAllowedActions(row, member, currentRole);
  return row;
}
```

No member or team string may be assigned through `innerHTML`.

- [ ] **Step 4: Wire role-aware actions**

- Owner: promote/demote Admin, remove Admin/Member, transfer ownership.
- Admin: remove Member, rotate invite.
- Member: read-only member list.
- Admin/Member: “Leave team”; Owner sees “Transfer ownership before leaving.”
- Hide controls client-side for UX, but rely on server 403 for security.
- Require confirmation before remove/transfer.
- Invite rotation shows the new code once and never stores it.

- [ ] **Step 5: Verify and commit**

```bash
go test ./pkg/api/... -run 'TestGUIRoleAdministrationFlow|TestTeam' -count=1 -race
git add web/index.html web/style.css web/teams.js pkg/api/gui_auth_e2e_test.go
git commit -m "feat(web): add team member administration"
```

---

### Task 4: Migrate the cowork dashboard to team APIs

**Files:**
- Create: `web/dashboard.js`
- Modify: `web/app.js`
- Modify: `web/index.html`
- Modify: `web/style.css`
- Modify: `web/embed.go`
- Modify: `pkg/api/gui_test.go`

**Interfaces:**
- Consumes: current team from `CoworkTeams`.
- Produces: team projects/tree/locks/agents/WS/preview dashboard.

- [ ] **Step 1: Write failing asset regression tests**

Add:

```go
func TestServeGUIUsesV2TeamRoutes(t *testing.T) {
	js := getGUIAsset(t, "/dashboard.js")
	for _, required := range []string{
		`"/api/teams/"`,
		`"/projects"`,
		`"/locks/acquire"`,
		`"/agents"`,
		`"/ws?project="`,
	} {
		if !strings.Contains(js, required) {
			t.Errorf("dashboard.js missing %q", required)
		}
	}
	for _, forbidden := range []string{"Authorization", "cowork_token", "token="} {
		if strings.Contains(js, forbidden) {
			t.Errorf("dashboard.js still contains legacy credential pattern %q", forbidden)
		}
	}
}
```

- [ ] **Step 2: Run tests and verify RED**

```bash
go test ./pkg/api/... -run TestServeGUIUsesV2TeamRoutes -count=1
```

Expected: FAIL because `dashboard.js` is absent.

- [ ] **Step 3: Implement team URL builder**

```js
function teamURL(suffix) {
  var team = window.CoworkTeams.current();
  if (!team) throw new Error("no active team");
  return "/api/teams/" + encodeURIComponent(team.id) + suffix;
}
```

Migrate:

```text
GET  teamURL("/projects")
GET  teamURL("/projects/<project>/tree")
POST teamURL("/locks/acquire")
POST teamURL("/locks/release")
GET  teamURL("/agents")
WS   teamURL("/ws?project=<project>")
```

Lock bodies are `{project_id, path}`; session and owner are not client fields.

- [ ] **Step 4: Migrate preview and WS**

Preview (transitional path while v1 `/preview/{id}` still exists; drop the
`teams/` segment at the Plan 4 cutover):

```js
previewFrame.src = "/preview/teams/" +
  encodeURIComponent(team.id) + "/" +
  encodeURIComponent(project.id) + "/";
```

WebSocket:

```js
var protocol = location.protocol === "https:" ? "wss:" : "ws:";
var ws = new WebSocket(protocol + "//" + location.host +
  teamURL("/ws?project=" + encodeURIComponent(project.id)));
```

Do not append credentials.

- [ ] **Step 5: Preserve XSS-safe rendering**

Project name, path, owner label, username, message, and task title must use `textContent`. The only allowed `innerHTML` writes are clearing a container with `element.innerHTML = ""` or static literals containing no server data.

- [ ] **Step 6: Verify and commit**

```bash
gofmt -w web/embed.go pkg/api/gui_test.go
go test ./pkg/api/... -run 'TestServeGUI|TestGUI' -count=1 -race
git add web/dashboard.js web/app.js web/index.html web/style.css web/embed.go pkg/api/gui_test.go
git commit -m "feat(web): migrate dashboard to team sessions"
```

Expected: the GUI contains no manual credential path.

---

### Task 5: Browser end-to-end verification

**Files:**
- Modify: `pkg/api/e2e_v2_test.go`
- Create: `docs/manual-team-auth-e2e.md`

**Interfaces:**
- Verifies Plans 1–3 as a user-visible flow.

- [ ] **Step 1: Add server E2E auth regression**

Add:

```go
func TestE2EV2BrowserJourney(t *testing.T) {
	alice := registerBrowserUser(t, "journey_alice", "correct horse 123")
	team, invite := createBrowserTeam(t, alice, "Journey Team")
	bob := registerBrowserUser(t, "journey_bob", "correct battery 456")
	joinBrowserTeam(t, bob, team.ID, invite)
	project := createProjectV2(t, alice, team.ID, "Prototype")
	lockAndApplyV2(t, bob, team.ID, project.ID, "index.html", "<h1>joined</h1>")
	assertPreviewV2(t, alice, team.ID, project.ID, "<h1>joined</h1>")
}
```

- [ ] **Step 2: Run automated E2E**

```bash
go test ./pkg/api/... -run TestE2EV2BrowserJourney -count=1 -race -v
```

Expected: PASS.

- [ ] **Step 3: Write manual browser checklist**

The checklist must include:

1. Register Alice.
2. Create team and copy invite once.
3. Open private window, register Bob, join team.
4. Alice promotes Bob to Admin.
5. Bob creates a project and acquires a lock.
6. Alice sees project/lock update over WS.
7. Cross-team URL returns 404.
8. Logout returns to auth page.
9. Browser storage contains only `cowork_current_team_id`, never credentials.

- [ ] **Step 4: Run browser automation**

Use a browser testing agent against a locally running server to complete the checklist. Capture screenshots of login, team onboarding, dashboard, and member management. Record defects in the implementation task; do not weaken the checklist.

- [ ] **Step 5: Commit**

```bash
git add pkg/api/e2e_v2_test.go docs/manual-team-auth-e2e.md
git commit -m "test(web): verify account and team browser journey"
```

---

## Plan 3 Completion Gate

Run:

```bash
go test ./pkg/auth/... ./pkg/api/... -count=1 -race
go build ./...
```

Then complete `docs/manual-team-auth-e2e.md`.

Expected:

- GUI registration/login works with Cookie + CSRF.
- Users create/join/switch teams.
- Roles control member UI and server actions.
- Dashboard and preview use team URLs.
- No API key, Device Session, or session name is visible or required in the GUI.
