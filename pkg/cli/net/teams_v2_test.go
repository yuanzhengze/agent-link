package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// seedLoggedInAccount writes a config+credentials pair pointing at server with
// an optional current team, and routes team output to buf.
func seedLoggedInAccount(t *testing.T, server, currentTeam string, buf *bytes.Buffer) {
	t.Helper()
	accountHome(t)
	cfg := AgentConfig{
		Server:      server,
		UserID:      "u_1",
		Username:    "kirby",
		DeviceID:    "d_1",
		Device:      "laptop",
		CurrentTeam: currentTeam,
	}
	if err := WriteAccountConfig(ConfigFilePath(), cfg); err != nil {
		t.Fatal(err)
	}
	if err := WriteCredentials(CredentialsFilePath(), AgentCredentials{DeviceSession: "ds_x"}); err != nil {
		t.Fatal(err)
	}
	orig := teamOut
	teamOut = buf
	t.Cleanup(func() { teamOut = orig })
}

func currentTeamFromConfig(t *testing.T) string {
	t.Helper()
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	return cfg.CurrentTeam
}

func TestRunTeamListParsesMemberships(t *testing.T) {
	var buf bytes.Buffer
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/teams" || r.Method != http.MethodGet {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Device ds_x" {
			t.Errorf("Authorization = %q; want Device ds_x", got)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"teams": []any{
				map[string]any{"id": "tm_1", "name": "Product", "role": "owner"},
				map[string]any{"id": "tm_2", "name": "Research", "role": "member"},
			},
		})
	}))
	defer srv.Close()

	seedLoggedInAccount(t, srv.URL, "tm_1", &buf)
	if err := RunTeamList(); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !bytes.Contains([]byte(out), []byte("tm_1")) || !bytes.Contains([]byte(out), []byte("Product")) {
		t.Fatalf("list output missing team: %q", out)
	}
	if !bytes.Contains([]byte(out), []byte("Research")) {
		t.Fatalf("list output missing second team: %q", out)
	}
}

func TestRunTeamCreatePrintsInviteOnce(t *testing.T) {
	var buf bytes.Buffer
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/teams" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{
			"team":        map[string]any{"id": "tm_new", "name": "Product", "role": "owner"},
			"invite_code": "inv_secret_123",
		})
	}))
	defer srv.Close()

	seedLoggedInAccount(t, srv.URL, "", &buf)
	if err := RunTeamCreate("Product"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("inv_secret_123")) {
		t.Fatalf("invite code not printed: %q", buf.String())
	}
	if got := currentTeamFromConfig(t); got != "tm_new" {
		t.Fatalf("current_team = %q; want tm_new", got)
	}
	// The invite code must never be persisted to disk.
	data, _ := os.ReadFile(ConfigFilePath())
	if bytes.Contains(data, []byte("inv_secret_123")) {
		t.Fatalf("invite code persisted to config: %s", data)
	}
}

func TestRunTeamJoinDoesNotPersistInvite(t *testing.T) {
	var buf bytes.Buffer
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/teams/join" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["invite_code"] != "inv_join_9" || body["team_id"] != "tm_join" {
			t.Errorf("join body = %+v", body)
		}
		json.NewEncoder(w).Encode(map[string]any{"id": "tm_join", "name": "Joined", "role": "member"})
	}))
	defer srv.Close()

	seedLoggedInAccount(t, srv.URL, "", &buf)
	if err := RunTeamJoin("tm_join", "inv_join_9"); err != nil {
		t.Fatal(err)
	}
	if got := currentTeamFromConfig(t); got != "tm_join" {
		t.Fatalf("current_team = %q; want tm_join", got)
	}
	data, _ := os.ReadFile(ConfigFilePath())
	if bytes.Contains(data, []byte("inv_join_9")) {
		t.Fatalf("invite code persisted to config: %s", data)
	}
	credData, _ := os.ReadFile(CredentialsFilePath())
	if bytes.Contains(credData, []byte("inv_join_9")) {
		t.Fatalf("invite code persisted to credentials: %s", credData)
	}
}

func TestRunTeamUsePersistsSelectedMembership(t *testing.T) {
	var buf bytes.Buffer
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/teams" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"teams": []any{
				map[string]any{"id": "tm_a", "name": "Alpha", "role": "member"},
				map[string]any{"id": "tm_b", "name": "Beta", "role": "admin"},
			},
		})
	}))
	defer srv.Close()

	seedLoggedInAccount(t, srv.URL, "tm_a", &buf)
	if err := RunTeamUse("tm_b"); err != nil {
		t.Fatal(err)
	}
	if got := currentTeamFromConfig(t); got != "tm_b" {
		t.Fatalf("current_team = %q; want tm_b", got)
	}

	// Using a team the user does not belong to must fail and not change state.
	if err := RunTeamUse("tm_ghost"); err == nil {
		t.Fatal("expected error using non-member team")
	}
	if got := currentTeamFromConfig(t); got != "tm_b" {
		t.Fatalf("current_team changed on failure = %q; want tm_b", got)
	}
}

func TestRunTeamMembersUsesCurrentTeam(t *testing.T) {
	var buf bytes.Buffer
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/teams/tm_cur/members" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"members": []any{
				map[string]any{"user_id": "u_1", "username": "kirby", "role": "owner"},
			},
		})
	}))
	defer srv.Close()

	seedLoggedInAccount(t, srv.URL, "tm_cur", &buf)
	if err := RunTeamMembers(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("kirby")) {
		t.Fatalf("members output missing user: %q", buf.String())
	}
}

func TestRunTeamLeaveClearsCurrentTeam(t *testing.T) {
	var buf bytes.Buffer
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/teams/tm_leave/leave" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	seedLoggedInAccount(t, srv.URL, "tm_leave", &buf)
	if err := RunTeamLeave(); err != nil {
		t.Fatal(err)
	}
	if got := currentTeamFromConfig(t); got != "" {
		t.Fatalf("current_team = %q; want empty after leave", got)
	}
}
