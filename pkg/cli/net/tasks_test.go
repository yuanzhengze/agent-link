package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupTaskEnv writes a v2 logged-in environment (account config with an active
// team + a device-session credential) and returns the "worker" session dir so a
// test can Chdir into it; FindCurrentSession then resolves to "worker".
func setupTaskEnv(t *testing.T, serverURL string) string {
	t.Helper()

	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	agentlinkDir := filepath.Join(homeDir, ".agentlink")
	os.MkdirAll(agentlinkDir, 0755)
	WriteAccountConfig(filepath.Join(agentlinkDir, "config.toml"), AgentConfig{
		Server:      serverURL,
		UserID:      "u_1",
		Username:    "kirby",
		DeviceID:    "d_1",
		Device:      "test-device",
		CurrentTeam: "tm_alpha",
	})
	WriteCredentials(filepath.Join(agentlinkDir, "credentials.json"), AgentCredentials{DeviceSession: "ds_test"})

	sessionDir := filepath.Join(homeDir, "worker")
	os.MkdirAll(sessionDir, 0755)
	WriteSessionTOML(filepath.Join(sessionDir, ".agentlink.toml"), "worker", "test-device")

	return sessionDir
}

func TestRunTaskSend(t *testing.T) {
	type capture struct {
		path       string
		to         string
		fromField  string
		taskID     string
		content    string
		authHeader string
		session    string
	}
	var captured capture

	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.path = r.URL.Path
		captured.authHeader = r.Header.Get("Authorization")
		captured.session = r.Header.Get("X-Agentlink-Session")

		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		r.Body.Close()
		captured.to, _ = req["to"].(string)
		captured.fromField, _ = req["from_session"].(string)
		captured.taskID, _ = req["task_id"].(string)
		captured.content, _ = req["content"].(string)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"id": "test-msg-id"})
	}))
	defer mockSrv.Close()

	sessionDir := setupTaskEnv(t, mockSrv.URL)

	t.Run("send with short name targets own device", func(t *testing.T) {
		captured = capture{}
		origWd, _ := os.Getwd()
		os.Chdir(sessionDir)
		defer os.Chdir(origWd)

		if err := RunTaskSend("worker", "001", "fix login bug", false, ""); err != nil {
			t.Fatal(err)
		}
		if captured.path != "/api/teams/tm_alpha/tasks" {
			t.Errorf("expected /api/teams/tm_alpha/tasks, got %s", captured.path)
		}
		if captured.to != "d_1:worker" {
			t.Errorf("expected to=d_1:worker, got %s", captured.to)
		}
		if captured.fromField != "" {
			t.Errorf("from_session must not be sent; got %q", captured.fromField)
		}
		if captured.session != "worker" {
			t.Errorf("expected X-Agentlink-Session=worker, got %s", captured.session)
		}
		if !strings.HasPrefix(captured.authHeader, "Device ") {
			t.Errorf("expected Device auth, got %q", captured.authHeader)
		}
		if captured.taskID != "001" || captured.content != "fix login bug" {
			t.Errorf("unexpected task fields: %+v", captured)
		}
	})

	t.Run("send with full device:session name", func(t *testing.T) {
		captured = capture{}
		origWd, _ := os.Getwd()
		os.Chdir(sessionDir)
		defer os.Chdir(origWd)

		if err := RunTaskSend("other-dev:reviewer", "002", "review code", false, ""); err != nil {
			t.Fatal(err)
		}
		if captured.to != "other-dev:reviewer" {
			t.Errorf("expected to=other-dev:reviewer, got %s", captured.to)
		}
	})

	t.Run("send server 409 shows message", func(t *testing.T) {
		mockSrv409 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]any{
				"error": "target session is busy",
				"recipient_status": map[string]any{
					"device_id": "d_2",
					"session":   "main",
					"current":   "task: deploy-042",
				},
			})
		}))
		defer mockSrv409.Close()

		sessionDir := setupTaskEnv(t, mockSrv409.URL)
		origWd, _ := os.Getwd()
		os.Chdir(sessionDir)
		defer os.Chdir(origWd)

		if err := RunTaskSend("worker", "001", "test", false, ""); err != nil {
			t.Errorf("expected no error for 409 with status, got: %s", err)
		}
	})
}

func TestRunTaskSend_errors(t *testing.T) {
	t.Run("missing config", func(t *testing.T) {
		homeDir := t.TempDir()
		t.Setenv("HOME", homeDir)

		err := RunTaskSend("worker", "001", "hi", false, "")
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "config file not found") {
			t.Errorf("expected config file error, got: %s", err)
		}
	})

	t.Run("missing credentials", func(t *testing.T) {
		homeDir := t.TempDir()
		t.Setenv("HOME", homeDir)
		os.MkdirAll(filepath.Join(homeDir, ".agentlink"), 0755)
		WriteAccountConfig(filepath.Join(homeDir, ".agentlink", "config.toml"), AgentConfig{
			Server: "http://localhost:1", UserID: "u_1", Username: "k", DeviceID: "d_1", Device: "test-dev", CurrentTeam: "tm_alpha",
		})

		err := RunTaskSend("worker", "001", "hi", false, "")
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "credentials file not found") {
			t.Errorf("expected credentials error, got: %s", err)
		}
	})

	t.Run("missing session file", func(t *testing.T) {
		homeDir := t.TempDir()
		t.Setenv("HOME", homeDir)
		os.MkdirAll(filepath.Join(homeDir, ".agentlink"), 0755)
		WriteAccountConfig(filepath.Join(homeDir, ".agentlink", "config.toml"), AgentConfig{
			Server: "http://localhost:1", UserID: "u_1", Username: "k", DeviceID: "d_1", Device: "test-dev", CurrentTeam: "tm_alpha",
		})
		WriteCredentials(filepath.Join(homeDir, ".agentlink", "credentials.json"), AgentCredentials{DeviceSession: "ds_x"})

		err := RunTaskSend("worker", "001", "hi", false, "")
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), ".agentlink.toml not found") {
			t.Errorf("expected .agentlink.toml error, got: %s", err)
		}
	})
}

func TestRunTaskResult(t *testing.T) {
	var captured struct {
		path   string
		status string
		result string
	}

	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.path = r.URL.Path
		var req map[string]string
		json.NewDecoder(r.Body).Decode(&req)
		r.Body.Close()
		captured.status = req["status"]
		captured.result = req["result"]

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	}))
	defer mockSrv.Close()

	t.Run("result completed puts task id in the path", func(t *testing.T) {
		sessionDir := setupTaskEnv(t, mockSrv.URL)
		origWd, _ := os.Getwd()
		os.Chdir(sessionDir)
		defer os.Chdir(origWd)

		if err := RunTaskResult("001", "completed", "bug fixed"); err != nil {
			t.Fatal(err)
		}
		if captured.path != "/api/teams/tm_alpha/tasks/001/result" {
			t.Errorf("expected /api/teams/tm_alpha/tasks/001/result, got %s", captured.path)
		}
		if captured.status != "completed" || captured.result != "bug fixed" {
			t.Errorf("unexpected body: %+v", captured)
		}
	})

	t.Run("result not found", func(t *testing.T) {
		mockSrv404 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "task not found"})
		}))
		defer mockSrv404.Close()

		sessionDir := setupTaskEnv(t, mockSrv404.URL)
		origWd, _ := os.Getwd()
		os.Chdir(sessionDir)
		defer os.Chdir(origWd)

		err := RunTaskResult("999", "completed", "x")
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "404") {
			t.Errorf("expected 404 error, got: %s", err)
		}
	})
}

func TestRunTaskResume(t *testing.T) {
	var captured struct {
		path    string
		content string
	}

	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.path = r.URL.Path
		var req map[string]string
		json.NewDecoder(r.Body).Decode(&req)
		r.Body.Close()
		captured.content = req["content"]

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	}))
	defer mockSrv.Close()

	sessionDir := setupTaskEnv(t, mockSrv.URL)
	origWd, _ := os.Getwd()
	os.Chdir(sessionDir)
	defer os.Chdir(origWd)

	if err := RunTaskResume("001", "new guidance: do X first"); err != nil {
		t.Fatal(err)
	}
	if captured.path != "/api/teams/tm_alpha/tasks/001/resume" {
		t.Errorf("expected /api/teams/tm_alpha/tasks/001/resume, got %s", captured.path)
	}
	if captured.content != "new guidance: do X first" {
		t.Errorf("content mismatch, got %s", captured.content)
	}
}

func TestRunTaskCancel(t *testing.T) {
	var capturedPath string

	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	}))
	defer mockSrv.Close()

	sessionDir := setupTaskEnv(t, mockSrv.URL)
	origWd, _ := os.Getwd()
	os.Chdir(sessionDir)
	defer os.Chdir(origWd)

	if err := RunTaskCancel("001"); err != nil {
		t.Fatal(err)
	}
	if capturedPath != "/api/teams/tm_alpha/tasks/001/cancel" {
		t.Errorf("expected /api/teams/tm_alpha/tasks/001/cancel, got %s", capturedPath)
	}
}

func TestRunTaskReopen(t *testing.T) {
	var captured struct {
		path   string
		reason string
	}

	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.path = r.URL.Path
		var req map[string]string
		json.NewDecoder(r.Body).Decode(&req)
		r.Body.Close()
		captured.reason = req["reason"]
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	}))
	defer mockSrv.Close()

	sessionDir := setupTaskEnv(t, mockSrv.URL)
	origWd, _ := os.Getwd()
	os.Chdir(sessionDir)
	defer os.Chdir(origWd)

	if err := RunTaskReopen("001", "changed requirements"); err != nil {
		t.Fatal(err)
	}
	if captured.path != "/api/teams/tm_alpha/tasks/001/reopen" {
		t.Errorf("expected /api/teams/tm_alpha/tasks/001/reopen, got %s", captured.path)
	}
	if captured.reason != "changed requirements" {
		t.Errorf("reason mismatch, got %s", captured.reason)
	}
}

func TestRunTaskStatus(t *testing.T) {
	t.Run("status success uses path param", func(t *testing.T) {
		mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/teams/tm_alpha/tasks/001" {
				t.Errorf("expected /api/teams/tm_alpha/tasks/001, got %s", r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{
				"task_id":      "001",
				"status":       "completed",
				"assigned_to":  "d_1:worker",
				"issued_by":    "d_1:main",
				"content":      "fix login bug",
				"result":       "bug fixed",
				"issued_at":    "2026-05-03T12:00:00Z",
				"completed_at": "2026-05-03T12:30:00Z",
			})
		}))
		defer mockSrv.Close()

		sessionDir := setupTaskEnv(t, mockSrv.URL)
		origWd, _ := os.Getwd()
		os.Chdir(sessionDir)
		defer os.Chdir(origWd)

		if err := RunTaskStatus("001"); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("status not found", func(t *testing.T) {
		mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "task not found"})
		}))
		defer mockSrv.Close()

		sessionDir := setupTaskEnv(t, mockSrv.URL)
		origWd, _ := os.Getwd()
		os.Chdir(sessionDir)
		defer os.Chdir(origWd)

		err := RunTaskStatus("999")
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "not found") {
			t.Errorf("expected not found error, got: %s", err)
		}
	})
}

func TestRunTaskList(t *testing.T) {
	t.Run("list with tasks", func(t *testing.T) {
		mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/teams/tm_alpha/tasks" {
				t.Errorf("expected /api/teams/tm_alpha/tasks, got %s", r.URL.Path)
			}
			if r.Header.Get("X-Agentlink-Session") != "worker" {
				t.Errorf("expected session header worker, got %s", r.Header.Get("X-Agentlink-Session"))
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"received": []map[string]string{
					{"task_id": "t-1", "status": "issued", "assigned_to": "d_1:worker", "issued_by": "d_1:main", "content": "task one", "issued_at": "2026-01-01T00:00:00Z"},
				},
				"sent": []map[string]string{
					{"task_id": "t-2", "status": "in_progress", "assigned_to": "d_1:worker", "issued_by": "d_1:main", "content": "task two", "issued_at": "2026-01-01T00:01:00Z"},
				},
			})
		}))
		defer mockSrv.Close()

		sessionDir := setupTaskEnv(t, mockSrv.URL)
		origWd, _ := os.Getwd()
		os.Chdir(sessionDir)
		defer os.Chdir(origWd)

		if err := RunTaskList(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("list empty", func(t *testing.T) {
		mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"received": []any{}, "sent": []any{}})
		}))
		defer mockSrv.Close()

		sessionDir := setupTaskEnv(t, mockSrv.URL)
		origWd, _ := os.Getwd()
		os.Chdir(sessionDir)
		defer os.Chdir(origWd)

		if err := RunTaskList(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("list missing config", func(t *testing.T) {
		homeDir := t.TempDir()
		t.Setenv("HOME", homeDir)

		err := RunTaskList()
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "config file not found") {
			t.Errorf("expected config error, got: %s", err)
		}
	})
}
