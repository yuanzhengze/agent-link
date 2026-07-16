package rt

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apisrv "github.com/team/agentlink/pkg/api"
	clinet "github.com/team/agentlink/pkg/cli/net"
	"github.com/team/agentlink/pkg/redis"
)

// e2ePassword scripts a fixed password for AccountIO so register/login never
// touch a real TTY. It is returned on every read (register asks twice).
func e2ePassword(pw string) func() ([]byte, error) {
	return func() ([]byte, error) { return []byte(pw), nil }
}

// e2ePrereqs makes init's CheckPrereqs (a LookPath existence check) pass by
// PREPENDING stub tmux/claude to PATH — unlike fakePrereqs it keeps the real
// PATH so the in-process server can still shell out to git for project
// create/apply. The stubs are never executed (launchSessionsFn is stubbed).
func e2ePrereqs(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	for _, name := range []string{"tmux", "claude"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// newE2EServer starts the full v2 API surface backed by a dedicated, flushed
// Redis DB so the CLI can drive it over real HTTP. It skips when Redis is
// unavailable, matching the api package's own integration tests.
func newE2EServer(t *testing.T) (*httptest.Server, *redis.Client) {
	t.Helper()
	rdb, err := redis.NewClientDB("localhost:6379", 9)
	if err != nil {
		t.Skip("redis not available; skipping CLI end-to-end test")
	}
	if err := rdb.FlushDB(context.Background()).Err(); err != nil {
		t.Fatalf("flush test db: %v", err)
	}
	srv := apisrv.NewWithOptions(apisrv.ServerOptions{
		DataDir:      t.TempDir(),
		Redis:        rdb,
		CookieSecure: false,
		PublicURL:    "http://localhost:8080",
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ts.Close()
		rdb.FlushDB(context.Background())
		rdb.Close()
	})
	return ts, rdb
}

// createTeamProject creates a project through the same device-authenticated
// route the CLI uses and returns its id.
func createTeamProject(t *testing.T, cfg *clinet.AgentConfig, creds *clinet.AgentCredentials, name string) string {
	t.Helper()
	path, err := clinet.TeamPath(cfg, "/projects")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := clinet.APIDo(cfg, creds, http.MethodPost, path, map[string]string{"name": name})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode project: %v", err)
	}
	if out.ID == "" {
		t.Fatal("server returned empty project id")
	}
	return out.ID
}

// rotateInvite mints a fresh invite code for the active team (owner/admin only).
func rotateInvite(t *testing.T, cfg *clinet.AgentConfig, creds *clinet.AgentCredentials) string {
	t.Helper()
	path, err := clinet.TeamPath(cfg, "/invite/rotate")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := clinet.APIDo(cfg, creds, http.MethodPost, path, nil)
	if err != nil {
		t.Fatalf("rotate invite: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		InviteCode string `json:"invite_code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode invite: %v", err)
	}
	if out.InviteCode == "" {
		t.Fatal("server returned empty invite code")
	}
	return out.InviteCode
}

// TestE2EV2CLIRegisterLoginTeamInitSync drives the whole released CLI flow
// against a real server: an owner registers, creates and selects a team, runs
// the local-only init, creates a project, and pushes a file through the Syncer's
// Device-authenticated lock/apply. A second user registers, joins the team, and
// pulls the snapshot to prove the file propagated. Finally the second user logs
// out and their now-revoked Device Session is rejected with 401.
func TestE2EV2CLIRegisterLoginTeamInitSync(t *testing.T) {
	ts, _ := newE2EServer(t)

	const (
		ownerPass  = "correct horse battery staple"
		memberPass = "another correct horse pw"
		fileBody   = "<html><body>e2e-hello</body></html>"
	)

	// --- Owner: register, create+select team, local init, project, sync push ---
	ownerHome := t.TempDir()
	t.Setenv("HOME", ownerHome)

	ownerIO := clinet.AccountIO{Out: io.Discard, Err: io.Discard, Password: e2ePassword(ownerPass)}
	if err := clinet.RunRegister(ts.URL, "e2eowner", "laptop", ownerIO); err != nil {
		t.Fatalf("owner register: %v", err)
	}
	if err := clinet.RunTeamCreate("Product"); err != nil {
		t.Fatalf("owner team create: %v", err)
	}

	ownerCfg, ownerCreds, err := clinet.LoadAuth()
	if err != nil {
		t.Fatalf("owner load auth: %v", err)
	}
	teamID := ownerCfg.CurrentTeam
	if teamID == "" {
		t.Fatal("owner has no active team after team create")
	}

	// Local-only init with tmux/claude stubbed: proves no network registration.
	e2ePrereqs(t)
	stubLaunch(t)
	if err := RunInit(&InitOptions{Path: filepath.Join(ownerHome, "agent_team"), Agent: "claude"}); err != nil {
		t.Fatalf("owner init: %v", err)
	}
	// Init must preserve the logged-in identity and selected team.
	if postInit, err := clinet.LoadConfig(); err != nil {
		t.Fatalf("reload config after init: %v", err)
	} else if postInit.CurrentTeam != teamID || postInit.UserID != ownerCfg.UserID {
		t.Fatalf("init clobbered identity: %+v", postInit)
	}

	projectID := createTeamProject(t, ownerCfg, ownerCreds, "Prototype")

	// Push a file via the Syncer using the stored Device Session (lock + apply).
	ownerDir := filepath.Join(ownerHome, "proto")
	if err := os.MkdirAll(ownerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ownerDir, "index.html"), []byte(fileBody), 0o644); err != nil {
		t.Fatal(err)
	}
	pusher := &Syncer{
		Project:       projectID,
		LocalDir:      ownerDir,
		Session:       "main",
		Device:        ownerCfg.Device,
		DeviceID:      ownerCfg.DeviceID,
		UserID:        ownerCfg.UserID,
		TeamID:        teamID,
		Server:        ts.URL,
		DeviceSession: ownerCreds.DeviceSession,
	}
	pusher.initDefaults()
	if conflict, err := pusher.acquireLock(context.Background(), "index.html"); err != nil || conflict {
		t.Fatalf("owner acquire lock: conflict=%v err=%v", conflict, err)
	}
	if conflict, err := pusher.applyOne(context.Background(), "index.html", fileBody); err != nil || conflict {
		t.Fatalf("owner apply: conflict=%v err=%v", conflict, err)
	}

	invite := rotateInvite(t, ownerCfg, ownerCreds)

	// --- Member: register, join team, pull snapshot, verify propagation ---
	memberHome := t.TempDir()
	t.Setenv("HOME", memberHome)

	memberIO := clinet.AccountIO{Out: io.Discard, Err: io.Discard, Password: e2ePassword(memberPass)}
	if err := clinet.RunRegister(ts.URL, "e2emember", "desktop", memberIO); err != nil {
		t.Fatalf("member register: %v", err)
	}
	if err := clinet.RunTeamJoin(teamID, invite); err != nil {
		t.Fatalf("member team join: %v", err)
	}

	memberCfg, memberCreds, err := clinet.LoadAuth()
	if err != nil {
		t.Fatalf("member load auth: %v", err)
	}
	if memberCfg.CurrentTeam != teamID {
		t.Fatalf("member active team = %q; want %q", memberCfg.CurrentTeam, teamID)
	}

	memberDir := filepath.Join(memberHome, "proto")
	puller := &Syncer{
		Project:       projectID,
		LocalDir:      memberDir,
		Session:       "main",
		Device:        memberCfg.Device,
		DeviceID:      memberCfg.DeviceID,
		UserID:        memberCfg.UserID,
		TeamID:        teamID,
		Server:        ts.URL,
		DeviceSession: memberCreds.DeviceSession,
	}
	if err := puller.pullSnapshot(); err != nil {
		t.Fatalf("member pull snapshot: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(memberDir, "index.html"))
	if err != nil {
		t.Fatalf("member missing synced file: %v", err)
	}
	if string(got) != fileBody {
		t.Fatalf("member synced content = %q; want %q", got, fileBody)
	}

	// --- Logout revokes the Device Session: subsequent Device calls are 401 ---
	if err := clinet.RunLogout(clinet.AccountIO{Out: io.Discard, Err: io.Discard}); err != nil {
		t.Fatalf("member logout: %v", err)
	}
	if _, err := clinet.APIDo(memberCfg, memberCreds, http.MethodGet, "/api/teams", nil); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected 401 with revoked device session, got %v", err)
	}
}
