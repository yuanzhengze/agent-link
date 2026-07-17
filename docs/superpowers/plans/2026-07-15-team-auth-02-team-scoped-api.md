# Team-Scoped Cowork API Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move projects, locks, apply/snapshot/tree, WebSocket, preview, agents, messages, and tasks into strict team scope using the authenticated Actor from Plan 1.

**Architecture:** Every business route lives under `/api/teams/{team_id}/...`. The team middleware resolves membership from the URL, project handlers additionally verify project ownership, and all Redis indexes use `agentlink:v2:*`. Browser WebSocket/preview use the HttpOnly session Cookie; CLI WebSocket uses the Device Session authorization header.

**Tech Stack:** Go 1.24, net/http ServeMux path values, Redis 9, coder/websocket, server-owned Git work trees.

## Global Constraints

- Requires Plan 1 completed and green.
- Source of truth: `docs/superpowers/specs/2026-07-15-team-auth-design.md`.
- Never infer team from a client-side “current team”; use the URL `team_id`.
- Cross-team project access returns 404, not 403.
- Browser Actor uses `device=web`, `session=gui`; Device clients send `X-Agentlink-Session`.
- All new Redis keys use `agentlink:v2:*`.
- Work trees live at `DATA_DIR/work/<team_id>/<project_id>`.
- Retain server-as-sole-Git-committer and existing path-safety rules.
- Every task uses TDD, `-race`, and a focused commit.

---

## File Structure

- `pkg/api/tenant_v2.go`: team/project key helpers, project access guard, Actor session enrichment.
- `pkg/api/projects_v2.go`: team-scoped project create/list/tree/snapshot.
- `pkg/api/locks_v2.go`: team-scoped lock acquire/release/list.
- `pkg/api/apply_v2.go`: team-scoped apply and event model.
- `pkg/api/ws_v2.go`: authenticated team/project subscriptions.
- `pkg/api/preview_v2.go`: authenticated team preview and live reload.
- `pkg/api/agents_v2.go`: team device presence/list.
- `pkg/api/messages_v2.go`: team-scoped messaging/inbox.
- `pkg/api/tasks_v2.go`: team-scoped task lifecycle.
- Matching `*_test.go` files test each unit.
- `pkg/api/e2e_v2_test.go`: cross-team and same-team end-to-end tests.

---

### Task 1: Team-scoped project model and Git work trees

**Files:**
- Create: `pkg/api/tenant_v2.go`
- Create: `pkg/api/projects_v2.go`
- Create: `pkg/api/projects_v2_test.go`
- Modify: `pkg/api/server.go`

**Interfaces:**
- Consumes: Plan 1 `ActorFromContext` and team middleware.
- Produces:
  - `projectV2Key`, `teamProjectsV2Key`, `projectDirV2`.
  - `loadTeamProject(ctx, teamID, projectID)`.
  - team-scoped project routes.

- [ ] **Step 1: Write failing project-isolation tests**

Create:

```go
func TestV2ProjectCreateAndListAreTeamScoped(t *testing.T)
func TestV2ProjectListDoesNotLeakOtherTeam(t *testing.T)
func TestV2ProjectIDFromOtherTeamReturns404(t *testing.T)
func TestV2ProjectWorkTreesUseTeamDirectory(t *testing.T)
```

The leakage test creates one user in two teams, creates one project per team, and asserts each list returns exactly its own project.

- [ ] **Step 2: Run tests and verify RED**

```bash
go test ./pkg/api/... -run 'TestV2Project' -count=1 -race
```

Expected: FAIL because v2 project routes are absent.

- [ ] **Step 3: Implement project keys and access guard**

```go
func projectV2Key(projectID string) string {
	return "agentlink:v2:project:" + projectID
}

func teamProjectsV2Key(teamID string) string {
	return "agentlink:v2:team:" + teamID + ":projects"
}

func (s *Server) projectDirV2(teamID, projectID string) string {
	return filepath.Join(s.dataDir, "work", teamID, projectID)
}

type TeamProject struct {
	ID         string `json:"id"`
	TeamID     string `json:"team_id"`
	Name       string `json:"name"`
	CreatedAt  string `json:"created_at"`
	HeadCommit string `json:"head_commit"`
}
```

`loadTeamProject` loads the project hash and returns `errProjectNotFound` whenever the hash is missing or `team_id != teamID`.

- [ ] **Step 4: Implement project create/list**

Register:

```go
s.mux.HandleFunc("POST /api/teams/{team_id}/projects", s.handleCreateProjectV2)
s.mux.HandleFunc("GET /api/teams/{team_id}/projects", s.handleListProjectsV2)
```

Create the Git work tree under `work/<team>/<project>`, commit seed `index.html`, then atomically write the project hash and add it to the team project set. If Redis persistence fails, remove the newly created work tree.

- [ ] **Step 5: Run tests and commit**

```bash
gofmt -w pkg/api/tenant_v2.go pkg/api/projects_v2.go pkg/api/projects_v2_test.go pkg/api/server.go
go test ./pkg/api/... -run 'TestV2Project' -count=1 -race
git add pkg/api/tenant_v2.go pkg/api/projects_v2.go pkg/api/projects_v2_test.go pkg/api/server.go
git commit -m "feat(api): scope projects and work trees by team"
```

Expected: all v2 project tests pass.

---

### Task 2: Team-scoped lock service and Actor ownership

**Files:**
- Create: `pkg/api/locks_v2.go`
- Create: `pkg/api/locks_v2_test.go`
- Modify: `pkg/api/auth_v2_middleware.go`
- Modify: `pkg/api/server.go`

**Interfaces:**
- Consumes: team Actor, `loadTeamProject`.
- Produces:
  - lock routes under `/api/teams/{team_id}/locks`.
  - `lockOwnerV2`.
  - structured owner metadata.

- [ ] **Step 1: Write failing lock tests**

Create:

```go
func TestV2LocksSamePathAreIndependentAcrossTeams(t *testing.T)
func TestV2LockOwnerUsesAuthenticatedActor(t *testing.T)
func TestV2LockRejectsProjectFromOtherTeam(t *testing.T)
func TestV2WebLockUsesWebGUISession(t *testing.T)
func TestV2DeviceLockRequiresValidSessionHeader(t *testing.T)
```

The ownership test sends a malicious `owner` field in JSON and asserts the server ignores it.

- [ ] **Step 2: Run tests and verify RED**

```bash
go test ./pkg/api/... -run 'TestV2Lock' -count=1 -race
```

Expected: FAIL because v2 lock handlers are absent.

- [ ] **Step 3: Enrich Device Actor session safely**

For Device-authenticated business requests:

```go
session := r.Header.Get("X-Agentlink-Session")
if session != "" && !deviceNameRE.MatchString(session) {
	writeError(w, http.StatusBadRequest, "invalid agent session")
	return
}
actor.SessionName = session
```

For Web-authenticated requests:

```go
actor.DeviceID = "web"
actor.DeviceName = "web"
actor.SessionName = "gui"
```

Lock/apply endpoints require a non-empty session after this enrichment; project listing does not.

- [ ] **Step 4: Implement v2 lock keys and scripts**

```go
func lockV2Key(teamID, projectID, path string) string {
	return "agentlink:v2:lock:" + teamID + ":" + projectID + ":" + path
}

func actorLocksV2Key(teamID string, actor auth.Actor) string {
	return "agentlink:v2:locks:" + teamID + ":" + actor.UserID + ":" +
		actor.DeviceID + ":" + actor.SessionName
}

type LockOwnerV2 struct {
	UserID      string `json:"user_id"`
	Username    string `json:"username"`
	DeviceID    string `json:"device_id"`
	DeviceName  string `json:"device_name"`
	SessionName string `json:"session_name"`
	Label       string `json:"label"`
}
```

Store owner fields directly in the lock hash. Lua acquire/release rules stay atomic, including expired-owner set cleanup and force release. A normal release succeeds only when all stable owner IDs match.

- [ ] **Step 5: Register routes**

```go
s.mux.HandleFunc("POST /api/teams/{team_id}/locks/acquire", s.handleLockAcquireV2)
s.mux.HandleFunc("POST /api/teams/{team_id}/locks/release", s.handleLockReleaseV2)
s.mux.HandleFunc("GET /api/teams/{team_id}/locks", s.handleLockListV2)
```

Request bodies no longer accept `session` or owner identity:

```go
type LockAcquireRequestV2 struct {
	ProjectID string `json:"project_id"`
	Path      string `json:"path"`
	TaskID    string `json:"task_id,omitempty"`
}
```

- [ ] **Step 6: Run tests and commit**

```bash
gofmt -w pkg/api
go test ./pkg/api/... -run 'TestV2Lock' -count=1 -race
git add pkg/api/locks_v2.go pkg/api/locks_v2_test.go pkg/api/auth_v2_middleware.go pkg/api/server.go
git commit -m "feat(api): add team-scoped actor-owned locks"
```

Expected: lock isolation and owner tests pass.

---

### Task 3: Team-scoped apply, tree, and atomic snapshot

**Files:**
- Create: `pkg/api/apply_v2.go`
- Create: `pkg/api/apply_v2_test.go`
- Modify: `pkg/api/projects_v2.go`
- Modify: `pkg/api/projects_v2_test.go`
- Modify: `pkg/api/server.go`

**Interfaces:**
- Consumes: v2 project and lock accessors.
- Produces:
  - v2 apply/tree/snapshot routes.
  - `EventV2`.
  - `BroadcasterV2`.

- [ ] **Step 1: Write failing apply and read tests**

Create:

```go
func TestV2ApplyRequiresActorLock(t *testing.T)
func TestV2ApplyRejectsCrossTeamProjectWith404(t *testing.T)
func TestV2ApplyCommitsAndUpdatesTeamProjectHead(t *testing.T)
func TestV2TreeShowsStructuredLockOwner(t *testing.T)
func TestV2SnapshotIsAtomicWithApply(t *testing.T)
func TestV2ApplyRejectsCaseInsensitiveGitTraversal(t *testing.T)
```

- [ ] **Step 2: Run tests and verify RED**

```bash
go test ./pkg/api/... -run 'TestV2Apply|TestV2Tree|TestV2Snapshot' -count=1 -race
```

Expected: FAIL because handlers do not exist.

- [ ] **Step 3: Define v2 event and apply contract**

```go
type EventV2 struct {
	Type       string       `json:"type"`
	TeamID     string       `json:"team_id"`
	ProjectID  string       `json:"project_id"`
	Path       string       `json:"path,omitempty"`
	Content    string       `json:"content,omitempty"`
	HeadCommit string       `json:"head_commit,omitempty"`
	By         LockOwnerV2  `json:"by,omitempty"`
	Owner      *LockOwnerV2 `json:"owner,omitempty"`
	At         string       `json:"at"`
}

type BroadcasterV2 interface {
	Broadcast(teamID, projectID string, event EventV2)
}

type ApplyRequestV2 struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}
```

- [ ] **Step 4: Implement apply**

Register:

```go
s.mux.HandleFunc(
	"POST /api/teams/{team_id}/projects/{project_id}/apply",
	s.handleApplyV2,
)
```

Order:

1. Load project and return 404 on mismatch.
2. Validate `safeApplyPath`.
3. Verify v2 lock belongs to Actor and lease is active.
4. Lock `projectLock(teamID + ":" + projectID)`.
5. Write, Git commit, update v2 project `head_commit`.
6. Broadcast `file_changed`.

- [ ] **Step 5: Implement tree and snapshot**

Register:

```go
s.mux.HandleFunc("GET /api/teams/{team_id}/projects/{project_id}/tree", s.handleTreeV2)
s.mux.HandleFunc("GET /api/teams/{team_id}/projects/{project_id}/snapshot", s.handleSnapshotV2)
```

Reuse regular-file-only walking and case-insensitive `.git` exclusion. Snapshot must hold the same project mutex across reading `head_commit` and every file.

- [ ] **Step 6: Run tests and commit**

```bash
gofmt -w pkg/api
go test ./pkg/api/... -run 'TestV2Apply|TestV2Tree|TestV2Snapshot' -count=1 -race
git add pkg/api/apply_v2.go pkg/api/apply_v2_test.go pkg/api/projects_v2.go pkg/api/projects_v2_test.go pkg/api/server.go
git commit -m "feat(api): add team-scoped apply tree and snapshot"
```

Expected: all v2 file workflow tests pass.

---

### Task 4: Authenticated team WebSocket and preview

**Files:**
- Create: `pkg/api/ws_v2.go`
- Create: `pkg/api/ws_v2_test.go`
- Create: `pkg/api/preview_v2.go`
- Create: `pkg/api/preview_v2_test.go`
- Modify: `pkg/api/server.go`

**Interfaces:**
- Consumes: `EventV2`, Web/Device authentication, team/project guard.
- Produces:
  - `/api/teams/{team_id}/ws?project=<id>`.
  - `/preview/{team_id}/{project_id}/{path...}`.

- [ ] **Step 1: Write failing WS/preview tests**

Create:

```go
func TestV2WSBrowserCookieReceivesOnlyOwnTeam(t *testing.T)
func TestV2WSDeviceAuthorizationHeader(t *testing.T)
func TestV2WSRejectsNonMember(t *testing.T)
func TestV2WSRejectsProjectFromOtherTeam(t *testing.T)
func TestV2PreviewRequiresMembership(t *testing.T)
func TestV2PreviewLiveReloadUsesCookieWSWithoutTokenQuery(t *testing.T)
```

The live-reload test must assert the injected script contains the team/project URL and does not contain `token=`.

- [ ] **Step 2: Run tests and verify RED**

```bash
go test ./pkg/api/... -run 'TestV2WS|TestV2Preview' -count=1 -race
```

Expected: FAIL because v2 routes are absent.

- [ ] **Step 3: Implement subscription isolation**

Key Hub subscriptions by a non-ambiguous pair:

```go
type subscriptionKey struct {
	TeamID    string
	ProjectID string
}
```

Use:

```go
func (h *HubV2) Broadcast(teamID, projectID string, event EventV2)
```

Snapshot the connection set under `RLock`, then fan out writes in parallel with the existing bounded timeout behavior.

- [ ] **Step 4: Authenticate WS before upgrade**

Route:

```go
s.mux.HandleFunc("GET /api/teams/{team_id}/ws", s.handleWSV2)
```

`handleWSV2` accepts:

- Browser `al_session` Cookie.
- `Authorization: Device <credential>` request header.

Then it validates team membership and project ownership before `websocket.Accept`.

- [ ] **Step 5: Implement protected preview**

Route:

```go
// Transitional: the canonical target is /preview/{team_id}/{project_id}/{path...}
// but that would shadow the still-live v1 /preview/{id}/{path...} route (Go's
// ServeMux picks the more specific v2 pattern for any >=2-segment path, so v1
// preview breaks). The literal "teams/" segment keeps the routes disjoint until
// Plan 4 removes v1 /preview, at which point this drops to the canonical form.
// Project ids are random ("p_..."), never "teams", so no collision is possible.
s.mux.HandleFunc(
	"GET /preview/teams/{team_id}/{project_id}/{path...}",
	s.handlePreviewV2,
)
```

Require Web Session membership. Keep path traversal protection and HTML-only injection. Inject:

```js
new WebSocket(
  (location.protocol === "https:" ? "wss:" : "ws:") +
  "//" + location.host +
  "/api/teams/" + encodeURIComponent(teamID) +
  "/ws?project=" + encodeURIComponent(projectID)
)
```

- [ ] **Step 6: Run tests and commit**

```bash
gofmt -w pkg/api
go test ./pkg/api/... -run 'TestV2WS|TestV2Preview' -count=1 -race
git add pkg/api/ws_v2.go pkg/api/ws_v2_test.go pkg/api/preview_v2.go pkg/api/preview_v2_test.go pkg/api/server.go
git commit -m "feat(api): secure team websocket and preview"
```

Expected: no user credential appears in a WS query string.

---

### Task 5: Team-scoped agents and presence

**Files:**
- Create: `pkg/api/agents_v2.go`
- Create: `pkg/api/agents_v2_test.go`
- Modify: `pkg/api/server.go`

**Interfaces:**
- Produces:
  - team heartbeat, agent list, session patch/delete, device logout.

- [ ] **Step 1: Write failing agent tests**

```go
func TestV2AgentListOnlyShowsTeamMembersDevices(t *testing.T)
func TestV2HeartbeatRecordsActorDeviceInTeam(t *testing.T)
func TestV2BrowserActorAppearsAsWebGUI(t *testing.T)
func TestV2AgentCannotMutateOtherUsersDevice(t *testing.T)
```

- [ ] **Step 2: Run tests and verify RED**

```bash
go test ./pkg/api/... -run TestV2Agent -count=1 -race
```

Expected: FAIL because v2 agent routes do not exist.

- [ ] **Step 3: Implement routes and keys**

Register:

```go
s.mux.HandleFunc("POST /api/teams/{team_id}/agents/heartbeat", s.handleHeartbeatV2)
s.mux.HandleFunc("GET /api/teams/{team_id}/agents", s.handleListAgentsV2)
s.mux.HandleFunc("PATCH /api/teams/{team_id}/agents/sessions", s.handlePatchSessionsV2)
s.mux.HandleFunc("DELETE /api/teams/{team_id}/agents/sessions", s.handleDeleteSessionV2)
```

Use team device index:

```text
agentlink:v2:team:<team_id>:devices
```

Only the Actor's own `device_id` may mutate sessions.

- [ ] **Step 4: Run tests and commit**

```bash
gofmt -w pkg/api
go test ./pkg/api/... -run TestV2Agent -count=1 -race
git add pkg/api/agents_v2.go pkg/api/agents_v2_test.go pkg/api/server.go
git commit -m "feat(api): scope device presence by team"
```

---

### Task 6: Team-scoped messaging and tasks

**Files:**
- Create: `pkg/api/messages_v2.go`
- Create: `pkg/api/messages_v2_test.go`
- Create: `pkg/api/tasks_v2.go`
- Create: `pkg/api/tasks_v2_test.go`
- Modify: `pkg/api/server.go`

**Interfaces:**
- Consumes: authenticated team Actor and team device/session index.
- Produces: v2 message/inbox/task routes and v2 Redis keys.

- [ ] **Step 1: Write failing isolation tests**

Create:

```go
func TestV2MessageCannotTargetDeviceOutsideTeam(t *testing.T)
func TestV2InboxIsIsolatedByTeam(t *testing.T)
func TestV2TaskCannotTargetSessionOutsideTeam(t *testing.T)
func TestV2TaskListsAreIsolatedByTeam(t *testing.T)
func TestV2TaskFromIdentityComesFromActor(t *testing.T)
```

- [ ] **Step 2: Run tests and verify RED**

```bash
go test ./pkg/api/... -run 'TestV2Message|TestV2Inbox|TestV2Task' -count=1 -race
```

Expected: FAIL because v2 routes are absent.

- [ ] **Step 3: Define team keys**

```go
func inboxV2Key(teamID, deviceID, session string) string {
	return "agentlink:v2:inbox:" + teamID + ":" + deviceID + ":" + session
}
func taskV2Key(teamID, taskID string) string {
	return "agentlink:v2:task:" + teamID + ":" + taskID
}
func receivedTasksV2Key(teamID, deviceID, session string) string {
	return "agentlink:v2:tasks:" + teamID + ":" + deviceID + ":" + session
}
func issuedTasksV2Key(teamID, deviceID, session string) string {
	return "agentlink:v2:issued:" + teamID + ":" + deviceID + ":" + session
}
```

- [ ] **Step 4: Register v2 message/task routes**

```go
s.mux.HandleFunc("POST /api/teams/{team_id}/messages", s.handleSendV2)
s.mux.HandleFunc("GET /api/teams/{team_id}/inbox", s.handlePullV2)
s.mux.HandleFunc("POST /api/teams/{team_id}/tasks", s.handleSendTaskV2)
s.mux.HandleFunc("POST /api/teams/{team_id}/tasks/{task_id}/result", s.handleTaskResultV2)
s.mux.HandleFunc("POST /api/teams/{team_id}/tasks/{task_id}/resume", s.handleTaskResumeV2)
s.mux.HandleFunc("POST /api/teams/{team_id}/tasks/{task_id}/cancel", s.handleTaskCancelV2)
s.mux.HandleFunc("POST /api/teams/{team_id}/tasks/{task_id}/reopen", s.handleTaskReopenV2)
s.mux.HandleFunc("GET /api/teams/{team_id}/tasks/{task_id}", s.handleTaskStatusV2)
s.mux.HandleFunc("GET /api/teams/{team_id}/tasks", s.handleTaskListV2)
```

Resolve targets only from the current team's device/session index; never accept `from_device` or `from_session` from JSON.

- [ ] **Step 5: Run tests and commit**

```bash
gofmt -w pkg/api
go test ./pkg/api/... -run 'TestV2Message|TestV2Inbox|TestV2Task' -count=1 -race
git add pkg/api/messages_v2.go pkg/api/messages_v2_test.go pkg/api/tasks_v2.go pkg/api/tasks_v2_test.go pkg/api/server.go
git commit -m "feat(api): isolate messages and tasks by team"
```

Expected: all cross-team target attempts fail without leaking target existence.

---

### Task 7: Cross-team end-to-end API verification

**Files:**
- Create: `pkg/api/e2e_v2_test.go`
- Modify: `pkg/api/handlers_test.go`

**Interfaces:**
- Verifies all outputs of Plans 1 and 2.

- [ ] **Step 1: Add v2 test fixtures**

Provide:

```go
func registerV2User(t *testing.T, username, password string) *http.Cookie
func loginDeviceV2(t *testing.T, username, password, device string) string
func createTeamV2(t *testing.T, cookie *http.Cookie, csrf, name string) (teamID, invite string)
func joinTeamV2(t *testing.T, cookie *http.Cookie, csrf, teamID, invite string)
```

Update cleanup to delete only test-created `agentlink:v2:*` keys in addition to existing v1 cleanup.

- [ ] **Step 2: Write end-to-end tests**

```go
func TestE2EV2TwoUsersSameTeamCollaborate(t *testing.T)
func TestE2EV2TwoTeamsCannotSeeEachOther(t *testing.T)
func TestE2EV2SamePathLocksAreIndependentByTeam(t *testing.T)
func TestE2EV2RemovedMemberLosesAccessImmediately(t *testing.T)
func TestE2EV2WebAndDeviceWSStayInsideTeam(t *testing.T)
```

The cross-team test must exercise list, tree, snapshot, lock, apply, preview, and WS.

- [ ] **Step 3: Run E2E and full API suite**

```bash
go test ./pkg/api/... -run TestE2EV2 -count=1 -race -v
go test ./pkg/auth/... ./pkg/api/... -count=1 -race
```

Expected: all v2 E2E tests pass.

- [ ] **Step 4: Commit**

```bash
git add pkg/api/e2e_v2_test.go pkg/api/handlers_test.go
git commit -m "test(api): verify multi-team isolation end to end"
```

---

## Plan 2 Completion Gate

Run:

```bash
go test ./pkg/auth/... ./pkg/api/... -count=1 -race
go build ./...
go vet ./pkg/auth/... ./pkg/api/...
```

Expected:

- All v2 business APIs require valid account and team membership.
- Project IDs cannot cross team boundaries.
- Locks, messages, tasks, presence, WebSocket, and preview are team-isolated.
- v1 routes still exist only as unreleased compatibility code until Plan 4 deletes them.
