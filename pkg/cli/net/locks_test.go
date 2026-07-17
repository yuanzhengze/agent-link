package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestNetLockAcquire(t *testing.T) {
	var capturedMethod, capturedPath, capturedSession, capturedAuth string
	var capturedBody struct {
		ProjectID string `json:"project_id"`
		Session   string `json:"session"`
		Path      string `json:"path"`
	}

	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = r.Method
		capturedPath = r.URL.Path
		capturedSession = r.Header.Get("X-Agentlink-Session")
		capturedAuth = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&capturedBody)
		r.Body.Close()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"owner": map[string]string{"label": "kirby / test-device / worker"},
		})
	}))
	defer mockSrv.Close()

	sessionDir := setupTaskEnv(t, mockSrv.URL)
	origWd, _ := os.Getwd()
	os.Chdir(sessionDir)
	defer os.Chdir(origWd)

	if err := RunLockAcquire("proj123", "index.html"); err != nil {
		t.Fatal(err)
	}
	if capturedMethod != "POST" {
		t.Errorf("expected POST, got %s", capturedMethod)
	}
	if capturedPath != "/api/teams/tm_alpha/locks/acquire" {
		t.Errorf("expected /api/teams/tm_alpha/locks/acquire, got %s", capturedPath)
	}
	if !strings.HasPrefix(capturedAuth, "Device ") {
		t.Errorf("expected Device auth, got %q", capturedAuth)
	}
	if capturedBody.ProjectID != "proj123" {
		t.Errorf("expected project_id=proj123, got %q", capturedBody.ProjectID)
	}
	if capturedBody.Path != "index.html" {
		t.Errorf("expected path=index.html, got %q", capturedBody.Path)
	}
	if capturedBody.Session != "" {
		t.Errorf("session must not be in the body; got %q", capturedBody.Session)
	}
	// setupTaskEnv writes .agentlink.toml with session = "worker"; the caller's
	// session travels in the header, and the server derives ownership from it.
	if capturedSession != "worker" {
		t.Errorf("expected X-Agentlink-Session=worker, got %q", capturedSession)
	}
}

func TestNetLockAcquire_conflict(t *testing.T) {
	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]any{
			"error": "file is locked by another session",
			"owner": map[string]string{"label": "someone / other"},
		})
	}))
	defer mockSrv.Close()

	sessionDir := setupTaskEnv(t, mockSrv.URL)
	origWd, _ := os.Getwd()
	os.Chdir(sessionDir)
	defer os.Chdir(origWd)

	err := RunLockAcquire("proj123", "index.html")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "409") {
		t.Errorf("expected 409 error, got: %s", err)
	}
}

func TestNetLockAcquire_noSession(t *testing.T) {
	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be called when session cannot be resolved")
	}))
	defer mockSrv.Close()

	// A logged-in v2 environment, but Chdir into a dir with no .agentlink.toml so
	// FindCurrentSession fails before any request is made.
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	os.MkdirAll(homeDir+"/.agentlink", 0755)
	WriteAccountConfig(homeDir+"/.agentlink/config.toml", AgentConfig{
		Server: mockSrv.URL, UserID: "u_1", Username: "kirby", DeviceID: "d_1", Device: "test-device", CurrentTeam: "tm_alpha",
	})
	WriteCredentials(homeDir+"/.agentlink/credentials.json", AgentCredentials{DeviceSession: "ds_x"})

	noSessionDir := homeDir + "/no-session"
	os.MkdirAll(noSessionDir, 0755)
	origWd, _ := os.Getwd()
	os.Chdir(noSessionDir)
	defer os.Chdir(origWd)

	err := RunLockAcquire("proj123", "index.html")
	if err == nil {
		t.Fatal("expected error when no session is found")
	}
	if !strings.Contains(err.Error(), ".agentlink.toml") {
		t.Errorf("expected .agentlink.toml error, got: %s", err)
	}
}

func TestNetLockRelease(t *testing.T) {
	var capturedMethod, capturedPath, capturedSession string
	var capturedBody struct {
		ProjectID string `json:"project_id"`
		Session   string `json:"session"`
		Path      string `json:"path"`
	}

	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = r.Method
		capturedPath = r.URL.Path
		capturedSession = r.Header.Get("X-Agentlink-Session")
		json.NewDecoder(r.Body).Decode(&capturedBody)
		r.Body.Close()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	}))
	defer mockSrv.Close()

	sessionDir := setupTaskEnv(t, mockSrv.URL)
	origWd, _ := os.Getwd()
	os.Chdir(sessionDir)
	defer os.Chdir(origWd)

	if err := RunLockRelease("proj123", "index.html"); err != nil {
		t.Fatal(err)
	}
	if capturedMethod != "POST" {
		t.Errorf("expected POST, got %s", capturedMethod)
	}
	if capturedPath != "/api/teams/tm_alpha/locks/release" {
		t.Errorf("expected /api/teams/tm_alpha/locks/release, got %s", capturedPath)
	}
	if capturedBody.ProjectID != "proj123" || capturedBody.Path != "index.html" {
		t.Errorf("unexpected body: %+v", capturedBody)
	}
	if capturedSession != "worker" {
		t.Errorf("expected X-Agentlink-Session=worker, got %q", capturedSession)
	}
}

func TestNetLockRelease_conflict(t *testing.T) {
	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]any{
			"error": "file is locked by another session",
			"owner": map[string]string{"label": "someone / other"},
		})
	}))
	defer mockSrv.Close()

	sessionDir := setupTaskEnv(t, mockSrv.URL)
	origWd, _ := os.Getwd()
	os.Chdir(sessionDir)
	defer os.Chdir(origWd)

	err := RunLockRelease("proj123", "index.html")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "409") {
		t.Errorf("expected 409 error, got: %s", err)
	}
}

func TestNetLockList(t *testing.T) {
	var capturedMethod, capturedPath, capturedProject string

	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = r.Method
		capturedPath = r.URL.Path
		capturedProject = r.URL.Query().Get("project_id")

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"locks": []map[string]any{
				{"path": "index.html", "owner": map[string]string{"label": "kirby / worker"}, "acquired_at": int64(1000), "lease_expires_at": int64(1120)},
			},
		})
	}))
	defer mockSrv.Close()

	sessionDir := setupTaskEnv(t, mockSrv.URL)
	origWd, _ := os.Getwd()
	os.Chdir(sessionDir)
	defer os.Chdir(origWd)

	if err := RunLockList("proj123"); err != nil {
		t.Fatal(err)
	}
	if capturedMethod != "GET" {
		t.Errorf("expected GET, got %s", capturedMethod)
	}
	if capturedPath != "/api/teams/tm_alpha/locks" {
		t.Errorf("expected /api/teams/tm_alpha/locks, got %s", capturedPath)
	}
	if capturedProject != "proj123" {
		t.Errorf("expected project_id=proj123, got %q", capturedProject)
	}
}

func TestNetLockList_empty(t *testing.T) {
	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"locks": []map[string]any{}})
	}))
	defer mockSrv.Close()

	sessionDir := setupTaskEnv(t, mockSrv.URL)
	origWd, _ := os.Getwd()
	os.Chdir(sessionDir)
	defer os.Chdir(origWd)

	if err := RunLockList("proj123"); err != nil {
		t.Fatal(err)
	}
}
