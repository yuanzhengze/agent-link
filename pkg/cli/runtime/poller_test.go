package rt

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	api "github.com/team/agentlink/pkg/cli/net"
)

// mockIdleDetector implements adapter.IdleDetector for testing.
type mockIdleDetector struct {
	busy        bool
	promptEmpty bool
}

func (m *mockIdleDetector) IsBusy(_ string) bool        { return m.busy }
func (m *mockIdleDetector) IsPromptEmpty(_ string) bool { return m.promptEmpty }

// ---------------------------------------------------------------------------
// Layer 2 — Poller logic with mocked deps
// ---------------------------------------------------------------------------

func TestPoller_injectsWhenIdle(t *testing.T) {
	msgContent := "task dispatch: write tests"

	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(pollerPullResponse{
			Items: []pollerInboxItem{
				{ID: "m1", Type: "msg", Content: msgContent, FromDevice: "dev-a", FromSession: "main"},
			},
		})
	}))
	defer mockSrv.Close()

	var injected string
	captureCalls := 0
	ctx, cancel := context.WithCancel(context.Background())

	p := &Poller{
		Session:       "worker",
		Server:        mockSrv.URL,
		TeamID:        "tm_x",
		DeviceSession: "ds_test",
		Interval:      10 * time.Millisecond,
		Ctx:           ctx,
		Stdout:        io.Discard,
		IdleDetector:  &mockIdleDetector{busy: false, promptEmpty: true},
		capturePane: func(string) (string, error) {
			captureCalls++
			if captureCalls >= 3 {
				cancel()
			}
			return "❯\n", nil // idle
		},
		sendKeys: func(_ string, text string) error {
			injected = text
			return nil
		},
		httpDo: http.DefaultClient.Do,
	}

	p.Run()
	expected := "[来自 dev-a:main 的消息] " + msgContent
	if injected != expected {
		t.Errorf("expected injected=%q, got %q", expected, injected)
	}
}

func TestPoller_injectsTaskWithGuidance(t *testing.T) {
	taskContent := "查 prod 为什么 500"
	taskID := "fix-001"

	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(pollerPullResponse{
			Items: []pollerInboxItem{
				{ID: "t1", Type: "task", TaskID: taskID, Content: taskContent, FromDevice: "dev-a", FromSession: "main"},
			},
		})
	}))
	defer mockSrv.Close()

	var injected string
	captureCalls := 0
	ctx, cancel := context.WithCancel(context.Background())

	p := &Poller{
		Session:       "worker",
		Server:        mockSrv.URL,
		TeamID:        "tm_x",
		DeviceSession: "ds_test",
		Interval:      10 * time.Millisecond,
		Ctx:           ctx,
		Stdout:        io.Discard,
		IdleDetector:  &mockIdleDetector{busy: false, promptEmpty: true},
		capturePane: func(string) (string, error) {
			captureCalls++
			if captureCalls >= 3 {
				cancel()
			}
			return "❯\n", nil
		},
		sendKeys: func(_ string, text string) error {
			injected = text
			return nil
		},
		httpDo: http.DefaultClient.Do,
	}

	p.Run()

	// Check prefix
	if !strings.Contains(injected, "[来自 dev-a:main 的任务 fix-001]") {
		t.Errorf("injected missing task prefix, got: %s", injected)
	}
	// Check content preserved
	if !strings.Contains(injected, taskContent) {
		t.Errorf("injected missing task content, got: %s", injected)
	}
	// Check completed guidance
	if !strings.Contains(injected, "agentlink task result fix-001 completed") {
		t.Errorf("injected missing completed guidance, got: %s", injected)
	}
	// Check suspended guidance
	if !strings.Contains(injected, "agentlink task result fix-001 suspended") {
		t.Errorf("injected missing suspended guidance, got: %s", injected)
	}
}

func TestPoller_skipsWhenBusy(t *testing.T) {
	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(pollerPullResponse{
			Items: []pollerInboxItem{
				{ID: "m1", Type: "msg", Content: "should not inject", FromDevice: "dev-a", FromSession: "main"},
			},
		})
	}))
	defer mockSrv.Close()

	injectCalls := 0
	ctx, cancel := context.WithCancel(context.Background())

	p := &Poller{
		Session:       "worker",
		Server:        mockSrv.URL,
		TeamID:        "tm_x",
		DeviceSession: "ds_test",
		Interval:      10 * time.Millisecond,
		Ctx:           ctx,
		Stdout:        io.Discard,
		IdleDetector:  &mockIdleDetector{busy: true},
		capturePane: func(string) (string, error) {
			return "❯", nil
		},
		sendKeys: func(_ string, text string) error {
			injectCalls++
			return nil
		},
		httpDo: http.DefaultClient.Do,
	}

	// Run for a few iterations then stop
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	p.Run()

	if injectCalls > 0 {
		t.Errorf("expected 0 injects when busy, got %d", injectCalls)
	}
}

func TestPoller_skipsWhenCapturerFails(t *testing.T) {
	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(pollerPullResponse{
			Items: []pollerInboxItem{
				{ID: "m1", Type: "msg", Content: "msg", FromDevice: "dev-a", FromSession: "main"},
			},
		})
	}))
	defer mockSrv.Close()

	injectCalls := 0
	ctx, cancel := context.WithCancel(context.Background())

	p := &Poller{
		Session:       "worker",
		Server:        mockSrv.URL,
		TeamID:        "tm_x",
		DeviceSession: "ds_test",
		Interval:      10 * time.Millisecond,
		Ctx:           ctx,
		Stdout:        io.Discard,
		IdleDetector:  &mockIdleDetector{busy: false, promptEmpty: true},
		capturePane: func(string) (string, error) {
			return "", io.ErrUnexpectedEOF // capture failed
		},
		sendKeys: func(_ string, text string) error {
			injectCalls++
			return nil
		},
		httpDo: http.DefaultClient.Do,
	}

	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	p.Run()

	if injectCalls > 0 {
		t.Errorf("expected 0 injects when capture fails, got %d", injectCalls)
	}
}

func TestPoller_skipsWhenInboxEmpty(t *testing.T) {
	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(pollerPullResponse{Items: []pollerInboxItem{}})
	}))
	defer mockSrv.Close()

	injectCalls := 0
	ctx, cancel := context.WithCancel(context.Background())

	p := &Poller{
		Session:       "worker",
		Server:        mockSrv.URL,
		TeamID:        "tm_x",
		DeviceSession: "ds_test",
		Interval:      10 * time.Millisecond,
		Ctx:           ctx,
		Stdout:        io.Discard,
		IdleDetector:  &mockIdleDetector{busy: false, promptEmpty: true},
		capturePane: func(string) (string, error) {
			return "❯\n", nil
		},
		sendKeys: func(_ string, text string) error {
			injectCalls++
			return nil
		},
		httpDo: http.DefaultClient.Do,
	}

	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	p.Run()

	if injectCalls > 0 {
		t.Errorf("expected 0 injects when inbox empty, got %d", injectCalls)
	}
}

// TestPoller_pullsFromTeamInboxWithDeviceAuth locks in the v2 wire contract:
// the poller pulls from /api/teams/<team>/inbox with a Device credential and
// its local session in X-Agentlink-Session — no Bearer token, no session query.
func TestPoller_pullsFromTeamInboxWithDeviceAuth(t *testing.T) {
	var gotPath, gotAuth, gotSession, gotLimit string
	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotSession = r.Header.Get("X-Agentlink-Session")
		gotLimit = r.URL.Query().Get("limit")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(pollerPullResponse{Items: []pollerInboxItem{}})
	}))
	defer mockSrv.Close()

	p := &Poller{
		Session:       "worker",
		Server:        mockSrv.URL,
		TeamID:        "tm_x",
		DeviceSession: "ds_secret",
		Stdout:        io.Discard,
	}
	p.initDefaults()

	if _, err := p.pullOne(); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/teams/tm_x/inbox" {
		t.Errorf("expected /api/teams/tm_x/inbox, got %s", gotPath)
	}
	if gotAuth != "Device ds_secret" {
		t.Errorf("expected Device auth, got %q", gotAuth)
	}
	if gotSession != "worker" {
		t.Errorf("expected X-Agentlink-Session=worker, got %q", gotSession)
	}
	if gotLimit != "1" {
		t.Errorf("expected limit=1, got %q", gotLimit)
	}
}

// ---------------------------------------------------------------------------
// Layer 3 — RunPoll entry point (error paths only)
// ---------------------------------------------------------------------------

func TestRunPoll_errors(t *testing.T) {
	t.Run("missing config", func(t *testing.T) {
		homeDir := t.TempDir()
		t.Setenv("HOME", homeDir)

		err := RunPoll()
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "config file not found") {
			t.Errorf("expected config error, got: %s", err)
		}
	})

	t.Run("missing credentials", func(t *testing.T) {
		homeDir := t.TempDir()
		t.Setenv("HOME", homeDir)
		os.MkdirAll(filepath.Join(homeDir, ".agentlink"), 0755)
		api.WriteAccountConfig(filepath.Join(homeDir, ".agentlink", "config.toml"), api.AgentConfig{
			Server: "http://localhost:1", UserID: "u_1", Username: "k", DeviceID: "d_1", Device: "test-dev", CurrentTeam: "tm_alpha",
		})

		err := RunPoll()
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
		api.WriteAccountConfig(filepath.Join(homeDir, ".agentlink", "config.toml"), api.AgentConfig{
			Server: "http://localhost:1", UserID: "u_1", Username: "k", DeviceID: "d_1", Device: "test-dev", CurrentTeam: "tm_alpha",
			Poll: api.PollConfig{Enabled: true, Interval: 5},
		})
		api.WriteCredentials(filepath.Join(homeDir, ".agentlink", "credentials.json"), api.AgentCredentials{DeviceSession: "ds_x"})

		err := RunPoll()
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), ".agentlink.toml not found") {
			t.Errorf("expected .agentlink.toml error, got: %s", err)
		}
	})

	t.Run("no active team", func(t *testing.T) {
		homeDir := t.TempDir()
		t.Setenv("HOME", homeDir)
		os.MkdirAll(filepath.Join(homeDir, ".agentlink"), 0755)
		api.WriteAccountConfig(filepath.Join(homeDir, ".agentlink", "config.toml"), api.AgentConfig{
			Server: "http://localhost:1", UserID: "u_1", Username: "k", DeviceID: "d_1", Device: "test-dev", CurrentTeam: "",
			Poll: api.PollConfig{Enabled: true, Interval: 5},
		})
		api.WriteCredentials(filepath.Join(homeDir, ".agentlink", "credentials.json"), api.AgentCredentials{DeviceSession: "ds_x"})

		err := RunPoll()
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "no active team") {
			t.Errorf("expected no-active-team error, got: %s", err)
		}
	})
}

func TestRunPoll_disabledByConfig(t *testing.T) {
	t.Run("poll disabled returns nil", func(t *testing.T) {
		homeDir := t.TempDir()
		t.Setenv("HOME", homeDir)

		agentlinkDir := filepath.Join(homeDir, ".agentlink")
		os.MkdirAll(agentlinkDir, 0755)
		api.WriteAccountConfig(filepath.Join(agentlinkDir, "config.toml"), api.AgentConfig{
			Server:      "http://srv:8080",
			UserID:      "u_1",
			Username:    "k",
			DeviceID:    "d_1",
			Device:      "dev",
			CurrentTeam: "tm_alpha",
			BaseDir:     "/tmp/agent_team",
			Poll:        api.PollConfig{Enabled: false, Interval: 5},
		})
		api.WriteCredentials(filepath.Join(agentlinkDir, "credentials.json"), api.AgentCredentials{DeviceSession: "ds_x"})

		err := RunPoll()
		if err != nil {
			t.Fatal(err)
		}
	})
}
