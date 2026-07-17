package rt

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	api "github.com/team/agentlink/pkg/cli/net"
)

// TestResumeRejectsRemovedCurrentTeam proves the pre-launch team/device check
// fails when the current team is no longer accessible (e.g. the member was
// removed): the team heartbeat returns 404 and pingServer surfaces an error, so
// resume aborts before touching tmux.
func TestResumeRejectsRemovedCurrentTeam(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/teams/tm_gone/agents/heartbeat" {
			t.Errorf("expected team heartbeat path, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"team not found"}`))
	}))
	defer srv.Close()

	cfg := &api.AgentConfig{Server: srv.URL, DeviceID: "d_1", Device: "dev", CurrentTeam: "tm_gone"}
	creds := &api.AgentCredentials{DeviceSession: "ds_test"}

	if err := pingServer(cfg, creds); err == nil {
		t.Fatal("expected pingServer to fail for a removed team membership")
	}
}

func TestPingServerSucceedsForCurrentTeam(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Device ds_test" {
			t.Errorf("expected Device auth, got %q", got)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	cfg := &api.AgentConfig{Server: srv.URL, DeviceID: "d_1", Device: "dev", CurrentTeam: "tm_alpha"}
	creds := &api.AgentCredentials{DeviceSession: "ds_test"}

	if err := pingServer(cfg, creds); err != nil {
		t.Fatalf("expected pingServer to succeed, got %v", err)
	}
}

func TestReadTOMLSection(t *testing.T) {
	content := `server = "http://example.com"
device = "my-device"

[poll]
enabled = true
interval = 5

[sessions]
main = "abc-123"
worker = "def-456"
`
	sessions := api.ReadTOMLSection(content, "sessions")
	if sessions == nil {
		t.Fatal("expected non-nil sessions map")
	}
	if sessions["main"] != "abc-123" {
		t.Errorf("main = %q, want abc-123", sessions["main"])
	}
	if sessions["worker"] != "def-456" {
		t.Errorf("worker = %q, want def-456", sessions["worker"])
	}
}

func TestReadTOMLSection_absent(t *testing.T) {
	content := `server = "http://example.com"
device = "my-device"
`
	sessions := api.ReadTOMLSection(content, "sessions")
	if sessions != nil {
		t.Errorf("expected nil for absent section, got %v", sessions)
	}
}

func TestReadTOMLSection_empty(t *testing.T) {
	content := `server = "http://example.com"

[sessions]
`
	sessions := api.ReadTOMLSection(content, "sessions")
	if sessions != nil {
		t.Errorf("expected nil for empty section, got %v", sessions)
	}
}

func TestUpdateSessionID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	// Initial config without [sessions]
	api.WriteConfigTOML(path, "http://srv:8080", "dev", "/tmp", "claude", false, nil)

	if err := api.UpdateSessionID(path, "main", "id-main-001"); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(path)
	cfg, err := loadConfigFromBytes(string(data))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sessions["main"] != "id-main-001" {
		t.Errorf("after first update, main = %q, want id-main-001", cfg.Sessions["main"])
	}

	// Update a second session — [sessions] already exists
	if err := api.UpdateSessionID(path, "worker", "id-worker-002"); err != nil {
		t.Fatal(err)
	}

	data, _ = os.ReadFile(path)
	cfg, err = loadConfigFromBytes(string(data))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sessions["main"] != "id-main-001" {
		t.Errorf("after second update, main = %q, want id-main-001", cfg.Sessions["main"])
	}
	if cfg.Sessions["worker"] != "id-worker-002" {
		t.Errorf("after second update, worker = %q, want id-worker-002", cfg.Sessions["worker"])
	}

	// Overwrite an existing session_id
	if err := api.UpdateSessionID(path, "main", "id-main-v2"); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	cfg, err = loadConfigFromBytes(string(data))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sessions["main"] != "id-main-v2" {
		t.Errorf("after overwrite, main = %q, want id-main-v2", cfg.Sessions["main"])
	}
}

func TestResumeSessionList_withSessions(t *testing.T) {
	cfg := &api.AgentConfig{
		BaseDir:  "/tmp/irrelevant",
		Sessions: map[string]string{"worker": "w-id", "main": "m-id"},
	}
	names, fallback := resumeSessionList(cfg)
	if fallback {
		t.Error("expected fallback=false when [sessions] present")
	}
	if len(names) != 2 || names[0] != "main" || names[1] != "worker" {
		t.Errorf("expected sorted [main worker], got %v", names)
	}
}

func TestResumeSessionList_legacyFallback(t *testing.T) {
	// Build a fake base_dir with main/ and worker/ subdirs containing .agentlink.toml
	baseDir := t.TempDir()
	for _, s := range []string{"main", "worker"} {
		dir := filepath.Join(baseDir, s)
		os.MkdirAll(dir, 0755)
		api.WriteSessionTOML(filepath.Join(dir, ".agentlink.toml"), s, "dev")
	}
	// Also add a non-session directory (no .agentlink.toml) to ensure it's skipped
	os.MkdirAll(filepath.Join(baseDir, "not-a-session"), 0755)

	cfg := &api.AgentConfig{BaseDir: baseDir, Sessions: nil}
	names, fallback := resumeSessionList(cfg)
	if !fallback {
		t.Error("expected fallback=true when [sessions] absent")
	}
	if len(names) != 2 || names[0] != "main" || names[1] != "worker" {
		t.Errorf("expected [main worker] from disk scan, got %v", names)
	}
}

func TestResumeSessionList_emptyBaseDir(t *testing.T) {
	baseDir := t.TempDir()
	cfg := &api.AgentConfig{BaseDir: baseDir, Sessions: nil}
	names, fallback := resumeSessionList(cfg)
	if !fallback {
		t.Error("expected fallback=true for empty base_dir")
	}
	if len(names) != 0 {
		t.Errorf("expected no sessions, got %v", names)
	}
}

// loadConfigFromBytes parses config content directly without touching $HOME.
func loadConfigFromBytes(content string) (*api.AgentConfig, error) {
	cfg := &api.AgentConfig{
		Server:  api.ReadTOML(content, "server"),
		Device:  api.ReadTOML(content, "device"),
		BaseDir: api.ReadTOML(content, "base_dir"),
		Agent:   api.ReadTOML(content, "agent"),
		Poll: api.PollConfig{
			Enabled:  api.ReadTOMLBool(content, "poll.enabled", true),
			Interval: api.ReadTOMLInt(content, "poll.interval", 5),
		},
		Sessions: api.ReadTOMLSection(content, "sessions"),
	}
	if cfg.Agent == "" {
		cfg.Agent = "claude"
	}
	return cfg, nil
}
