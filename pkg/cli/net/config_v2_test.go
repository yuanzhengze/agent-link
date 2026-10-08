package api

import (
	"os"
	"path/filepath"
	"testing"
)

// configPathV2 sets HOME to a temp dir and returns the config.toml path the
// account loaders will read.
func configPathV2(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".agentlink")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "config.toml")
}

func sampleAccountConfig() AgentConfig {
	return AgentConfig{
		Server:   "https://cowork.example",
		UserID:   "u_abc",
		Username: "kirby",
		DeviceID: "d_xyz",
		Device:   "laptop",
		BaseDir:  "/tmp/base",
		Agent:    "claude",
		Poll:     PollConfig{Enabled: false, Interval: 9},
		Sessions: map[string]string{"main": "s_1"},
	}
}

func TestLoadConfigV2AllowsLoginWithoutCurrentTeam(t *testing.T) {
	path := configPathV2(t)
	cfg := sampleAccountConfig()
	cfg.CurrentTeam = "" // a freshly-registered user has no team yet
	if err := WriteAccountConfig(path, cfg); err != nil {
		t.Fatal(err)
	}

	got, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig with empty current_team must succeed: %v", err)
	}
	if got.CurrentTeam != "" {
		t.Errorf("current_team = %q; want empty", got.CurrentTeam)
	}
	if got.Server != cfg.Server || got.UserID != cfg.UserID || got.Username != cfg.Username ||
		got.DeviceID != cfg.DeviceID || got.Device != cfg.Device {
		t.Errorf("account fields not round-tripped: %+v", got)
	}
}

func TestWriteCredentialsUses0600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	if err := WriteCredentials(path, AgentCredentials{DeviceSession: "ds_secret"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("credentials mode = %o; want 0600", info.Mode().Perm())
	}

	creds, err := LoadCredentialsAt(path)
	if err != nil {
		t.Fatal(err)
	}
	if creds.DeviceSession != "ds_secret" {
		t.Errorf("device_session = %q; want ds_secret", creds.DeviceSession)
	}
}

func TestCredentialsRejectLegacyAPIKeyShape(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".agentlink")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A legacy credentials.json that only carries an api_key must be rejected:
	// the v2 client authenticates exclusively with a device_session.
	if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(`{"api_key":"sk_live_x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCredentials(); err == nil {
		t.Fatal("LoadCredentials must reject a legacy api_key-only file")
	}
}

func TestSetCurrentTeamPreservesRuntimeConfig(t *testing.T) {
	path := configPathV2(t)
	cfg := sampleAccountConfig()
	if err := WriteAccountConfig(path, cfg); err != nil {
		t.Fatal(err)
	}

	if err := SetCurrentTeam(path, "tm_abc"); err != nil {
		t.Fatal(err)
	}

	got, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got.CurrentTeam != "tm_abc" {
		t.Errorf("current_team = %q; want tm_abc", got.CurrentTeam)
	}
	if got.Poll.Enabled != false || got.Poll.Interval != 9 {
		t.Errorf("poll not preserved: %+v", got.Poll)
	}
	if got.Sessions["main"] != "s_1" {
		t.Errorf("sessions not preserved: %+v", got.Sessions)
	}
	if got.Server != cfg.Server || got.UserID != cfg.UserID || got.Device != cfg.Device ||
		got.Username != cfg.Username || got.DeviceID != cfg.DeviceID || got.BaseDir != cfg.BaseDir ||
		got.Agent != cfg.Agent {
		t.Errorf("account/runtime fields not preserved: %+v", got)
	}
}
