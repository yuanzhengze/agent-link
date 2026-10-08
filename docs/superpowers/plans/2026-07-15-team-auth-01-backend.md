# Account and Team Authentication Backend Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add password accounts, revocable browser/device sessions, multi-team membership, invitations, and Owner/Admin/Member authorization without yet migrating the existing cowork business routes.

**Architecture:** A focused `pkg/auth` package owns password hashing, Redis v2 keys, sessions, teams, and role checks. `pkg/api` exposes `/api/auth/*` and `/api/teams/*`, converts valid credentials into a structured `Actor`, and applies CSRF only to Cookie-authenticated writes. Existing v1 routes remain untouched only while this backend foundation is built; Plan 4 removes them before release.

**Tech Stack:** Go 1.24, `golang.org/x/crypto/argon2`, Go `net/http`, Redis 9, opaque 256-bit sessions, SHA-256 indexes, HttpOnly cookies.

## Global Constraints

- Source of truth: `docs/superpowers/specs/2026-07-15-team-auth-design.md`.
- All new Redis keys use `agentlink:v2:*`; never read or delete v1 keys.
- Usernames are 3–32 ASCII letters/digits/`-`/`_`, indexed case-insensitively.
- Passwords are at least 10 characters and are never logged or persisted in plaintext.
- Web sessions: 12-hour idle timeout, 7-day absolute timeout.
- Device sessions: 90-day timeout.
- Production Cookie auth requires HTTPS; only localhost may use insecure cookies.
- Every implementation task follows RED → GREEN → full relevant suite → commit.
- Do not stage the pre-existing unrelated deletions, `agent-link/`, README edits, or `pkg/api/e2e_test.go`.

---

## File Structure

### New package `pkg/auth`

- `model.go`: `User`, `Team`, `Actor`, `Role`, session models, validation errors.
- `password.go`: username normalization and Argon2id PHC hash/verify.
- `random.go`: cryptographically random user/team/session/invite IDs.
- `keys.go`: all `agentlink:v2:*` key builders.
- `store.go`: user, membership, and team Redis persistence.
- `sessions.go`: Web/Device session creation, lookup, rotation, revocation.
- `service.go`: registration, login, team operations, role invariants, rate limiting.
- Matching `*_test.go` files own unit/Redis integration tests.

### API

- `pkg/api/auth_v2.go`: register/login/logout/me/change-password/device-login/device-logout handlers.
- `pkg/api/auth_v2_middleware.go`: Cookie/Device credential parsing, CSRF, Actor context.
- `pkg/api/teams_v2.go`: team create/join/list/member administration handlers.
- `pkg/api/server.go`: v2 service construction and route registration.
- `pkg/config/config.go`: Cookie security and public base URL configuration.
- `cmd/server/main.go`: pass server auth options.

### Administration

- `cmd/agentlink-admin/main.go`: server-side password reset command.

---

### Task 1: Passwords, usernames, IDs, and shared models

**Files:**
- Create: `pkg/auth/model.go`
- Create: `pkg/auth/password.go`
- Create: `pkg/auth/password_test.go`
- Create: `pkg/auth/random.go`
- Create: `pkg/auth/random_test.go`
- Modify: `go.mod`
- Modify: `go.sum`

**Interfaces:**
- Produces:
  - `func NormalizeUsername(string) (string, error)`
  - `func HashPassword(string) (string, error)`
  - `func VerifyPassword(encoded, password string) (bool, error)`
  - `func NewUserID() (string, error)`
  - `func NewTeamID() (string, error)`
  - `func NewSecret(prefix string, bytes int) (string, error)`
  - `type Actor`, `type User`, `type Team`, `type Role`

- [ ] **Step 1: Add the password dependency**

Run:

```bash
go get golang.org/x/crypto@latest
```

Expected: `go.mod` contains a direct `golang.org/x/crypto` requirement and `go mod tidy` succeeds.

- [ ] **Step 2: Write failing password and username tests**

Create tests with these exact cases:

```go
func TestNormalizeUsername(t *testing.T) {
	tests := map[string]string{
		"Kirby": "kirby",
		"pm_01": "pm_01",
		"A-b":   "a-b",
	}
	for in, want := range tests {
		got, err := NormalizeUsername(in)
		if err != nil || got != want {
			t.Fatalf("NormalizeUsername(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, invalid := range []string{"ab", "has space", "中文名", "a/b", strings.Repeat("a", 33)} {
		if _, err := NormalizeUsername(invalid); err == nil {
			t.Errorf("NormalizeUsername(%q) unexpectedly succeeded", invalid)
		}
	}
}

func TestHashAndVerifyPassword(t *testing.T) {
	encoded, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := VerifyPassword(encoded, "correct horse battery staple"); err != nil || !ok {
		t.Fatalf("verify correct password = %v, %v", ok, err)
	}
	if ok, err := VerifyPassword(encoded, "wrong password"); err != nil || ok {
		t.Fatalf("verify wrong password = %v, %v", ok, err)
	}
}
```

- [ ] **Step 3: Run tests and verify RED**

Run:

```bash
go test ./pkg/auth/... -run 'TestNormalizeUsername|TestHashAndVerifyPassword' -count=1
```

Expected: FAIL because `NormalizeUsername`, `HashPassword`, and `VerifyPassword` do not exist.

- [ ] **Step 4: Implement models and password hashing**

Define:

```go
type Role string

const (
	RoleOwner  Role = "owner"
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
)

type Actor struct {
	UserID      string
	Username    string
	TeamID      string
	Role        Role
	DeviceID    string
	DeviceName  string
	SessionName string
	ClientType  string
}

type User struct {
	ID                 string
	Username           string
	UsernameNormalized string
	PasswordPHC        string
	PasswordVersion    int64
	Status             string
	MustChangePassword bool
	CreatedAt          time.Time
}

type Team struct {
	ID          string
	Name        string
	OwnerUserID string
	CreatedAt   time.Time
}
```

Use these Argon2id parameters in `password.go`:

```go
const (
	argonMemory      = 64 * 1024
	argonIterations  = 3
	argonParallelism = 2
	argonSaltLength  = 16
	argonKeyLength   = 32
)
```

Encode hashes as:

```text
$argon2id$v=19$m=65536,t=3,p=2$<base64-salt>$<base64-hash>
```

`VerifyPassword` must parse all parameters from the PHC string, use `subtle.ConstantTimeCompare`, and return an error for malformed encodings.

- [ ] **Step 5: Add deterministic shape tests for generated IDs**

```go
func TestGeneratedIdentifiers(t *testing.T) {
	userID, _ := NewUserID()
	teamID, _ := NewTeamID()
	secret, _ := NewSecret("ds_", 32)
	if !strings.HasPrefix(userID, "usr_") || !strings.HasPrefix(teamID, "tm_") {
		t.Fatalf("unexpected IDs: %q %q", userID, teamID)
	}
	if !strings.HasPrefix(secret, "ds_") || len(secret) < 40 {
		t.Fatalf("unexpected secret shape: %q", secret)
	}
}
```

Implement IDs with `crypto/rand`; use lowercase base32 without ambiguous `0/O/1/I` characters for team IDs and hex/base64url for opaque secrets.

- [ ] **Step 6: Run package tests and commit**

Run:

```bash
gofmt -w pkg/auth
go test ./pkg/auth/... -count=1 -race
go vet ./pkg/auth/...
git add go.mod go.sum pkg/auth/model.go pkg/auth/password.go pkg/auth/password_test.go pkg/auth/random.go pkg/auth/random_test.go
git commit -m "feat(auth): add account identity primitives"
```

Expected: all commands pass.

---

### Task 2: Redis v2 user, team, and session store

**Files:**
- Create: `pkg/auth/keys.go`
- Create: `pkg/auth/store.go`
- Create: `pkg/auth/store_test.go`
- Create: `pkg/auth/sessions.go`
- Create: `pkg/auth/sessions_test.go`

**Interfaces:**
- Consumes: Task 1 models and ID/password helpers.
- Produces:
  - `func NewStore(*redis.Client) *Store`
  - `func (s *Store) CreateUser(context.Context, User) error`
  - `func (s *Store) UserByUsername(context.Context, string) (User, error)`
  - `func (s *Store) CreateTeam(context.Context, Team, inviteHash string) error`
  - `func (s *Store) Team(context.Context, string) (Team, error)`
  - `func (s *Store) Role(context.Context, teamID, userID string) (Role, error)`
  - Web/Device session create, resolve, and revoke methods listed below.

- [ ] **Step 1: Write failing store tests**

Use a real test Redis and clean only `agentlink:v2:test:*` plus the test-specific IDs. Cover:

```go
func TestStoreCreateUserEnforcesNormalizedUniqueness(t *testing.T)
func TestStoreCreateTeamAddsOwnerMembershipAtomically(t *testing.T)
func TestStoreRoleReturnsErrNotMember(t *testing.T)
func TestWebSessionResolveAndRevoke(t *testing.T)
func TestDeviceSessionResolveAndRevoke(t *testing.T)
func TestPasswordVersionInvalidatesSessions(t *testing.T)
```

For username uniqueness, start two goroutines attempting `Kirby` and `kirby`; assert exactly one succeeds with `ErrUsernameExists`.

- [ ] **Step 2: Run tests and verify RED**

Run:

```bash
go test ./pkg/auth/... -run 'TestStore|TestWebSession|TestDeviceSession|TestPasswordVersion' -count=1 -race
```

Expected: FAIL because `Store` and session methods do not exist.

- [ ] **Step 3: Implement key builders and domain errors**

Use exact helpers:

```go
func userKey(id string) string             { return "agentlink:v2:user:" + id }
func usernameKey(name string) string       { return "agentlink:v2:username:" + name }
func userTeamsKey(id string) string        { return userKey(id) + ":teams" }
func teamKey(id string) string             { return "agentlink:v2:team:" + id }
func teamMembersKey(id string) string      { return teamKey(id) + ":members" }
func teamProjectsKey(id string) string     { return teamKey(id) + ":projects" }
func webSessionKey(hash string) string     { return "agentlink:v2:web_session:" + hash }
func deviceSessionKey(hash string) string  { return "agentlink:v2:device_session:" + hash }
func userWebSessionsKey(id string) string  { return userKey(id) + ":web_sessions" }
func userDeviceSessionsKey(id string) string {
	return userKey(id) + ":device_sessions"
}
```

Define sentinel errors:

```go
var (
	ErrNotFound       = errors.New("not found")
	ErrUsernameExists = errors.New("username already exists")
	ErrNotMember      = errors.New("not a team member")
	ErrForbidden      = errors.New("forbidden")
	ErrSessionExpired = errors.New("session expired")
)
```

- [ ] **Step 4: Implement atomic user and team creation**

Use Lua scripts so indexes and hashes cannot diverge:

```lua
-- CreateUser: KEYS[1]=username index, KEYS[2]=user hash
if redis.call('EXISTS', KEYS[1]) == 1 then return 0 end
redis.call('SET', KEYS[1], ARGV[1])
redis.call('HSET', KEYS[2],
  'id', ARGV[1], 'username', ARGV[2], 'username_normalized', ARGV[3],
  'password_phc', ARGV[4], 'password_version', ARGV[5],
  'status', 'active', 'must_change_password', '0', 'created_at', ARGV[6])
return 1
```

```lua
-- CreateTeam: team hash + owner member + reverse user index
if redis.call('EXISTS', KEYS[1]) == 1 then return 0 end
redis.call('HSET', KEYS[1],
  'id', ARGV[1], 'name', ARGV[2], 'owner_user_id', ARGV[3],
  'invite_hash', ARGV[4], 'invite_version', '1', 'created_at', ARGV[5])
redis.call('HSET', KEYS[2], ARGV[3], 'owner')
redis.call('SADD', KEYS[3], ARGV[1])
return 1
```

- [ ] **Step 5: Implement session persistence**

Expose:

```go
type WebSession struct {
	UserID            string
	CSRFHash          string
	PasswordVersion   int64
	CreatedAt         time.Time
	LastSeenAt        time.Time
	AbsoluteExpiresAt time.Time
}

type DeviceSession struct {
	UserID          string
	DeviceID        string
	PasswordVersion int64
	CreatedAt       time.Time
	LastSeenAt      time.Time
}

func (s *Store) CreateWebSession(ctx context.Context, sessionHash string, ws WebSession) error
func (s *Store) ResolveWebSession(ctx context.Context, sessionHash string, now time.Time) (WebSession, error)
func (s *Store) RevokeWebSession(ctx context.Context, sessionHash string) error
func (s *Store) CreateDeviceSession(ctx context.Context, hash string, ds DeviceSession) error
func (s *Store) ResolveDeviceSession(ctx context.Context, hash string, now time.Time) (DeviceSession, error)
func (s *Store) RevokeDeviceSession(ctx context.Context, hash string) error
func (s *Store) RevokeAllUserSessions(ctx context.Context, userID string) error
```

Use Redis TTL for idle expiry and store absolute expiry in the Web Session hash. Resolution must compare the session password version with the current user password version before returning.

- [ ] **Step 6: Run store tests and commit**

```bash
gofmt -w pkg/auth
go test ./pkg/auth/... -count=1 -race
go vet ./pkg/auth/...
git add pkg/auth/keys.go pkg/auth/store.go pkg/auth/store_test.go pkg/auth/sessions.go pkg/auth/sessions_test.go
git commit -m "feat(auth): persist users teams and revocable sessions"
```

Expected: all tests pass and no v1 Redis key is touched.

---

### Task 3: Auth service, rate limiting, and session lifecycle

**Files:**
- Create: `pkg/auth/service.go`
- Create: `pkg/auth/service_test.go`

**Interfaces:**
- Consumes: Task 2 `Store`.
- Produces:
  - `func NewService(*Store, Clock) *Service`
  - account registration/login/change/reset methods
  - browser/device session issue/resolve/revoke methods
  - login rate limiting.

- [ ] **Step 1: Write failing service tests**

Cover:

```go
func TestServiceRegisterHashesPassword(t *testing.T)
func TestServiceLoginReturnsGenericInvalidCredentials(t *testing.T)
func TestServiceLoginRateLimitFiveFailures(t *testing.T)
func TestServiceChangePasswordRevokesAllSessions(t *testing.T)
func TestServiceDeviceLoginCreatesOwnedDevice(t *testing.T)
func TestServiceResetPasswordRequiresChangeAndRevokesSessions(t *testing.T)
```

Inject a fake clock:

```go
type Clock interface{ Now() time.Time }
type FixedClock struct{ T time.Time }
func (c FixedClock) Now() time.Time { return c.T }
```

- [ ] **Step 2: Run tests and verify RED**

```bash
go test ./pkg/auth/... -run TestService -count=1 -race
```

Expected: FAIL because `Service` does not exist.

- [ ] **Step 3: Implement service contracts**

Use these request/result types:

```go
type RegisterInput struct {
	Username string
	Password string
}

type LoginInput struct {
	Username string
	Password string
	IP       string
}

type WebLoginResult struct {
	User          User
	SessionSecret string
	CSRFSecret    string
}

type DeviceLoginInput struct {
	Username   string
	Password   string
	IP         string
	DeviceName string
}

type DeviceLoginResult struct {
	User             User
	DeviceID         string
	DeviceCredential string
}
```

Rate-limit keys:

```text
agentlink:v2:login_fail:user:<sha256(normalized)>
agentlink:v2:login_fail:ip:<sha256(ip)>
```

Increment both with a Lua script, set 15-minute TTL on first failure, and reject when either counter reaches 5. Successful login deletes both counters.

- [ ] **Step 4: Run service tests and commit**

```bash
gofmt -w pkg/auth
go test ./pkg/auth/... -count=1 -race
go vet ./pkg/auth/...
git add pkg/auth/service.go pkg/auth/service_test.go
git commit -m "feat(auth): add account and session lifecycle service"
```

Expected: all service tests pass.

---

### Task 4: Browser/device auth API and Actor middleware

**Files:**
- Create: `pkg/api/auth_v2.go`
- Create: `pkg/api/auth_v2_test.go`
- Create: `pkg/api/auth_v2_middleware.go`
- Create: `pkg/api/auth_v2_middleware_test.go`
- Modify: `pkg/api/server.go`
- Modify: `pkg/config/config.go`
- Modify: `cmd/server/main.go`
- Modify: `pkg/api/handlers_test.go`

**Interfaces:**
- Consumes: `auth.Service`.
- Produces:
  - `/api/auth/*` routes.
  - `ActorFromContext(context.Context) (auth.Actor, bool)`.
  - `requireIdentity(http.Handler) http.Handler`.
  - Cookie and Device credential authentication.

- [ ] **Step 1: Write failing API tests**

Add end-to-end handler tests for:

```go
func TestAuthRegisterSetsSessionAndCSRFCookies(t *testing.T)
func TestAuthLoginWrongPasswordIsGeneric401(t *testing.T)
func TestAuthBrowserLoginRejectsCrossOrigin(t *testing.T)
func TestAuthMeRotatesCSRF(t *testing.T)
func TestAuthCookieWriteRejectsMissingCSRF(t *testing.T)
func TestAuthDeviceLoginAndLogout(t *testing.T)
func TestAuthLogoutRevokesCookieSession(t *testing.T)
func TestAuthChangePasswordRevokesAllSessions(t *testing.T)
func TestAuthMustChangePasswordSessionIsRestricted(t *testing.T)
```

The registration test must assert:

```go
session := cookieByName(resp.Cookies(), "al_session")
csrf := cookieByName(resp.Cookies(), "al_csrf")
if session == nil || !session.HttpOnly || session.Value == "" {
	t.Fatal("missing HttpOnly al_session")
}
if csrf == nil || csrf.HttpOnly || csrf.Value == "" {
	t.Fatal("missing readable al_csrf")
}
```

- [ ] **Step 2: Run API tests and verify RED**

```bash
go test ./pkg/api/... -run 'TestAuth' -count=1 -race
```

Expected: FAIL because `/api/auth/*` routes are absent.

- [ ] **Step 3: Add server options and route registration**

Introduce:

```go
type ServerOptions struct {
	Addr         string
	DataDir      string
	Redis        *redis.Client
	CookieSecure bool
	PublicURL    string
}

func NewWithOptions(opts ServerOptions) *Server
```

Keep the existing `New(addr, dataDir, rdb, registerPassword)` only as a temporary test compatibility wrapper until Plan 4 removes v1 auth. Add `authService *auth.Service` to `Server`.

Register:

```go
s.mux.HandleFunc("POST /api/auth/register", s.handleAuthRegister)
s.mux.HandleFunc("POST /api/auth/login", s.handleAuthLogin)
s.mux.HandleFunc("POST /api/auth/logout", s.handleAuthLogout)
s.mux.HandleFunc("GET /api/auth/me", s.handleAuthMe)
s.mux.HandleFunc("POST /api/auth/change-password", s.handleAuthChangePassword)
s.mux.HandleFunc("POST /api/auth/device-login", s.handleDeviceLogin)
s.mux.HandleFunc("POST /api/auth/device-logout", s.handleDeviceLogout)
```

- [ ] **Step 4: Implement Cookie and CSRF behavior**

Use constants:

```go
const (
	sessionCookieName = "al_session"
	csrfCookieName    = "al_csrf"
)
```

For Cookie-authenticated mutating requests require:

```go
if r.Header.Get("Origin") != configuredOrigin {
	writeError(w, http.StatusForbidden, "invalid origin")
	return
}
csrfCookie, err := r.Cookie(csrfCookieName)
if err != nil ||
	r.Header.Get("X-CSRF-Token") == "" ||
	subtle.ConstantTimeCompare([]byte(csrfCookie.Value), []byte(r.Header.Get("X-CSRF-Token"))) != 1 ||
	sha256Hex(r.Header.Get("X-CSRF-Token")) != session.CSRFHash {
	writeError(w, http.StatusForbidden, "invalid csrf token")
	return
}
```

`GET /api/auth/me` rotates CSRF, updates the session hash, and resets `al_csrf`.

For public `register/login`, reject a non-empty `Origin` unless it exactly matches the configured public origin; allow an absent `Origin` for CLI registration. This blocks browser login-CSRF without requiring a pre-login CSRF session.

- [ ] **Step 5: Implement Device authorization**

Accept only:

```text
Authorization: Device ds_<opaque-secret>
```

Hash the secret, resolve the Device Session, load the user, and set Actor fields `UserID`, `Username`, `DeviceID`, `DeviceName`, `ClientType=device`. Reject old `Bearer sk_live_*` on v2 routes.

After loading the user, if `MustChangePassword` is true, allow only `GET /api/auth/me`, `POST /api/auth/change-password`, and `POST /api/auth/logout`; all other protected routes return 403 `password change required`. The login/device-login response includes the Boolean flag so GUI/CLI can immediately start the change-password flow.

- [ ] **Step 6: Run API tests and commit**

```bash
gofmt -w pkg/api pkg/config cmd/server
go test ./pkg/auth/... ./pkg/api/... -count=1 -race
go vet ./pkg/auth/... ./pkg/api/... ./cmd/server/...
git add pkg/api/auth_v2.go pkg/api/auth_v2_test.go pkg/api/auth_v2_middleware.go pkg/api/auth_v2_middleware_test.go pkg/api/server.go pkg/api/handlers_test.go pkg/config/config.go cmd/server/main.go
git commit -m "feat(api): add cookie and device session authentication"
```

Expected: auth tests pass; existing v1 tests remain green during the internal transition.

---

### Task 5: Team service, invitations, and role invariants

**Files:**
- Modify: `pkg/auth/store.go`
- Modify: `pkg/auth/store_test.go`
- Modify: `pkg/auth/service.go`
- Modify: `pkg/auth/service_test.go`

**Interfaces:**
- Produces:
  - `CreateTeam`, `JoinTeam`, `ListTeams`, `RotateInvite`.
  - `ListMembers`, `ChangeRole`, `RemoveMember`, `TransferOwner`.

- [ ] **Step 1: Write failing role and invitation tests**

Add:

```go
func TestCreateTeamMakesCreatorOwner(t *testing.T)
func TestJoinTeamRequiresCurrentInvite(t *testing.T)
func TestRotateInviteInvalidatesPreviousCode(t *testing.T)
func TestAdminCanRemoveMemberButNotAdminOrOwner(t *testing.T)
func TestOwnerCanPromoteAndDemoteAdmin(t *testing.T)
func TestOwnerMustTransferBeforeLeaving(t *testing.T)
func TestAdminAndMemberCanLeaveTeam(t *testing.T)
func TestTransferOwnerIsAtomic(t *testing.T)
```

- [ ] **Step 2: Run tests and verify RED**

```bash
go test ./pkg/auth/... -run 'TestCreateTeam|TestJoinTeam|TestRotateInvite|TestAdmin|TestOwner|TestTransferOwner' -count=1 -race
```

Expected: FAIL because team service methods are missing.

- [ ] **Step 3: Implement team service signatures**

```go
type CreateTeamResult struct {
	Team       Team
	InviteCode string
}

type Member struct {
	UserID   string
	Username string
	Role     Role
}

func (s *Service) CreateTeam(ctx context.Context, userID, name string) (CreateTeamResult, error)
func (s *Service) JoinTeam(ctx context.Context, userID, teamID, inviteCode string) (Team, error)
func (s *Service) ListTeams(ctx context.Context, userID string) ([]Team, error)
func (s *Service) RotateInvite(ctx context.Context, actor Actor) (string, error)
func (s *Service) ListMembers(ctx context.Context, actor Actor) ([]Member, error)
func (s *Service) ChangeRole(ctx context.Context, actor Actor, targetUserID string, role Role) error
func (s *Service) RemoveMember(ctx context.Context, actor Actor, targetUserID string) error
func (s *Service) TransferOwner(ctx context.Context, actor Actor, targetUserID string) error
func (s *Service) LeaveTeam(ctx context.Context, actor Actor) error
```

Hash invitation codes with SHA-256; compare with `subtle.ConstantTimeCompare`. Use Lua for role changes that update `owner_user_id` and member roles together.

`CreateTeam` must retry `NewTeamID()` when the Redis create script reports an ID collision, with a hard limit of 5 attempts; exhausting the limit returns an internal error and writes no partial membership/index data.

- [ ] **Step 4: Run tests and commit**

```bash
gofmt -w pkg/auth
go test ./pkg/auth/... -count=1 -race
git add pkg/auth/store.go pkg/auth/store_test.go pkg/auth/service.go pkg/auth/service_test.go
git commit -m "feat(auth): add team invitations and role management"
```

Expected: all role invariants pass.

---

### Task 6: Team HTTP API and team Actor middleware

**Files:**
- Create: `pkg/api/teams_v2.go`
- Create: `pkg/api/teams_v2_test.go`
- Modify: `pkg/api/auth_v2_middleware.go`
- Modify: `pkg/api/auth_v2_middleware_test.go`
- Modify: `pkg/api/server.go`

**Interfaces:**
- Consumes: Task 5 service methods.
- Produces:
  - all `/api/teams` management routes.
  - `requireTeamRole(min ...auth.Role)` middleware.
  - Actor enriched with `TeamID` and `Role`.

- [ ] **Step 1: Write failing team API tests**

Cover the complete HTTP matrix:

```go
func TestTeamCreateJoinList(t *testing.T)
func TestTeamInviteRotationRejectsOldCode(t *testing.T)
func TestTeamMemberRoleMatrix(t *testing.T)
func TestTeamTransferOwner(t *testing.T)
func TestTeamMemberCanLeaveAndOwnerCannot(t *testing.T)
func TestTeamMiddlewareRejectsNonMember(t *testing.T)
func TestTeamMiddlewareUsesURLTeamNotClientPreference(t *testing.T)
```

- [ ] **Step 2: Run tests and verify RED**

```bash
go test ./pkg/api/... -run 'TestTeam' -count=1 -race
```

Expected: FAIL because team routes are absent.

- [ ] **Step 3: Register exact routes**

```go
s.mux.HandleFunc("GET /api/teams", s.handleListTeams)
s.mux.HandleFunc("POST /api/teams", s.handleCreateTeam)
s.mux.HandleFunc("POST /api/teams/join", s.handleJoinTeam)
s.mux.HandleFunc("GET /api/teams/{team_id}", s.handleGetTeam)
s.mux.HandleFunc("GET /api/teams/{team_id}/members", s.handleListTeamMembers)
s.mux.HandleFunc("POST /api/teams/{team_id}/invite/rotate", s.handleRotateTeamInvite)
s.mux.HandleFunc("PATCH /api/teams/{team_id}/members/{user_id}", s.handleChangeTeamRole)
s.mux.HandleFunc("DELETE /api/teams/{team_id}/members/{user_id}", s.handleRemoveTeamMember)
s.mux.HandleFunc("POST /api/teams/{team_id}/transfer-owner", s.handleTransferTeamOwner)
s.mux.HandleFunc("POST /api/teams/{team_id}/leave", s.handleLeaveTeam)
```

Public auth routes bypass identity middleware; all team routes require identity, then resolve membership from `r.PathValue("team_id")`.

- [ ] **Step 4: Map domain errors consistently**

```go
switch {
case errors.Is(err, auth.ErrNotFound):
	writeError(w, http.StatusNotFound, "not found")
case errors.Is(err, auth.ErrNotMember), errors.Is(err, auth.ErrForbidden):
	writeError(w, http.StatusForbidden, "forbidden")
case errors.Is(err, auth.ErrUsernameExists), errors.Is(err, auth.ErrAlreadyMember):
	writeError(w, http.StatusConflict, err.Error())
default:
	writeError(w, http.StatusInternalServerError, "internal error")
}
```

Invitation failures return a generic 400 without revealing whether the team ID or code was wrong.

- [ ] **Step 5: Run all backend tests and commit**

```bash
gofmt -w pkg/api
go test ./pkg/auth/... ./pkg/api/... -count=1 -race
go vet ./pkg/auth/... ./pkg/api/...
git add pkg/api/teams_v2.go pkg/api/teams_v2_test.go pkg/api/auth_v2_middleware.go pkg/api/auth_v2_middleware_test.go pkg/api/server.go
git commit -m "feat(api): add multi-team membership endpoints"
```

Expected: all new auth/team tests pass.

---

### Task 7: Server administrator password reset

**Files:**
- Create: `cmd/agentlink-admin/main.go`
- Create: `cmd/agentlink-admin/main_test.go`
- Modify: `Makefile`

**Interfaces:**
- Consumes: `auth.Service`.
- Produces: `agentlink-admin user reset-password <username>`.

- [ ] **Step 1: Write failing reset command test**

Extract command logic into:

```go
func run(args []string, stdout, stderr io.Writer, rdb *redis.Client) int
```

The test creates a user, a Web Session, and a Device Session; invokes:

```go
code := run([]string{"user", "reset-password", "kirby"}, &out, &errOut, testRdb)
```

Assert exit code 0, output contains a one-time temporary password, `must_change_password=1`, password version incremented, and both sessions revoked.

- [ ] **Step 2: Run test and verify RED**

```bash
go test ./cmd/agentlink-admin/... -count=1 -race
```

Expected: FAIL because the command does not exist.

- [ ] **Step 3: Implement reset command**

The command must:

1. Load `REDIS_ADDR`.
2. Find normalized username.
3. Generate a 20-character high-entropy temporary password.
4. Hash it with `auth.HashPassword`.
5. Atomically increment password version and set `must_change_password=1`.
6. Revoke all user sessions.
7. Print the temporary password exactly once to stdout.

Add:

```make
build-admin:
	$(GO) build -o agentlink-admin ./cmd/agentlink-admin/
```

- [ ] **Step 4: Verify and commit**

```bash
gofmt -w cmd/agentlink-admin
go test ./cmd/agentlink-admin/... ./pkg/auth/... -count=1 -race
go build ./cmd/agentlink-admin/
git add cmd/agentlink-admin Makefile
git commit -m "feat(admin): add account password reset command"
```

Expected: tests and build pass.

---

## Plan 1 Completion Gate

Run:

```bash
go test ./pkg/auth/... ./pkg/api/... ./cmd/agentlink-admin/... -count=1 -race
go build ./...
go vet ./pkg/auth/... ./pkg/api/... ./cmd/agentlink-admin/...
```

Expected:

- Account registration/login works through Cookie and Device Session.
- CSRF and rate limiting are enforced.
- Multi-team create/join/member administration works.
- No v2 endpoint accepts legacy API keys.
- Existing business routes are not yet team-scoped; do not release until Plans 2–4 are complete.
