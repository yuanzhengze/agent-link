package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// scriptedPasswords returns a password reader that yields the given passwords in
// order and a pointer to the number of times it was called.
func scriptedPasswords(passwords ...string) (func() ([]byte, error), *int) {
	i := 0
	count := 0
	f := func() ([]byte, error) {
		count++
		if i >= len(passwords) {
			return nil, errors.New("no more scripted passwords")
		}
		p := passwords[i]
		i++
		return []byte(p), nil
	}
	return f, &count
}

func accountHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".agentlink"), 0o755); err != nil {
		t.Fatal(err)
	}
	return home
}

func readCredsFile(t *testing.T) AgentCredentials {
	t.Helper()
	data, err := os.ReadFile(CredentialsFilePath())
	if err != nil {
		t.Fatalf("read credentials: %v", err)
	}
	var creds AgentCredentials
	if err := json.Unmarshal(data, &creds); err != nil {
		t.Fatalf("decode credentials: %v", err)
	}
	return creds
}

func TestRunRegisterReadsPasswordTwiceAndNeverPrintsIt(t *testing.T) {
	accountHome(t)
	const password = "correct horse 123"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/register":
			json.NewEncoder(w).Encode(map[string]any{
				"user": map[string]any{"id": "u_1", "username": "kirby", "must_change_password": false},
			})
		case "/api/auth/device-login":
			json.NewEncoder(w).Encode(map[string]any{
				"user":              map[string]any{"id": "u_1", "username": "kirby"},
				"device_id":         "d_1",
				"device_credential": "ds_new",
			})
		case "/api/teams":
			json.NewEncoder(w).Encode(map[string]any{"teams": []any{}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	pwFn, count := scriptedPasswords(password, password)
	var out bytes.Buffer
	if err := RunRegister(srv.URL, "kirby", "laptop", AccountIO{Out: &out, Password: pwFn}); err != nil {
		t.Fatal(err)
	}

	if *count != 2 {
		t.Fatalf("password read %d times; want 2", *count)
	}
	if strings.Contains(out.String(), password) {
		t.Fatalf("password leaked to output: %q", out.String())
	}
	if got := readCredsFile(t); got.DeviceSession != "ds_new" {
		t.Fatalf("device_session = %q; want ds_new", got.DeviceSession)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Username != "kirby" || cfg.DeviceID != "d_1" || cfg.UserID != "u_1" {
		t.Fatalf("config not saved: %+v", cfg)
	}
}

func TestRunLoginWritesDeviceCredentialAndSelectsTeam(t *testing.T) {
	accountHome(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/device-login":
			json.NewEncoder(w).Encode(map[string]any{
				"user":              map[string]any{"id": "u_1", "username": "kirby"},
				"device_id":         "d_1",
				"device_credential": "ds_login",
			})
		case "/api/teams":
			json.NewEncoder(w).Encode(map[string]any{
				"teams": []any{map[string]any{"id": "tm_1", "name": "Product", "role": "member"}},
			})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	pwFn, count := scriptedPasswords("correct horse 123")
	if err := RunLogin(srv.URL, "kirby", "laptop", AccountIO{Out: &bytes.Buffer{}, Password: pwFn}); err != nil {
		t.Fatal(err)
	}
	if *count != 1 {
		t.Fatalf("password read %d times; want 1", *count)
	}
	if got := readCredsFile(t); got.DeviceSession != "ds_login" {
		t.Fatalf("device_session = %q; want ds_login", got.DeviceSession)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CurrentTeam != "tm_1" {
		t.Fatalf("current_team = %q; want tm_1", cfg.CurrentTeam)
	}
}

func TestRunLoginCompletesRequiredPasswordChangeBeforeSavingCredential(t *testing.T) {
	accountHome(t)

	deviceLoginCalls := 0
	var changePwAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/device-login":
			deviceLoginCalls++
			if deviceLoginCalls == 1 {
				json.NewEncoder(w).Encode(map[string]any{
					"user":              map[string]any{"id": "u_1", "username": "kirby", "must_change_password": true},
					"device_id":         "d_1",
					"device_credential": "ds_restricted",
				})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"user":              map[string]any{"id": "u_1", "username": "kirby", "must_change_password": false},
				"device_id":         "d_1",
				"device_credential": "ds_final",
			})
		case "/api/auth/change-password":
			changePwAuth = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusNoContent)
		case "/api/teams":
			json.NewEncoder(w).Encode(map[string]any{"teams": []any{}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	// login password, then new password + confirm.
	pwFn, count := scriptedPasswords("old-pass-123", "new-pass-456", "new-pass-456")
	if err := RunLogin(srv.URL, "kirby", "laptop", AccountIO{Out: &bytes.Buffer{}, Password: pwFn}); err != nil {
		t.Fatal(err)
	}
	if *count != 3 {
		t.Fatalf("password read %d times; want 3", *count)
	}
	if changePwAuth != "Device ds_restricted" {
		t.Fatalf("change-password auth = %q; want Device ds_restricted", changePwAuth)
	}
	if got := readCredsFile(t); got.DeviceSession != "ds_final" {
		t.Fatalf("device_session = %q; want ds_final (post-change)", got.DeviceSession)
	}
}

func TestRunLogoutRevokesDeviceAndDeletesLocalCredential(t *testing.T) {
	accountHome(t)

	var logoutAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/auth/device-logout" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		logoutAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	// Seed a logged-in account.
	cfg := AgentConfig{Server: srv.URL, UserID: "u_1", Username: "kirby", DeviceID: "d_1", Device: "laptop"}
	if err := WriteAccountConfig(ConfigFilePath(), cfg); err != nil {
		t.Fatal(err)
	}
	if err := WriteCredentials(CredentialsFilePath(), AgentCredentials{DeviceSession: "ds_x"}); err != nil {
		t.Fatal(err)
	}

	if err := RunLogout(AccountIO{Out: &bytes.Buffer{}}); err != nil {
		t.Fatal(err)
	}
	if logoutAuth != "Device ds_x" {
		t.Fatalf("logout auth = %q; want Device ds_x", logoutAuth)
	}
	if _, err := os.Stat(CredentialsFilePath()); !os.IsNotExist(err) {
		t.Fatalf("credentials file should be deleted, stat err = %v", err)
	}
}

func TestAPIDoUsesDeviceAuthorization(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &AgentConfig{Server: srv.URL}
	creds := &AgentCredentials{DeviceSession: "ds_x"}
	resp, err := APIDo(cfg, creds, http.MethodGet, "/api/teams", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotAuth != "Device ds_x" {
		t.Fatalf("Authorization = %q; want Device ds_x", gotAuth)
	}
}

func TestAPIDoWithSessionAddsAgentSessionHeader(t *testing.T) {
	var gotSession string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSession = r.Header.Get("X-Agentlink-Session")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &AgentConfig{Server: srv.URL}
	creds := &AgentCredentials{DeviceSession: "ds_x"}
	resp, err := APIDoWithSession(cfg, creds, "main", http.MethodPost, "/api/teams/tm_1/locks/acquire", map[string]string{"project_id": "p", "path": "index.html"})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotSession != "main" {
		t.Fatalf("X-Agentlink-Session = %q; want main", gotSession)
	}
}
