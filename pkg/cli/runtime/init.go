package rt

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/team/agentlink/pkg/adapter"
	api "github.com/team/agentlink/pkg/cli/net"
)

// InitOptions configures a purely local team-workspace initialization. Account
// and team identity come from a prior `agentlink login` + `agentlink team use`;
// init performs no registration and makes no network calls.
type InitOptions struct {
	Path   string
	Agent  string
	NoPoll bool
	Force  bool
}

// sessionNames are the tmux sessions init always creates.
var sessionNames = []string{"main", "worker"}

// launchSessionsFn is indirected so tests can run init end to end without
// spawning real tmux/claude processes.
var launchSessionsFn = launchSessions

func RunInit(opts *InitOptions) error {
	if opts.Agent == "" {
		opts.Agent = "claude"
	}

	// Identity must already exist: init is local-only and never registers.
	cfg, creds, err := api.LoadAuth()
	if err != nil {
		return fmt.Errorf("not logged in; run `agentlink login` first: %w", err)
	}
	if cfg.CurrentTeam == "" {
		return errors.New("no active team; run `agentlink team use <team_id>` before init")
	}
	if creds.DeviceSession == "" {
		return errors.New("missing device credential; run `agentlink login` first")
	}
	device := cfg.Device

	// Pre-check prerequisites
	launcher := adapter.NewLauncher(opts.Agent)
	if launcher == nil {
		return fmt.Errorf("unknown agent type %q", opts.Agent)
	}
	if err := launcher.CheckPrereqs(); err != nil {
		return err
	}

	// Resolve and validate target directory
	absPath, err := filepath.Abs(opts.Path)
	if err != nil {
		return fmt.Errorf("cannot resolve path %q: %w", opts.Path, err)
	}

	if _, err := os.Stat(absPath); err == nil {
		if !opts.Force {
			return fmt.Errorf("directory %q already exists; use --force to override", absPath)
		}
	}

	// Create directories
	agentlinkDir := filepath.Join(os.Getenv("HOME"), ".agentlink")
	if err := os.MkdirAll(agentlinkDir, 0755); err != nil {
		return fmt.Errorf("cannot create %s: %w", agentlinkDir, err)
	}

	for _, session := range sessionNames {
		if err := os.MkdirAll(filepath.Join(absPath, session), 0755); err != nil {
			return fmt.Errorf("cannot create %s: %w", filepath.Join(absPath, session), err)
		}
	}

	// Write config.toml preserving account/team identity; [sessions] is added
	// after the tmux launch records ids. The device credential is left as-is.
	if err := writeInitConfig(cfg, absPath, opts, nil); err != nil {
		return fmt.Errorf("cannot write config: %w", err)
	}

	// Write .agentlink.toml and CLAUDE.md for each session
	for _, session := range sessionNames {
		sessionDir := filepath.Join(absPath, session)
		tomlPath := filepath.Join(sessionDir, ".agentlink.toml")
		if err := api.WriteSessionTOML(tomlPath, session, device); err != nil {
			return fmt.Errorf("cannot write %s: %w", tomlPath, err)
		}
		claudePath := filepath.Join(sessionDir, "CLAUDE.md")
		if err := os.WriteFile(claudePath, []byte(launcher.InitTemplate(session, device)), 0600); err != nil {
			return fmt.Errorf("cannot write %s: %w", claudePath, err)
		}
	}

	// Launch tmux sessions and record Claude session_ids
	sessions, err := launchSessionsFn(absPath, opts.Agent, launchOpts{
		Resume:   false,
		NoPoll:   opts.NoPoll,
		Existing: nil,
	})
	if err != nil {
		return fmt.Errorf("cannot launch sessions: %w", err)
	}

	// Rewrite config.toml with [sessions] segment
	if err := writeInitConfig(cfg, absPath, opts, sessions); err != nil {
		return fmt.Errorf("cannot rewrite config with session ids: %w", err)
	}

	// Print success
	fmt.Printf("✓ Agent team initialized at %s\n", absPath)
	fmt.Printf("✓ Using account %q on device %q (team %s)\n", cfg.Username, device, cfg.CurrentTeam)
	fmt.Println("✓ tmux sessions created: main, worker")
	if opts.NoPoll {
		fmt.Println("  Auto-polling disabled (use agentlink poll to start manually)")
	} else {
		fmt.Println("✓ poller sessions created: main-poller, worker-poller")
	}
	fmt.Println()
	fmt.Println("Next steps:")
	fmt.Println("  agentlink attach worker    # switch to worker session")

	return nil
}

// writeInitConfig persists the config.toml, keeping the logged-in account and
// active team while updating only the local runtime fields (base dir, agent,
// poll, sessions).
func writeInitConfig(cfg *api.AgentConfig, baseDir string, opts *InitOptions, sessions map[string]string) error {
	out := *cfg
	out.BaseDir = baseDir
	out.Agent = opts.Agent
	interval := cfg.Poll.Interval
	if interval <= 0 {
		interval = api.DefaultPollInterval
	}
	out.Poll = api.PollConfig{Enabled: !opts.NoPoll, Interval: interval}
	out.Sessions = sessions
	return api.WriteAccountConfig(api.ConfigFilePath(), out)
}

// launchOpts controls how launchSessions starts each tmux session.
type launchOpts struct {
	// Resume=true starts the agent with --resume <session_id>; false starts fresh.
	Resume bool
	// NoPoll suppresses the per-session poller tmux session.
	NoPoll bool
	// Existing maps session name → recorded session_id (from 23a).
	// On Resume, sessions with a non-empty id use --resume; others fall back
	// to --continue (23c). Ignored when Resume=false.
	Existing map[string]string
	// Only, when non-empty, launches just that single session (used by
	// `session add`). When empty, launches "main" and "worker".
	Only string
}

// launchSessions starts tmux session(s) + optional poller under baseDir.
// Returns a map of session name → Claude session_id (recorded from
// ~/.claude.json after each launch).
//
// Sessions are launched serially so that each Claude Code process writes a
// distinct lastSessionId before the next one starts; otherwise both would
// race on the same ~/.claude.json field.
func launchSessions(baseDir, agent string, opts launchOpts) (map[string]string, error) {
	selfExe, _ := os.Executable()
	launcher := adapter.NewLauncher(agent)
	if launcher == nil {
		return nil, fmt.Errorf("unknown agent type %q", agent)
	}

	var sessions []string
	if opts.Only != "" {
		sessions = []string{opts.Only}
	} else {
		sessions = []string{"main", "worker"}
	}
	recorded := map[string]string{}

	for _, session := range sessions {
		// Kill any pre-existing tmux session for this name
		exec.Command("tmux", "kill-session", "-t", session).Run()
		exec.Command("tmux", "kill-session", "-t", session+"-poller").Run()

		dir := filepath.Join(baseDir, session)
		name, args := launcher.Command()
		if opts.Resume {
			sessionID := opts.Existing[session]
			args = launcher.ResumeArgs(sessionID)
		}
		cmdArgs := append([]string{"new-session", "-d", "-s", session, "-c", dir, name}, args...)
		if err := exec.Command("tmux", cmdArgs...).Run(); err != nil {
			return nil, fmt.Errorf("cannot create tmux session %q: %w", session, err)
		}

		fmt.Printf("  %s — waiting for Claude to start", session)
		id := opts.Existing[session]
		if !opts.Resume || id == "" {
			recorded[session] = readClaudeSessionIDWithTimeout(10 * time.Second)
		} else {
			recorded[session] = id
		}
		if recorded[session] != "" {
			fmt.Println(" ✓")
		} else {
			fmt.Println(" (session_id unavailable, continue fallback)")
		}

		if !opts.NoPoll {
			exec.Command("tmux", "new-session", "-d", "-s", session+"-poller", "-c", dir, selfExe, "poll").Run()
		}
	}

	return recorded, nil
}

// readClaudeSessionIDWithTimeout polls ~/.claude.json for lastSessionId,
// waiting up to timeout for a non-empty value. Returns "" on timeout.
func readClaudeSessionIDWithTimeout(timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		id, _ := readClaudeSessionID()
		if id != "" {
			return id
		}
		time.Sleep(500 * time.Millisecond)
	}
	return ""
}

// readClaudeSessionID reads ~/.claude.json and returns lastSessionId.
// Returns "" if the file is absent or the field is empty.
func readClaudeSessionID() (string, error) {
	path := filepath.Join(os.Getenv("HOME"), ".claude.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var doc struct {
		LastSessionID string `json:"lastSessionId"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return "", err
	}
	return doc.LastSessionID, nil
}
