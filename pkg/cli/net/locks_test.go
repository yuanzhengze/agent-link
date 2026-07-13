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
	var capturedMethod, capturedPath string
	var capturedBody struct {
		Project string `json:"project"`
		Session string `json:"session"`
		Path    string `json:"path"`
	}

	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = r.Method
		capturedPath = r.URL.Path
		json.NewDecoder(r.Body).Decode(&capturedBody)
		r.Body.Close()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"owner": "test-device:worker"})
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
	if capturedPath != "/locks/acquire" {
		t.Errorf("expected /locks/acquire, got %s", capturedPath)
	}
	if capturedBody.Project != "proj123" {
		t.Errorf("expected project=proj123, got %q", capturedBody.Project)
	}
	if capturedBody.Path != "index.html" {
		t.Errorf("expected path=index.html, got %q", capturedBody.Path)
	}
	// setupTaskEnv writes .agentlink.toml with session = "worker".
	if capturedBody.Session != "worker" {
		t.Errorf("expected session=worker, got %q", capturedBody.Session)
	}
}

func TestNetLockAcquire_conflict(t *testing.T) {
	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "file is locked by another session",
			"owner": "dev:other",
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

	// setupTaskEnv sets up a session dir; use a plain tempdir with no
	// .agentlink.toml instead so FindCurrentSession fails.
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	os.MkdirAll(homeDir+"/.agentlink", 0755)
	WriteConfigTOML(homeDir+"/.agentlink/config.toml", mockSrv.URL, "test-device", homeDir, "claude", false, nil)
	os.WriteFile(homeDir+"/.agentlink/credentials.json", []byte(`{"api_key":"sk_live_x"}`), 0600)

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
	var capturedMethod, capturedPath string
	var capturedBody struct {
		Project string `json:"project"`
		Session string `json:"session"`
		Path    string `json:"path"`
	}

	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = r.Method
		capturedPath = r.URL.Path
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
	if capturedPath != "/locks/release" {
		t.Errorf("expected /locks/release, got %s", capturedPath)
	}
	if capturedBody.Project != "proj123" || capturedBody.Path != "index.html" || capturedBody.Session != "worker" {
		t.Errorf("unexpected body: %+v", capturedBody)
	}
}

func TestNetLockRelease_conflict(t *testing.T) {
	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "file is locked by another session",
			"owner": "dev:other",
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
		capturedProject = r.URL.Query().Get("project")

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"locks": []map[string]any{
				{"path": "index.html", "owner": "test-device:worker", "acquired_at": int64(1000), "lease_expires_at": int64(1120)},
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
	if capturedPath != "/locks/list" {
		t.Errorf("expected /locks/list, got %s", capturedPath)
	}
	if capturedProject != "proj123" {
		t.Errorf("expected project=proj123, got %q", capturedProject)
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
