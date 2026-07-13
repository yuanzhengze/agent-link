package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestNetProjectCreate(t *testing.T) {
	var capturedMethod, capturedPath string
	var capturedBody struct {
		Name string `json:"name"`
	}

	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = r.Method
		capturedPath = r.URL.Path
		json.NewDecoder(r.Body).Decode(&capturedBody)
		r.Body.Close()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"id":          "proj123",
			"name":        "demo",
			"head_commit": "abc123",
		})
	}))
	defer mockSrv.Close()

	sessionDir := setupTaskEnv(t, mockSrv.URL)
	origWd, _ := os.Getwd()
	os.Chdir(sessionDir)
	defer os.Chdir(origWd)

	if err := RunProjectCreate("demo"); err != nil {
		t.Fatal(err)
	}
	if capturedMethod != "POST" {
		t.Errorf("expected POST, got %s", capturedMethod)
	}
	if capturedPath != "/projects" {
		t.Errorf("expected /projects, got %s", capturedPath)
	}
	if capturedBody.Name != "demo" {
		t.Errorf("expected name=demo, got %q", capturedBody.Name)
	}
}

func TestNetProjectCreate_error(t *testing.T) {
	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "internal error"})
	}))
	defer mockSrv.Close()

	sessionDir := setupTaskEnv(t, mockSrv.URL)
	origWd, _ := os.Getwd()
	os.Chdir(sessionDir)
	defer os.Chdir(origWd)

	err := RunProjectCreate("demo")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("expected 500 error, got: %s", err)
	}
}

func TestNetProjectList(t *testing.T) {
	var capturedMethod, capturedPath string

	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = r.Method
		capturedPath = r.URL.Path

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"projects": []map[string]string{
				{"id": "proj123", "name": "demo", "created_at": "2026-05-03T12:00:00Z", "head_commit": "abc123"},
				{"id": "proj456", "name": "other", "created_at": "2026-05-04T12:00:00Z", "head_commit": "def456"},
			},
		})
	}))
	defer mockSrv.Close()

	sessionDir := setupTaskEnv(t, mockSrv.URL)
	origWd, _ := os.Getwd()
	os.Chdir(sessionDir)
	defer os.Chdir(origWd)

	if err := RunProjectList(); err != nil {
		t.Fatal(err)
	}
	if capturedMethod != "GET" {
		t.Errorf("expected GET, got %s", capturedMethod)
	}
	if capturedPath != "/projects" {
		t.Errorf("expected /projects, got %s", capturedPath)
	}
}

func TestNetProjectList_empty(t *testing.T) {
	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"projects": []map[string]string{}})
	}))
	defer mockSrv.Close()

	sessionDir := setupTaskEnv(t, mockSrv.URL)
	origWd, _ := os.Getwd()
	os.Chdir(sessionDir)
	defer os.Chdir(origWd)

	if err := RunProjectList(); err != nil {
		t.Fatal(err)
	}
}
