# Account CLI, Sync Migration, and Legacy Cutover Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give CLI users password-based register/login and team selection, migrate every CLI/runtime flow to Device Sessions and team URLs, then remove legacy register-password/API-key authentication before release.

**Architecture:** CLI config stores server, device identity, current team, and local runtime preferences; credentials store one opaque Device Session with mode `0600`. Net clients automatically apply `Authorization: Device`, team-scoped URLs, and the local Agent session header. The final cutover deletes all v1 routes, API-key Redis logic, preview token, and obsolete tests.

**Tech Stack:** Go 1.24, `golang.org/x/term` password input, JSON credentials, TOML-like existing config, coder/websocket headers, Redis v2.

## Global Constraints

- Requires Plans 1–3 completed.
- Before execution, separately commit or set aside the current cowork Task 11 changes (`README.md`, `README_zh.md`, `pkg/api/e2e_test.go`); never mix them into auth commits.
- Passwords must be read from a TTY and never accepted as command-line flags.
- `credentials.json` remains mode `0600`.
- `current_team` is required for team business commands.
- Device Session secrets never appear in logs, query strings, or normal command output.
- `agentlink init` performs no network registration.
- Final release contains no v1 auth route or `REGISTER_PASSWORD`.
- Each task uses RED → GREEN → focused commit.

---

## File Structure

- `pkg/cli/net/config.go`: v2 config and credentials.
- `pkg/cli/net/client.go`: Device authorization, team URL helpers, session header.
- `pkg/cli/net/accounts.go`: register/login/logout.
- `pkg/cli/net/teams.go`: list/create/join/use/member commands.
- `pkg/cli/net/*_test.go`: request/credential/config tests.
- `pkg/cli/runtime/init.go`: local-only initialization.
- `pkg/cli/runtime/init_wizard.go`: remove registration password prompts.
- `pkg/cli/runtime/sync.go`: team API and WS Device Header.
- `pkg/cli/runtime/poller.go`, `resume.go`, `session.go`: team/device-session migration.
- `cmd/agentlink/main.go`: new command dispatch and usage.
- `pkg/api/server.go`, `handlers.go`, v1 cowork files: legacy cutover.
- `README.md`, `README_zh.md`, deployment docs: new user flow.

---

### Task 1: V2 CLI config and Device Session credentials

**Files:**
- Modify: `pkg/cli/net/config.go`
- Create: `pkg/cli/net/config_v2_test.go`
- Modify: `go.mod`
- Modify: `go.sum`

**Interfaces:**
- Produces:
  - v2 `AgentConfig` and `AgentCredentials`.
  - `WriteAccountConfig`, `WriteCredentials`, `SetCurrentTeam`.

- [ ] **Step 1: Add secure terminal dependency**

```bash
go get golang.org/x/term@latest
```

Expected: direct `golang.org/x/term` requirement.

- [ ] **Step 2: Write failing config tests**

```go
func TestLoadConfigV2AllowsLoginWithoutCurrentTeam(t *testing.T)
func TestWriteCredentialsUses0600(t *testing.T)
func TestCredentialsRejectLegacyAPIKeyShape(t *testing.T)
func TestSetCurrentTeamPreservesRuntimeConfig(t *testing.T)
```

The credentials permission test must call `os.Stat` and assert `info.Mode().Perm() == 0o600`.

- [ ] **Step 3: Run tests and verify RED**

```bash
go test ./pkg/cli/net/... -run 'TestLoadConfigV2|TestWriteCredentials|TestCredentialsReject|TestSetCurrentTeam' -count=1
```

Expected: FAIL because v2 fields/helpers are absent.

- [ ] **Step 4: Replace config models**

```go
type AgentConfig struct {
	Server      string
	UserID      string
	Username    string
	DeviceID    string
	Device      string
	CurrentTeam string
	BaseDir     string
	Agent       string
	Poll        PollConfig
	Sessions    map[string]string
}

type AgentCredentials struct {
	DeviceSession string `json:"device_session"`
}
```

`LoadCredentials` rejects an absent `device_session`; it must not accept `api_key`.

`LoadConfig` requires `server`, `device_id`, and `device`, but deliberately permits an empty `current_team` so a newly registered user can run `team create` or `team join`. Only `TeamPath` and team business commands require a selected team.

Expose:

```go
func WriteCredentials(path string, creds AgentCredentials) error
func WriteAccountConfig(path string, cfg AgentConfig) error
func SetCurrentTeam(path, teamID string) error
```

Write temporary files then rename to avoid partial credentials/config writes.

- [ ] **Step 5: Verify and commit**

```bash
gofmt -w pkg/cli/net
go test ./pkg/cli/net/... -run 'TestLoadConfigV2|TestWriteCredentials|TestCredentialsReject|TestSetCurrentTeam' -count=1 -race
git add go.mod go.sum pkg/cli/net/config.go pkg/cli/net/config_v2_test.go
git commit -m "feat(cli): add account and team credential config"
```

---

### Task 2: CLI register, login, logout, and team commands

**Files:**
- Create: `pkg/cli/net/accounts.go`
- Create: `pkg/cli/net/accounts_test.go`
- Create: `pkg/cli/net/teams.go`
- Create: `pkg/cli/net/teams_v2_test.go`
- Modify: `pkg/cli/net/client.go`
- Modify: `cmd/agentlink/main.go`

**Interfaces:**
- Produces:
  - `RunRegister`, `RunLogin`, `RunLogout`.
  - `RunTeamList`, `RunTeamCreate`, `RunTeamJoin`, `RunTeamUse`, `RunTeamMembers`, `RunTeamLeave`.
  - Device-authenticated `APIDo`.

- [ ] **Step 1: Write failing account request tests**

Inject password input:

```go
var readPassword = func(fd int) ([]byte, error) {
	return term.ReadPassword(fd)
}
```

Tests replace it and restore with `t.Cleanup`. Add:

```go
func TestRunRegisterReadsPasswordTwiceAndNeverPrintsIt(t *testing.T)
func TestRunLoginWritesDeviceCredentialAndSelectsTeam(t *testing.T)
func TestRunLoginCompletesRequiredPasswordChangeBeforeSavingCredential(t *testing.T)
func TestRunLogoutRevokesDeviceAndDeletesLocalCredential(t *testing.T)
func TestAPIDoUsesDeviceAuthorization(t *testing.T)
func TestAPIDoWithSessionAddsAgentSessionHeader(t *testing.T)
```

- [ ] **Step 2: Run tests and verify RED**

```bash
go test ./pkg/cli/net/... -run 'TestRunRegister|TestRunLogin|TestRunLogout|TestAPIDo' -count=1 -race
```

Expected: FAIL because new account commands are absent.

- [ ] **Step 3: Implement Device client**

```go
func APIDo(cfg *AgentConfig, creds *AgentCredentials, method, path string, body any) (*http.Response, error)
func APIDoWithSession(cfg *AgentConfig, creds *AgentCredentials, session, method, path string, body any) (*http.Response, error)

func TeamPath(cfg *AgentConfig, suffix string) (string, error) {
	if cfg.CurrentTeam == "" {
		return "", errors.New("no active team; run agentlink team use <team_id>")
	}
	return "/api/teams/" + url.PathEscape(cfg.CurrentTeam) + suffix, nil
}
```

Set:

```go
req.Header.Set("Authorization", "Device "+creds.DeviceSession)
if session != "" {
	req.Header.Set("X-Agentlink-Session", session)
}
```

- [ ] **Step 4: Implement account commands**

Signatures:

```go
type AccountIO struct {
	In       io.Reader
	Out      io.Writer
	Err      io.Writer
	Password func() ([]byte, error)
}

func RunRegister(server, username, device string, io AccountIO) error
func RunLogin(server, username, device string, io AccountIO) error
func RunLogout(io AccountIO) error
```

Register:

1. Prompt password + confirmation.
2. `POST /api/auth/register`.
3. `POST /api/auth/device-login` using the same in-memory password.
4. Write config/credentials only after both succeed.

Login prompts once and calls device-login. If the response has `must_change_password=true`, prompt for a new password and confirmation, call `/api/auth/change-password` with the restricted Device Session, then repeat device-login with the new password before saving credentials. Never echo or print a password/credential.

- [ ] **Step 5: Write failing team command tests**

```go
func TestRunTeamListParsesMemberships(t *testing.T)
func TestRunTeamCreatePrintsInviteOnce(t *testing.T)
func TestRunTeamJoinDoesNotPersistInvite(t *testing.T)
func TestRunTeamUsePersistsSelectedMembership(t *testing.T)
func TestRunTeamMembersUsesCurrentTeam(t *testing.T)
func TestRunTeamLeaveClearsCurrentTeam(t *testing.T)
```

- [ ] **Step 6: Implement team commands and dispatch**

Commands:

```text
agentlink register --server <url> --username <name> [--device <name>]
agentlink login --server <url> --username <name> [--device <name>]
agentlink logout
agentlink team list
agentlink team create <name>
agentlink team join <team_id> <invite_code>
agentlink team use <team_id>
agentlink team members
agentlink team leave
```

Validate `team use` against `GET /api/teams` before persisting. `team leave` calls the current team's `/leave` endpoint and clears `current_team` only after a successful response.

- [ ] **Step 7: Verify and commit**

```bash
gofmt -w pkg/cli/net cmd/agentlink
go test ./pkg/cli/net/... -count=1 -race
go build ./cmd/agentlink/
git add pkg/cli/net/accounts.go pkg/cli/net/accounts_test.go pkg/cli/net/teams.go pkg/cli/net/teams_v2_test.go pkg/cli/net/client.go cmd/agentlink/main.go
git commit -m "feat(cli): add account login and team commands"
```

Expected: no command requires users to copy a credential.

---

### Task 3: Make `agentlink init` local-only

**Files:**
- Modify: `pkg/cli/runtime/init.go`
- Modify: `pkg/cli/runtime/init_wizard.go`
- Modify: `pkg/cli/runtime/init_test.go`
- Modify: `cmd/agentlink/main.go`

**Interfaces:**
- Consumes: logged-in v2 config and credentials.
- Produces: local session directories, CLAUDE.md, tmux, and pollers without server registration.

- [ ] **Step 1: Rewrite failing init expectations**

Replace registration-server tests with:

```go
func TestRunInitRequiresExistingLogin(t *testing.T)
func TestRunInitMakesNoRegisterRequest(t *testing.T)
func TestRunInitPreservesAccountAndCurrentTeam(t *testing.T)
func TestRunInitWritesSessionMarkersAndCLAUDEMD(t *testing.T)
```

The no-register test uses an HTTP handler that fails the test on `/agents/register` or `/api/auth/*`.

- [ ] **Step 2: Run tests and verify RED**

```bash
go test ./pkg/cli/runtime/... -run TestRunInit -count=1 -race
```

Expected: old init attempts `/agents/register`.

- [ ] **Step 3: Remove registration from init**

Change options:

```go
type InitOptions struct {
	Path   string
	Agent  string
	NoPoll bool
	Force  bool
}
```

At start:

```go
cfg, creds, err := api.LoadAuth()
if err != nil {
	return fmt.Errorf("not logged in; run agentlink login: %w", err)
}
if cfg.CurrentTeam == "" || creds.DeviceSession == "" {
	return errors.New("select a team with agentlink team use before init")
}
```

Remove `registerDeviceInteractive`, `RegisterRequest`, register password prompts, and credential writing from init. Preserve account/current-team fields when writing runtime fields.

- [ ] **Step 4: Update CLI usage**

```text
agentlink init [--agent claude] [--no-poll] [--force] [./path]
```

No `--server`, `--password`, or `--device`.

- [ ] **Step 5: Verify and commit**

```bash
gofmt -w pkg/cli/runtime cmd/agentlink
go test ./pkg/cli/runtime/... -run TestRunInit -count=1 -race
git add pkg/cli/runtime/init.go pkg/cli/runtime/init_wizard.go pkg/cli/runtime/init_test.go cmd/agentlink/main.go
git commit -m "refactor(cli): make init use the logged-in device"
```

---

### Task 4: Migrate ordinary CLI API clients to team routes

**Files:**
- Modify: `pkg/cli/net/projects.go`
- Modify: `pkg/cli/net/projects_test.go`
- Modify: `pkg/cli/net/locks.go`
- Modify: `pkg/cli/net/locks_test.go`
- Modify: `pkg/cli/net/agents.go`
- Modify: `pkg/cli/net/agents_test.go`
- Modify: `pkg/cli/net/messages.go`
- Modify: `pkg/cli/net/messages_test.go`
- Modify: `pkg/cli/net/tasks.go`
- Modify: `pkg/cli/net/tasks_test.go`
- Modify: `pkg/cli/net/whoami.go`

**Interfaces:**
- Consumes: `TeamPath`, Device Session, `FindCurrentSession`.
- Produces: all normal CLI requests against v2 team APIs.

- [ ] **Step 1: Change tests to exact v2 paths and bodies**

Required examples:

```go
if r.URL.Path != "/api/teams/tm_alpha/projects" { ... }
if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Device ") { ... }
if got := r.Header.Get("X-Agentlink-Session"); got != "main" { ... }
```

Lock request JSON must be:

```json
{"project_id":"prj123","path":"index.html"}
```

It must not contain `session`, `owner`, or `device`.

- [ ] **Step 2: Run net tests and verify RED**

```bash
go test ./pkg/cli/net/... -count=1 -race
```

Expected: failures show old paths, Bearer header, and old lock body.

- [ ] **Step 3: Migrate projects, locks, and agents**

Use `TeamPath` for every URL. Call `APIDoWithSession` only for lock operations. Decode v2 structured lock owner and print `owner.label`.

- [ ] **Step 4: Migrate messages, inbox, tasks, and whoami**

Targets remain device/session labels, but the server resolves them inside the current team. Never send `from_device` or `from_session`; those come from Actor and `X-Agentlink-Session`.

- [ ] **Step 5: Verify and commit**

```bash
gofmt -w pkg/cli/net
go test ./pkg/cli/net/... -count=1 -race
git add pkg/cli/net
git commit -m "refactor(cli): route commands through the active team"
```

Expected: all net tests assert Device auth and team URLs.

---

### Task 5: Migrate poller, resume, session management, and sync

**Files:**
- Modify: `pkg/cli/runtime/poller.go`
- Modify: `pkg/cli/runtime/poller_test.go`
- Modify: `pkg/cli/runtime/resume.go`
- Modify: `pkg/cli/runtime/resume_test.go`
- Modify: `pkg/cli/runtime/session.go`
- Modify: `pkg/cli/runtime/session_test.go`
- Modify: `pkg/cli/runtime/sync.go`
- Modify: `pkg/cli/runtime/sync_test.go`

**Interfaces:**
- Consumes: current team and Device Session.
- Produces: v2 team polling/sync and account-owned local session management.

- [ ] **Step 1: Update runtime tests to v2**

Assert:

```text
Authorization: Device <credential>
X-Agentlink-Session: <local session>
/api/teams/<team>/inbox
/api/teams/<team>/agents/heartbeat
/api/teams/<team>/projects/<project>/snapshot
/api/teams/<team>/projects/<project>/apply
/api/teams/<team>/locks/acquire
/api/teams/<team>/ws?project=<project>
```

Add:

```go
func TestSyncWSUsesDeviceHeaderNotQueryCredential(t *testing.T)
func TestResumeRejectsRemovedCurrentTeam(t *testing.T)
func TestSessionAddPreservesAccountConfig(t *testing.T)
```

- [ ] **Step 2: Run runtime tests and verify RED**

```bash
go test ./pkg/cli/runtime/... -count=1 -race
```

Expected: old Bearer paths fail; ignore only the previously documented environment-dependent prereq test until its dedicated fix.

- [ ] **Step 3: Migrate Poller and resume**

Add `TeamID`, `DeviceSession`, and `DeviceID` fields. Every request uses Device authorization and the poller's session header. Resume verifies both device session validity and current team membership before launching tmux.

- [ ] **Step 4: Migrate sync REST**

`Syncer` receives `TeamID` and uses:

```go
func (s *Syncer) projectPath(suffix string) string {
	return "/api/teams/" + url.PathEscape(s.TeamID) +
		"/projects/" + url.PathEscape(s.Project) + suffix
}
```

Lock body uses `{project_id,path}`. Apply body uses `{path,content}`. Both requests set Device auth and `X-Agentlink-Session`.

- [ ] **Step 5: Migrate sync WebSocket**

Use:

```go
headers := http.Header{}
headers.Set("Authorization", "Device "+s.DeviceSession)
headers.Set("X-Agentlink-Session", s.Session)
conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
	HTTPHeader: headers,
})
```

The URL contains team/project but no credential.

- [ ] **Step 6: Verify and commit**

```bash
gofmt -w pkg/cli/runtime
go test ./pkg/cli/runtime/... -count=1 -race
git add pkg/cli/runtime
git commit -m "refactor(cli): migrate runtime and sync to team sessions"
```

---

### Task 6: Delete legacy authentication and v1 routes

**Files:**
- Modify: `pkg/api/server.go`
- Modify: `pkg/config/config.go`
- Modify: `cmd/server/main.go`
- Create: `pkg/api/http_helpers.go`
- Create: `pkg/api/validation.go`
- Delete: `pkg/api/apply.go`
- Delete: `pkg/api/apply_test.go`
- Delete: `pkg/api/locks.go`
- Delete: `pkg/api/locks_test.go`
- Delete: `pkg/api/projects.go`
- Delete: `pkg/api/projects_test.go`
- Delete: `pkg/api/ws.go`
- Delete: `pkg/api/ws_test.go`
- Delete: `pkg/api/preview.go`
- Delete: `pkg/api/preview_test.go`
- Refactor/delete v1 portions of: `pkg/api/handlers.go`, `pkg/api/handlers_test.go`, `pkg/api/agents_test.go`, `pkg/api/tasks_test.go`

**Interfaces:**
- Consumes: complete v2 API.
- Produces: one authentication path and one set of business routes.

- [ ] **Step 1: Add cutover regression tests**

```go
func TestLegacyRegisterRouteIsGone(t *testing.T)
func TestLegacyBearerAPIKeyIsRejected(t *testing.T)
func TestLegacyBusinessRoutesAreGone(t *testing.T)
func TestServerConfigHasNoRegisterPassword(t *testing.T)
func TestPreviewTokenNoLongerExists(t *testing.T)
```

Expected statuses:

- `/agents/register`: 404 or 405, never 200.
- `Bearer sk_live_x`: 401 on v2 routes.
- `/projects`, `/locks/acquire`, `/ws`: 404 or 405.

- [ ] **Step 2: Run tests and verify RED**

```bash
go test ./pkg/api/... ./pkg/config/... -run 'TestLegacy|TestServerConfig|TestPreviewToken' -count=1 -race
```

Expected: legacy routes/config still exist.

- [ ] **Step 3: Extract shared utilities**

Before deleting v1 files, move only still-used helpers:

- `writeJSON`, `writeError` → `http_helpers.go`.
- username/device/session regex → `validation.go`.
- `injectBeforeBodyClose` (currently in `preview.go`, used by `preview_v2.go`) → a surviving v2 file (e.g. `preview_v2.go` or `http_helpers.go`).
- hardened `safeApplyPath`, Git helpers, event interfaces must already exist in v2 files from Plan 2.

Run v2 tests after extraction before deleting anything.

- [ ] **Step 4: Remove legacy routes and fields**

Delete:

- `contextKeyDevice`.
- `registerPassword`.
- `previewToken` and `generatePreviewToken`.
- old route registration (including v1 `GET /preview/{id}/{path...}`).
- SHA-256 API-key lookup and `agentlink:api_key:*`.
- public unauthenticated preview exemption.

Once v1 `/preview/{id}` is gone, rename the v2 preview route from the
transitional `GET /preview/teams/{team_id}/{project_id}/{path...}` to the
canonical `GET /preview/{team_id}/{project_id}/{path...}` and update the Web GUI
`previewFrame.src` (drop the `teams/` segment).

> **Deferred hardening (decided 2026-07-16): preview origin isolation.** The v2
> preview is served same-origin as the app and renders member-authored HTML, so
> a member's stored `<script>` runs on the app origin and can read the readable
> `al_csrf` and act as a viewing member (in-team privilege escalation). Accepted
> for now (≈10 trusted members, attributable authorship); mitigated only with
> `X-Content-Type-Options: nosniff`. Real fix (schedule here or in Plan 3): serve
> preview from a separate origin/subdomain so the preview page cannot read app
> cookies or make same-origin authenticated requests; give preview its own
> read-only, revocable, short-lived auth (separate-scope cookie or preview token),
> optionally with `<iframe sandbox>` + CSP. This changes cookie scoping and the
> preview auth path, so it is its own task, not a drive-by edit during cutover.

`config.Config` becomes:

```go
type Config struct {
	RedisAddr   string
	PublicURL   string
	CookieSecure bool
}
```

Reject startup when `CookieSecure=false` and `PublicURL` is not localhost.

- [ ] **Step 5: Delete obsolete files/tests**

Delete v1 cowork files after confirming every reused helper has a v2 home. Replace old message/task/agent tests with their v2 equivalents; do not keep dead dual-auth test fixtures.

- [ ] **Step 6: Run full suite and commit**

```bash
gofmt -w pkg/api pkg/config cmd/server
go test ./... -count=1 -race
go build ./...
go vet ./...
git add -u pkg/api pkg/config cmd/server
git add pkg/api/http_helpers.go pkg/api/validation.go
git commit -m "refactor(auth): remove legacy API key authentication"
```

Expected: one authentication system remains.

---

### Task 7: Fix environment-sensitive prerequisite test

**Files:**
- Modify: `pkg/cli/runtime/init_test.go`

**Interfaces:**
- Produces: deterministic full repository test baseline.

- [ ] **Step 1: Reproduce the existing failure**

```bash
go test ./pkg/cli/runtime/... -run 'TestCheckPrereqs/partial_PATH_with_tmux_only' -count=1 -v
```

Expected on the current machine: FAIL because `claude` and `tmux` share a directory and the test accidentally exposes both.

- [ ] **Step 2: Make the fixture isolate binaries**

Create a temporary bin directory containing only a symlink/copy named `tmux`, set `PATH` to that directory, and never derive PATH from the real tmux parent:

```go
bin := t.TempDir()
realTmux, err := exec.LookPath("tmux")
if err != nil { t.Skip("tmux unavailable") }
if err := os.Symlink(realTmux, filepath.Join(bin, "tmux")); err != nil {
	t.Fatal(err)
}
t.Setenv("PATH", bin)
```

- [ ] **Step 3: Verify and commit**

```bash
go test ./pkg/cli/runtime/... -run TestCheckPrereqs -count=1 -race
git add pkg/cli/runtime/init_test.go
git commit -m "test(cli): isolate prerequisite PATH fixtures"
```

Expected: the formerly environment-sensitive test is deterministic.

---

### Task 8: Final E2E, docs, and clean-break operations

**Files:**
- Modify: `pkg/api/e2e_v2_test.go`
- Create: `pkg/cli/runtime/auth_e2e_test.go`
- Modify: `README.md`
- Modify: `README_zh.md`
- Modify: `docs/deploy-server.md`
- Modify: `docs/cowork-design.md`
- Modify: `docs/cowork-impl.md`
- Create: `docs/team-auth-operations.md`

**Interfaces:**
- Verifies the released account/team flow.

- [ ] **Step 1: Add CLI-to-server E2E**

Test:

```go
func TestE2EV2CLIRegisterLoginTeamInitSync(t *testing.T)
```

Flow:

1. Register account.
2. Device login.
3. Create and select team.
4. Run local-only init with tmux operations stubbed.
5. Create project.
6. Start Syncer with Device Session.
7. Lock/apply and verify another user receives the file.
8. Logout and assert subsequent Device request returns 401.

- [ ] **Step 2: Run full E2E**

```bash
go test ./pkg/api/... ./pkg/cli/net/... ./pkg/cli/runtime/... -run TestE2EV2 -count=1 -race -v
```

Expected: all account/team/CLI/browser isolation flows pass.

- [ ] **Step 3: Rewrite user docs**

README quick start must be:

```bash
agentlink register --server https://cowork.example --username kirby
agentlink team create "Product"
agentlink team use tm_a7k3p9d2
agentlink init ./agent_team
agentlink project create "Prototype"
agentlink sync <project_id> ./prototype
```

Remove every instruction mentioning `REGISTER_PASSWORD`, `sk_live_`, API token paste, or old routes.

Add a prominent supersession notice at the top of `docs/cowork-design.md` and `docs/cowork-impl.md` linking to `docs/superpowers/specs/2026-07-15-team-auth-design.md`; those historical documents retain the original v1 implementation record but are not valid authentication instructions.

- [ ] **Step 4: Write operations guide**

Document:

- Required `PUBLIC_URL`, `COOKIE_SECURE`, `REDIS_ADDR`, `DATA_DIR`.
- HTTPS requirement.
- `agentlink-admin user reset-password`.
- Redis backup.
- v1 namespace rollback.
- Explicit v1 cleanup command after acceptance.
- Session revocation and incident response.

- [ ] **Step 5: Verify no legacy references**

```bash
rg 'REGISTER_PASSWORD|sk_live_|api_key|/agents/register|cowork_token' \
  --glob '!docs/superpowers/**' \
  --glob '!docs/cowork-design.md' \
  --glob '!docs/cowork-impl.md' \
  --glob '!issues/**' .
```

Expected: no user-facing or active-code legacy references. Excluded historical issue/design documents must carry the supersession notice.

- [ ] **Step 6: Full verification and commit**

```bash
go test ./... -count=1 -race
go build ./...
go vet ./...
git add pkg/api/e2e_v2_test.go pkg/cli/runtime/auth_e2e_test.go README.md README_zh.md docs/deploy-server.md docs/cowork-design.md docs/cowork-impl.md docs/team-auth-operations.md
git commit -m "docs(auth): complete account and team cutover"
```

Expected: full repository green with no accepted baseline failure.

---

## Plan 4 Completion Gate

Run:

```bash
go test ./... -count=1 -race
go build ./...
go vet ./...
rg 'REGISTER_PASSWORD|sk_live_|cowork_token|agentlink:api_key:' \
  --glob '!docs/superpowers/**' \
  --glob '!docs/cowork-design.md' \
  --glob '!docs/cowork-impl.md' \
  --glob '!issues/**' .
```

Expected:

- Full test/build/vet suite passes.
- CLI users register/login/select team without seeing a token.
- GUI uses Cookie + CSRF only.
- CLI REST/WS uses an automatically stored Device Session.
- No old auth route, API-key keyspace, preview token, or registration password remains.
- All business data is team-scoped.
