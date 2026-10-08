package rt

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/team/agentlink/pkg/adapter"
	api "github.com/team/agentlink/pkg/cli/net"
)

func TestCheckPrereqs(t *testing.T) {
	l := adapter.NewLauncher("claude")

	t.Run("both available in PATH", func(t *testing.T) {
		err := l.CheckPrereqs()
		if err != nil {
			t.Logf("prereqs check: %v (may be expected in some environments)", err)
		}
	})

	t.Run("empty PATH", func(t *testing.T) {
		t.Setenv("PATH", "")
		err := l.CheckPrereqs()
		if err == nil {
			t.Fatal("expected error with empty PATH")
		}
		if !strings.Contains(err.Error(), "to be installed") {
			t.Errorf("error should mention to be installed, got: %s", err)
		}
	})

	t.Run("partial PATH with tmux only", func(t *testing.T) {
		bin := t.TempDir()
		realTmux, err := exec.LookPath("tmux")
		if err != nil {
			t.Skip("tmux unavailable")
		}
		if err := os.Symlink(realTmux, filepath.Join(bin, "tmux")); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", bin)
		err = l.CheckPrereqs()
		if err == nil {
			t.Fatal("expected error when claude is missing")
		}
		if !strings.Contains(err.Error(), "claude") {
			t.Errorf("error should mention claude, got: %s", err)
		}
		if strings.Contains(err.Error(), "tmux") {
			t.Errorf("error should not mention tmux when it's available, got: %s", err)
		}
	})
}

func TestWriteConfigTOML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	err := api.WriteConfigTOML(path, "http://server:8080", "my-device", "/tmp/agent_team", "claude", false, nil)
	if err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	content := string(data)
	if !strings.Contains(content, `server = "http://server:8080"`) {
		t.Errorf("missing server, got: %s", content)
	}
	if !strings.Contains(content, `device = "my-device"`) {
		t.Errorf("missing device, got: %s", content)
	}
}

func TestWriteSessionTOML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".agentlink.toml")

	err := api.WriteSessionTOML(path, "worker", "my-device")
	if err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	content := string(data)
	if !strings.Contains(content, `session = "worker"`) {
		t.Errorf("missing session, got: %s", content)
	}
	if !strings.Contains(content, `device = "my-device"`) {
		t.Errorf("missing device, got: %s", content)
	}
}

func TestWriteSessionTOMLFileMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".agentlink.toml")

	api.WriteSessionTOML(path, "main", "dev")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("expected 0600, got %o", info.Mode().Perm())
	}
}

// seedLogin writes a v2 config+credentials pair for a logged-in user with an
// active team, so init can run without any registration step.
func seedLogin(t *testing.T, server string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".agentlink"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := api.AgentConfig{
		Server:      server,
		UserID:      "u_1",
		Username:    "kirby",
		DeviceID:    "d_1",
		Device:      "laptop",
		CurrentTeam: "tm_1",
	}
	if err := api.WriteAccountConfig(api.ConfigFilePath(), cfg); err != nil {
		t.Fatal(err)
	}
	if err := api.WriteCredentials(api.CredentialsFilePath(), api.AgentCredentials{DeviceSession: "ds_x"}); err != nil {
		t.Fatal(err)
	}
	return home
}

// fakePrereqs puts dummy tmux/claude executables on PATH so CheckPrereqs (a
// LookPath existence check) passes without the real binaries.
func fakePrereqs(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	for _, name := range []string{"tmux", "claude"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
}

// stubLaunch replaces the tmux session launcher with a no-op that returns fake
// session ids, so init tests never spawn real tmux/claude.
func stubLaunch(t *testing.T) {
	t.Helper()
	orig := launchSessionsFn
	launchSessionsFn = func(baseDir, agent string, opts launchOpts) (map[string]string, error) {
		return map[string]string{"main": "sid-main", "worker": "sid-worker"}, nil
	}
	t.Cleanup(func() { launchSessionsFn = orig })
}

func TestRunInitRequiresExistingLogin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	fakePrereqs(t)
	stubLaunch(t)

	err := RunInit(&InitOptions{Path: filepath.Join(home, "team"), Agent: "claude"})
	if err == nil {
		t.Fatal("expected error when not logged in")
	}
	if !strings.Contains(err.Error(), "login") {
		t.Errorf("error should tell the user to login, got: %s", err)
	}
}

func TestRunInitMakesNoRegisterRequest(t *testing.T) {
	// This is a pure local operation: assert nothing is ever sent to the server
	// by pointing at an address that would fail loudly if dialed.
	home := seedLogin(t, "http://127.0.0.1:0")
	fakePrereqs(t)
	stubLaunch(t)

	if err := RunInit(&InitOptions{Path: filepath.Join(home, "team"), Agent: "claude"}); err != nil {
		t.Fatalf("local init should succeed: %v", err)
	}
}

func TestRunInitPreservesAccountAndCurrentTeam(t *testing.T) {
	home := seedLogin(t, "http://unused")
	fakePrereqs(t)
	stubLaunch(t)

	workDir := filepath.Join(home, "team")
	if err := RunInit(&InitOptions{Path: workDir, Agent: "claude"}); err != nil {
		t.Fatal(err)
	}

	cfg, err := api.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UserID != "u_1" || cfg.Username != "kirby" || cfg.DeviceID != "d_1" || cfg.CurrentTeam != "tm_1" {
		t.Errorf("account/team not preserved: %+v", cfg)
	}
	absWork, _ := filepath.Abs(workDir)
	if cfg.BaseDir != absWork {
		t.Errorf("base_dir = %q; want %q", cfg.BaseDir, absWork)
	}
	// The device credential must be untouched by init.
	creds, err := api.LoadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if creds.DeviceSession != "ds_x" {
		t.Errorf("device_session changed by init: %q", creds.DeviceSession)
	}
}

func TestRunInitWritesSessionMarkersAndCLAUDEMD(t *testing.T) {
	home := seedLogin(t, "http://unused")
	fakePrereqs(t)
	stubLaunch(t)

	workDir := filepath.Join(home, "team")
	if err := RunInit(&InitOptions{Path: workDir, Agent: "claude"}); err != nil {
		t.Fatal(err)
	}

	for _, session := range []string{"main", "worker"} {
		tomlPath := filepath.Join(workDir, session, ".agentlink.toml")
		data, err := os.ReadFile(tomlPath)
		if err != nil {
			t.Fatalf("missing %s: %v", tomlPath, err)
		}
		if !strings.Contains(string(data), `session = "`+session+`"`) {
			t.Errorf("%s missing session marker: %s", tomlPath, data)
		}
		if !strings.Contains(string(data), `device = "laptop"`) {
			t.Errorf("%s missing device marker: %s", tomlPath, data)
		}
		claudePath := filepath.Join(workDir, session, "CLAUDE.md")
		cd, err := os.ReadFile(claudePath)
		if err != nil {
			t.Fatalf("missing %s: %v", claudePath, err)
		}
		if len(cd) == 0 {
			t.Errorf("%s is empty", claudePath)
		}
	}
}

func TestRunInitExistingDirWithoutForce(t *testing.T) {
	home := seedLogin(t, "http://unused")
	fakePrereqs(t)
	stubLaunch(t)

	workDir := filepath.Join(home, "already-there")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	err := RunInit(&InitOptions{Path: workDir, Agent: "claude"})
	if err == nil {
		t.Fatal("expected error for existing directory")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("expected 'already exists' error, got: %s", err)
	}
}

func TestPollConfigParsing(t *testing.T) {
	base := `server = "http://srv:8080"
device_id = "d_1"
device = "dev"
base_dir = "/tmp/agent_team"
agent = "claude"
`

	t.Run("poll enabled true", func(t *testing.T) {
		homeDir := t.TempDir()
		t.Setenv("HOME", homeDir)
		agentlinkDir := filepath.Join(homeDir, ".agentlink")
		os.MkdirAll(agentlinkDir, 0755)
		config := base + "\n[poll]\nenabled = true\ninterval = 10\n"
		os.WriteFile(filepath.Join(agentlinkDir, "config.toml"), []byte(config), 0600)

		cfg, err := api.LoadConfig()
		if err != nil {
			t.Fatal(err)
		}
		if !cfg.Poll.Enabled {
			t.Error("expected Poll.Enabled=true")
		}
		if cfg.Poll.Interval != 10 {
			t.Errorf("expected Poll.Interval=10, got %d", cfg.Poll.Interval)
		}
	})

	t.Run("poll enabled false", func(t *testing.T) {
		homeDir := t.TempDir()
		t.Setenv("HOME", homeDir)
		agentlinkDir := filepath.Join(homeDir, ".agentlink")
		os.MkdirAll(agentlinkDir, 0755)
		config := base + "\n[poll]\nenabled = false\ninterval = 5\n"
		os.WriteFile(filepath.Join(agentlinkDir, "config.toml"), []byte(config), 0600)

		cfg, err := api.LoadConfig()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Poll.Enabled {
			t.Error("expected Poll.Enabled=false")
		}
	})

	t.Run("poll section missing defaults to enabled", func(t *testing.T) {
		homeDir := t.TempDir()
		t.Setenv("HOME", homeDir)
		agentlinkDir := filepath.Join(homeDir, ".agentlink")
		os.MkdirAll(agentlinkDir, 0755)
		os.WriteFile(filepath.Join(agentlinkDir, "config.toml"), []byte(base), 0600)

		cfg, err := api.LoadConfig()
		if err != nil {
			t.Fatal(err)
		}
		if !cfg.Poll.Enabled {
			t.Error("expected Poll.Enabled=true by default")
		}
		if cfg.Poll.Interval != 5 {
			t.Errorf("expected Poll.Interval=5 (default), got %d", cfg.Poll.Interval)
		}
	})
}

func TestRunPoll_disabled(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	agentlinkDir := filepath.Join(homeDir, ".agentlink")
	os.MkdirAll(agentlinkDir, 0755)
	config := `server = "http://srv:8080"
device_id = "d_1"
device = "dev"
base_dir = "` + filepath.Join(homeDir, "agent_team") + `"
agent = "claude"

[poll]
enabled = false
`
	os.WriteFile(filepath.Join(agentlinkDir, "config.toml"), []byte(config), 0600)
	api.WriteCredentials(filepath.Join(agentlinkDir, "credentials.json"), api.AgentCredentials{DeviceSession: "ds_x"})

	sessionDir := filepath.Join(homeDir, "agent_team", "worker")
	os.MkdirAll(sessionDir, 0755)
	api.WriteSessionTOML(filepath.Join(sessionDir, ".agentlink.toml"), "worker", "dev")

	origWd, _ := os.Getwd()
	os.Chdir(sessionDir)
	defer os.Chdir(origWd)

	if err := RunPoll(); err != nil {
		t.Fatal(err)
	}
}
