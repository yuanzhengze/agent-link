package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestRunPing(t *testing.T) {
	var authHeader, capturedPath string

	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		capturedPath = r.URL.Path

		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	}))
	defer mockSrv.Close()

	t.Run("ping success", func(t *testing.T) {
		authHeader = ""
		sessionDir := setupTaskEnv(t, mockSrv.URL)
		origWd, _ := os.Getwd()
		os.Chdir(sessionDir)
		defer os.Chdir(origWd)

		if err := RunPing(); err != nil {
			t.Fatal(err)
		}
		if capturedPath != "/api/teams/tm_alpha/agents/heartbeat" {
			t.Errorf("expected /api/teams/tm_alpha/agents/heartbeat, got %s", capturedPath)
		}
		if !strings.HasPrefix(authHeader, "Device ") {
			t.Errorf("expected Device auth, got %q", authHeader)
		}
	})

	t.Run("ping server error", func(t *testing.T) {
		errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": "internal error"})
		}))
		defer errSrv.Close()

		sessionDir := setupTaskEnv(t, errSrv.URL)
		origWd, _ := os.Getwd()
		os.Chdir(sessionDir)
		defer os.Chdir(origWd)

		err := RunPing()
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "500") {
			t.Errorf("expected 500 error, got: %s", err)
		}
	})
}

func TestRunList(t *testing.T) {
	makeAgent := func(username, device string, sessions []string, online bool, lastSeen string) map[string]any {
		return map[string]any{
			"username":    username,
			"device_name": device,
			"sessions":    sessions,
			"online":      online,
			"last_seen":   lastSeen,
		}
	}

	t.Run("list team roster", func(t *testing.T) {
		mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/teams/tm_alpha/agents" {
				t.Errorf("expected /api/teams/tm_alpha/agents, got %s", r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"agents": []map[string]any{
					makeAgent("kirby", "my-dev", []string{"main", "worker"}, true, "2026-05-03T12:00:00Z"),
				},
			})
		}))
		defer mockSrv.Close()

		sessionDir := setupTaskEnv(t, mockSrv.URL)
		origWd, _ := os.Getwd()
		os.Chdir(sessionDir)
		defer os.Chdir(origWd)

		if err := RunList(false); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("list all flag still targets team roster", func(t *testing.T) {
		mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/teams/tm_alpha/agents" {
				t.Errorf("expected /api/teams/tm_alpha/agents, got %s", r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"agents": []map[string]any{
					makeAgent("kirby", "dev-a", []string{"main"}, true, "2026-05-03T12:00:00Z"),
					makeAgent("alice", "dev-b", []string{"worker"}, false, "2026-05-03T10:00:00Z"),
				},
			})
		}))
		defer mockSrv.Close()

		sessionDir := setupTaskEnv(t, mockSrv.URL)
		origWd, _ := os.Getwd()
		os.Chdir(sessionDir)
		defer os.Chdir(origWd)

		if err := RunList(true); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("list empty response", func(t *testing.T) {
		mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"agents": []map[string]any{},
			})
		}))
		defer mockSrv.Close()

		sessionDir := setupTaskEnv(t, mockSrv.URL)
		origWd, _ := os.Getwd()
		os.Chdir(sessionDir)
		defer os.Chdir(origWd)

		if err := RunList(false); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("list server error", func(t *testing.T) {
		mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": "server error"})
		}))
		defer mockSrv.Close()

		sessionDir := setupTaskEnv(t, mockSrv.URL)
		origWd, _ := os.Getwd()
		os.Chdir(sessionDir)
		defer os.Chdir(origWd)

		err := RunList(false)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "500") {
			t.Errorf("expected 500 error, got: %s", err)
		}
	})

	t.Run("list no sessions", func(t *testing.T) {
		mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"agents": []map[string]any{
					makeAgent("kirby", "empty-sess", []string{}, false, ""),
				},
			})
		}))
		defer mockSrv.Close()

		sessionDir := setupTaskEnv(t, mockSrv.URL)
		origWd, _ := os.Getwd()
		os.Chdir(sessionDir)
		defer os.Chdir(origWd)

		if err := RunList(false); err != nil {
			t.Fatal(err)
		}
	})
}

func TestRunPingList_errors(t *testing.T) {
	writeLoggedInConfig := func(t *testing.T, serverURL string) {
		t.Helper()
		homeDir := t.TempDir()
		t.Setenv("HOME", homeDir)
		os.MkdirAll(homeDir+"/.agentlink", 0755)
		WriteAccountConfig(homeDir+"/.agentlink/config.toml", AgentConfig{
			Server: serverURL, UserID: "u_1", Username: "kirby", DeviceID: "d_1", Device: "test-dev", CurrentTeam: "tm_alpha",
		})
	}

	t.Run("ping missing config", func(t *testing.T) {
		homeDir := t.TempDir()
		t.Setenv("HOME", homeDir)

		err := RunPing()
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "config file not found") {
			t.Errorf("expected config file error, got: %s", err)
		}
	})

	t.Run("list missing config", func(t *testing.T) {
		homeDir := t.TempDir()
		t.Setenv("HOME", homeDir)

		err := RunList(false)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "config file not found") {
			t.Errorf("expected config file error, got: %s", err)
		}
	})

	t.Run("ping missing credentials", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		defer srv.Close()
		writeLoggedInConfig(t, srv.URL)

		err := RunPing()
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "credentials file not found") {
			t.Errorf("expected credentials error, got: %s", err)
		}
	})

	t.Run("list missing credentials", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		defer srv.Close()
		writeLoggedInConfig(t, srv.URL)

		err := RunList(false)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "credentials file not found") {
			t.Errorf("expected credentials error, got: %s", err)
		}
	})
}
