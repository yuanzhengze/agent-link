package rt

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	api "github.com/team/agentlink/pkg/cli/net"
)

// RunResume rebuilds tmux sessions and pollers from the on-disk config,
// without re-registering the device. Each session's Claude Code is resumed
// to its recorded session_id (from [sessions]); configs without [sessions]
// fall back to --continue (23c).
func RunResume() error {
	cfg, err := api.LoadConfig()
	if err != nil {
		return err
	}
	creds, err := api.LoadCredentials()
	if err != nil {
		return err
	}
	if cfg.CurrentTeam == "" {
		return fmt.Errorf("no active team; run agentlink team use <team_id> before resume")
	}

	if err := checkTmux(); err != nil {
		return err
	}

	// Verify the device session is still valid AND we are still a member of
	// the current team. A team heartbeat exercises both: a revoked device
	// session yields 401, and a removed membership yields 404/403 — either
	// way resume cannot proceed and the user must re-login or re-select a
	// team.
	if err := pingServer(cfg, creds); err != nil {
		return fmt.Errorf("device/team check failed (was the device logged out or removed from the team?): %w", err)
	}

	// Determine which sessions to resume. With [sessions], use those keys;
	// without (legacy config), scan BaseDir for session directories (23c).
	sessionNames, fallback := resumeSessionList(cfg)
	if len(sessionNames) == 0 {
		return fmt.Errorf("no sessions found under %s; run `agentlink init` first", cfg.BaseDir)
	}

	if fallback {
		fmt.Println("⚠ config.toml has no [sessions] segment — using --continue fallback")
		fmt.Println("  Run `agentlink init` again to enable precise session resume")
	}

	// Launch tmux sessions with Resume=true. launchSessions reads
	// opts.Existing to decide --resume <id> vs --continue per session.
	if _, err := launchSessions(cfg.BaseDir, cfg.Agent, launchOpts{
		Resume:   true,
		NoPoll:   !cfg.Poll.Enabled,
		Existing: cfg.Sessions,
	}); err != nil {
		return err
	}

	// Send a heartbeat so the device shows online immediately.
	if err := api.RunPing(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: heartbeat failed: %v\n", err)
	}

	fmt.Println("✓ sessions resumed")
	for _, name := range sessionNames {
		fmt.Printf("  %s — attach with: agentlink attach %s\n", name, name)
	}
	return nil
}

// resumeSessionList returns the session names to resume and whether the
// [sessions] segment was absent (triggering 23c fallback).
func resumeSessionList(cfg *api.AgentConfig) ([]string, bool) {
	if len(cfg.Sessions) > 0 {
		names := make([]string, 0, len(cfg.Sessions))
		for k := range cfg.Sessions {
			names = append(names, k)
		}
		sort.Strings(names)
		return names, false
	}
	// Legacy config: scan BaseDir for directories containing .agentlink.toml
	var names []string
	entries, err := os.ReadDir(cfg.BaseDir)
	if err != nil {
		return nil, true
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		tomlPath := filepath.Join(cfg.BaseDir, e.Name(), ".agentlink.toml")
		if _, err := os.Stat(tomlPath); err == nil {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, true
}

// pingServer verifies the device session is valid and the current team
// membership still holds by issuing a team-scoped heartbeat. APIDo already maps
// any non-2xx status (401 revoked device, 404/403 removed membership) to an
// error, so a successful return means both checks passed.
func pingServer(cfg *api.AgentConfig, creds *api.AgentCredentials) error {
	path, err := api.TeamPath(cfg, "/agents/heartbeat")
	if err != nil {
		return err
	}
	resp, err := api.APIDo(cfg, creds, "POST", path, nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}
