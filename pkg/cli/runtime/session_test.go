package rt

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	api "github.com/team/agentlink/pkg/cli/net"
)

// seedSessionEnv writes a logged-in v2 environment (account config with an
// active team + device-session credential) rooted at a temp HOME, with
// base_dir = HOME so session directories land under it.
func seedSessionEnv(t *testing.T, serverURL string) string {
	t.Helper()
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	agentlinkDir := filepath.Join(homeDir, ".agentlink")
	os.MkdirAll(agentlinkDir, 0755)
	api.WriteAccountConfig(filepath.Join(agentlinkDir, "config.toml"), api.AgentConfig{
		Server:      serverURL,
		UserID:      "u_1",
		Username:    "kirby",
		DeviceID:    "d_1",
		Device:      "test-device",
		CurrentTeam: "tm_alpha",
		BaseDir:     homeDir,
	})
	api.WriteCredentials(filepath.Join(agentlinkDir, "credentials.json"), api.AgentCredentials{DeviceSession: "ds_test"})
	return homeDir
}

func TestRunSessionAdd(t *testing.T) {
	stubLaunch(t)

	// Mock server that handles GET /agents and PATCH /agents/sessions.
	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/teams/tm_alpha/agents":
			if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Device ") {
				t.Errorf("expected Device auth, got %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"agents": []map[string]any{
					{"device_id": "d_1", "device_name": "test-device", "sessions": []string{"main", "worker"}},
				},
			})
		case "/api/teams/tm_alpha/agents/sessions":
			var req map[string][]string
			json.NewDecoder(r.Body).Decode(&req)
			r.Body.Close()
			sessions := req["sessions"]
			if len(sessions) != 3 || sessions[2] != "reviewer" {
				t.Errorf("expected sessions=[main,worker,reviewer], got %v", sessions)
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"sessions": sessions})
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer mockSrv.Close()

	t.Run("add session success", func(t *testing.T) {
		homeDir := seedSessionEnv(t, mockSrv.URL)

		if err := RunSessionAdd("reviewer"); err != nil {
			t.Fatal(err)
		}

		sessionDir := filepath.Join(homeDir, "reviewer")
		if _, err := os.Stat(sessionDir); err != nil {
			t.Errorf("expected session directory to exist: %s", sessionDir)
		}
		if _, err := os.Stat(filepath.Join(sessionDir, ".agentlink.toml")); err != nil {
			t.Errorf("expected .agentlink.toml to exist")
		}
		if _, err := os.Stat(filepath.Join(sessionDir, "CLAUDE.md")); err != nil {
			t.Errorf("expected CLAUDE.md to exist")
		}
	})

	t.Run("add duplicate session", func(t *testing.T) {
		seedSessionEnv(t, mockSrv.URL)

		err := RunSessionAdd("main")
		if err == nil {
			t.Fatal("expected error for duplicate session")
		}
		if !strings.Contains(err.Error(), "already registered") {
			t.Errorf("expected 'already registered' error, got: %s", err)
		}
	})

	t.Run("add session missing config", func(t *testing.T) {
		homeDir := t.TempDir()
		t.Setenv("HOME", homeDir)

		err := RunSessionAdd("reviewer")
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "config file not found") {
			t.Errorf("expected config error, got: %s", err)
		}
	})
}

// TestSessionAddPreservesAccountConfig verifies that recording a new session's
// id (via UpdateSessionID) leaves the account identity and current team intact.
func TestSessionAddPreservesAccountConfig(t *testing.T) {
	stubLaunch(t)

	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/teams/tm_alpha/agents":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"agents": []map[string]any{
					{"device_id": "d_1", "device_name": "test-device", "sessions": []string{"main"}},
				},
			})
		case "/api/teams/tm_alpha/agents/sessions":
			var req map[string][]string
			json.NewDecoder(r.Body).Decode(&req)
			r.Body.Close()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"sessions": req["sessions"]})
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer mockSrv.Close()

	homeDir := seedSessionEnv(t, mockSrv.URL)

	if err := RunSessionAdd("reviewer"); err != nil {
		t.Fatal(err)
	}

	cfg, err := api.LoadConfig()
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if cfg.UserID != "u_1" || cfg.Username != "kirby" || cfg.DeviceID != "d_1" {
		t.Errorf("account identity not preserved: %+v", cfg)
	}
	if cfg.CurrentTeam != "tm_alpha" {
		t.Errorf("current team not preserved: %q", cfg.CurrentTeam)
	}
	_ = homeDir
}

func TestRunSessionRemove(t *testing.T) {
	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/teams/tm_alpha/agents":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"agents": []map[string]any{
					{"device_id": "d_1", "device_name": "test-device", "sessions": []string{"main", "worker", "reviewer"}},
				},
			})
		case "/api/teams/tm_alpha/agents/sessions":
			var req map[string][]string
			json.NewDecoder(r.Body).Decode(&req)
			r.Body.Close()
			sessions := req["sessions"]
			if len(sessions) != 2 {
				t.Errorf("expected 2 sessions after removal, got %v", sessions)
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"sessions": sessions})
		}
	}))
	defer mockSrv.Close()

	t.Run("remove session success", func(t *testing.T) {
		homeDir := seedSessionEnv(t, mockSrv.URL)
		os.MkdirAll(filepath.Join(homeDir, "reviewer"), 0755)

		if err := RunSessionRemove("reviewer"); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(homeDir, "reviewer")); err == nil {
			t.Error("expected session directory to be removed")
		}
	})

	t.Run("remove non-existing session", func(t *testing.T) {
		seedSessionEnv(t, mockSrv.URL)

		err := RunSessionRemove("nonexistent")
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "not found") {
			t.Errorf("expected 'not found' error, got: %s", err)
		}
	})
}

func TestRunUninstall(t *testing.T) {
	t.Run("uninstall success revokes device session", func(t *testing.T) {
		var gotMethod, gotPath, gotAuth string
		mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod = r.Method
			gotPath = r.URL.Path
			gotAuth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		}))
		defer mockSrv.Close()

		homeDir := seedSessionEnv(t, mockSrv.URL)
		agentlinkDir := filepath.Join(homeDir, ".agentlink")

		if err := RunUninstall(); err != nil {
			t.Fatal(err)
		}
		if gotMethod != "POST" || gotPath != "/api/auth/device-logout" {
			t.Errorf("expected POST /api/auth/device-logout, got %s %s", gotMethod, gotPath)
		}
		if !strings.HasPrefix(gotAuth, "Device ") {
			t.Errorf("expected Device auth, got %q", gotAuth)
		}
		if _, err := os.Stat(agentlinkDir); err == nil {
			t.Error("expected .agentlink directory to be removed")
		}
	})

	t.Run("uninstall server error keeps local files", func(t *testing.T) {
		mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": "server error"})
		}))
		defer mockSrv.Close()

		homeDir := seedSessionEnv(t, mockSrv.URL)
		agentlinkDir := filepath.Join(homeDir, ".agentlink")

		if err := RunUninstall(); err == nil {
			t.Fatal("expected error when device-logout fails")
		}
		if _, err := os.Stat(agentlinkDir); err != nil {
			t.Error("expected .agentlink directory to remain after API failure")
		}
	})

	t.Run("uninstall missing config", func(t *testing.T) {
		homeDir := t.TempDir()
		t.Setenv("HOME", homeDir)

		err := RunUninstall()
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "config file not found") {
			t.Errorf("expected config error, got: %s", err)
		}
	})
}

func TestRunAttach_errors(t *testing.T) {
	t.Run("attach missing config", func(t *testing.T) {
		homeDir := t.TempDir()
		t.Setenv("HOME", homeDir)

		err := RunAttach("main")
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "config file not found") {
			t.Errorf("expected config error, got: %s", err)
		}
	})

	t.Run("attach directory not found", func(t *testing.T) {
		seedSessionEnv(t, "http://localhost:1")

		err := RunAttach("nonexistent")
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "not found") {
			t.Errorf("expected 'not found' error, got: %s", err)
		}
	})
}
