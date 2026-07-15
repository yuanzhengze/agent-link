package rt

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	if !strings.Contains(content, `base_dir = "/tmp/agent_team"`) {
		t.Errorf("missing base_dir, got: %s", content)
	}
	if !strings.Contains(content, `agent = "claude"`) {
		t.Errorf("missing agent, got: %s", content)
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

func TestRunInitE2E(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude not in PATH, skipping")
	}
	// Mock register API
	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/agents/register" {
			t.Errorf("expected /agents/register, got %s", r.URL.Path)
		}

		var req registerRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		r.Body.Close()

		if req.Device != "test-device" {
			t.Errorf("request device: expected %q, got %q", "test-device", req.Device)
		}
		if len(req.Sessions) != 2 || req.Sessions[0] != "main" || req.Sessions[1] != "worker" {
			t.Errorf("request sessions: expected [main worker], got %v", req.Sessions)
		}
		if req.RegisterPassword != "test-pw" {
			t.Errorf("request password: expected %q, got %q", "test-pw", req.RegisterPassword)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(registerResponse{
			APIKey:       "sk_live_" + strings.Repeat("a", 64),
			Device:       "test-device",
			Sessions:     []string{"main", "worker"},
			RegisteredAt: "2026-05-03T12:00:00Z",
		})
	}))
	defer mockSrv.Close()

	// Isolated home directory
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	workDir := filepath.Join(homeDir, "my-team")

	opts := &InitOptions{
		Server:   mockSrv.URL,
		Password: "test-pw",
		Device:   "test-device",
		Path:     workDir,
	}

	if err := RunInit(opts); err != nil {
		t.Fatal(err)
	}

	// === Verify directory structure ===
	for _, sub := range []string{"main", "worker"} {
		info, err := os.Stat(filepath.Join(workDir, sub))
		if err != nil {
			t.Errorf("missing directory %s: %v", sub, err)
		} else if !info.IsDir() {
			t.Errorf("%s is not a directory", sub)
		}
	}

	// === Verify config.toml ===
	configPath := filepath.Join(homeDir, ".agentlink", "config.toml")
	configData, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	configContent := string(configData)
	if !strings.Contains(configContent, `server = "`+mockSrv.URL+`"`) {
		t.Errorf("config.toml missing server, got:\n%s", configContent)
	}
	if !strings.Contains(configContent, `device = "test-device"`) {
		t.Errorf("config.toml missing device, got:\n%s", configContent)
	}
	if !strings.Contains(configContent, `base_dir = "`+workDir+`"`) {
		t.Errorf("config.toml missing base_dir %q, got:\n%s", workDir, configContent)
	}

	// === Verify credentials.json ===
	credPath := filepath.Join(homeDir, ".agentlink", "credentials.json")
	credData, err := os.ReadFile(credPath)
	if err != nil {
		t.Fatal(err)
	}
	var creds map[string]string
	if err := json.Unmarshal(credData, &creds); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(creds["api_key"], "sk_live_") {
		t.Errorf("api_key should start with sk_live_, got %q", creds["api_key"])
	}
	if len(creds["api_key"]) != len("sk_live_")+64 {
		t.Errorf("api_key length: expected %d, got %d", len("sk_live_")+64, len(creds["api_key"]))
	}
	if creds["registered_at"] != "2026-05-03T12:00:00Z" {
		t.Errorf("registered_at: expected %q, got %q", "2026-05-03T12:00:00Z", creds["registered_at"])
	}

	// === Verify main/.agentlink.toml ===
	mainToml := filepath.Join(workDir, "main", ".agentlink.toml")
	mainData, err := os.ReadFile(mainToml)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mainData), `session = "main"`) {
		t.Errorf("main/.agentlink.toml missing session=main, got:\n%s", mainData)
	}
	if !strings.Contains(string(mainData), `device = "test-device"`) {
		t.Errorf("main/.agentlink.toml missing device=test-device, got:\n%s", mainData)
	}

	// === Verify worker/.agentlink.toml ===
	workerToml := filepath.Join(workDir, "worker", ".agentlink.toml")
	workerData, err := os.ReadFile(workerToml)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(workerData), `session = "worker"`) {
		t.Errorf("worker/.agentlink.toml missing session=worker, got:\n%s", workerData)
	}
	if !strings.Contains(string(workerData), `device = "test-device"`) {
		t.Errorf("worker/.agentlink.toml missing device=test-device, got:\n%s", workerData)
	}
}

func TestRunInitE2E_existingDir(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude not in PATH, skipping")
	}
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	workDir := filepath.Join(homeDir, "already-there")
	os.MkdirAll(workDir, 0755)

	opts := &InitOptions{
		Server:   "http://localhost:1",
		Password: "test-pw",
		Device:   "test-device",
		Path:     workDir,
	}

	err := RunInit(opts)
	if err == nil {
		t.Fatal("expected error for existing directory")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("expected 'already exists' error, got: %s", err)
	}
}
func TestPollConfigParsing(t *testing.T) {
	t.Run("poll enabled true", func(t *testing.T) {
		homeDir := t.TempDir()
		t.Setenv("HOME", homeDir)

		agentlinkDir := filepath.Join(homeDir, ".agentlink")
		os.MkdirAll(agentlinkDir, 0755)
		config := `server = "http://srv:8080"
device = "dev"
base_dir = "/tmp/agent_team"
agent = "claude"

[poll]
enabled = true
interval = 10
`
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
		config := `server = "http://srv:8080"
device = "dev"
base_dir = "/tmp/agent_team"
agent = "claude"

[poll]
enabled = false
interval = 5
`
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
		api.WriteConfigTOML(filepath.Join(agentlinkDir, "config.toml"), "http://srv:8080", "dev", "/tmp/agent_team", "claude", false, nil)

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

	t.Run("poll interval from config", func(t *testing.T) {
		homeDir := t.TempDir()
		t.Setenv("HOME", homeDir)

		agentlinkDir := filepath.Join(homeDir, ".agentlink")
		os.MkdirAll(agentlinkDir, 0755)
		config := `server = "http://srv:8080"
device = "dev"
base_dir = "/tmp/agent_team"
agent = "claude"

[poll]
enabled = true
interval = 30
`
		os.WriteFile(filepath.Join(agentlinkDir, "config.toml"), []byte(config), 0600)

		cfg, err := api.LoadConfig()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Poll.Interval != 30 {
			t.Errorf("expected Poll.Interval=30, got %d", cfg.Poll.Interval)
		}
	})
}

func TestRunPoll_disabled(t *testing.T) {
	t.Run("poll disabled exits clean", func(t *testing.T) {
		homeDir := t.TempDir()
		t.Setenv("HOME", homeDir)

		agentlinkDir := filepath.Join(homeDir, ".agentlink")
		os.MkdirAll(agentlinkDir, 0755)
		config := `server = "http://srv:8080"
device = "dev"
base_dir = "` + filepath.Join(homeDir, "agent_team") + `"
agent = "claude"

[poll]
enabled = false
`
		os.WriteFile(filepath.Join(agentlinkDir, "config.toml"), []byte(config), 0600)

		creds := map[string]string{"api_key": "sk_live_test"}
		credData, _ := json.MarshalIndent(creds, "", "  ")
		os.WriteFile(filepath.Join(agentlinkDir, "credentials.json"), credData, 0600)

		sessionDir := filepath.Join(homeDir, "agent_team", "worker")
		os.MkdirAll(sessionDir, 0755)
		api.WriteSessionTOML(filepath.Join(sessionDir, ".agentlink.toml"), "worker", "dev")

		origWd, _ := os.Getwd()
		os.Chdir(sessionDir)
		defer os.Chdir(origWd)

		err := RunPoll()
		if err != nil {
			t.Fatal(err)
		}
	})
}
