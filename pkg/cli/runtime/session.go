package rt

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/team/agentlink/pkg/adapter"
	api "github.com/team/agentlink/pkg/cli/net"
)

func RunSessionAdd(name string) error {
	cfg, creds, err := api.LoadAuth()
	if err != nil {
		return err
	}

	if err := checkTmux(); err != nil {
		return err
	}

	sessionDir := filepath.Join(cfg.BaseDir, name)
	if _, err := os.Stat(sessionDir); err == nil {
		return fmt.Errorf("session directory %q already exists", sessionDir)
	}

	// Fetch current sessions from server
	currentSessions, err := fetchSessions(cfg, creds)
	if err != nil {
		return err
	}

	// Check for duplicate
	for _, s := range currentSessions {
		if s == name {
			return fmt.Errorf("session %q already registered on device", name)
		}
	}

	updatedSessions := append(currentSessions, name)

	// Call PATCH /agents/sessions
	if err := patchSessions(cfg, creds, updatedSessions); err != nil {
		return err
	}

	// Create local directory
	if err := os.MkdirAll(sessionDir, 0755); err != nil {
		return fmt.Errorf("cannot create %s: %w", sessionDir, err)
	}

	// Write .agentlink.toml
	tomlPath := filepath.Join(sessionDir, ".agentlink.toml")
	if err := api.WriteSessionTOML(tomlPath, name, cfg.Device); err != nil {
		return fmt.Errorf("cannot write %s: %w", tomlPath, err)
	}

	// Write CLAUDE.md
	claudePath := filepath.Join(sessionDir, "CLAUDE.md")
	launcher := adapter.NewLauncher(cfg.Agent)
	claudeContent := launcher.InitTemplate(name, cfg.Device)
	if err := os.WriteFile(claudePath, []byte(claudeContent), 0600); err != nil {
		return fmt.Errorf("cannot write %s: %w", claudePath, err)
	}

	// Launch tmux session and record Claude session_id
	sessions, err := launchSessionsFn(cfg.BaseDir, cfg.Agent, launchOpts{
		Resume:   false,
		NoPoll:   !cfg.Poll.Enabled,
		Existing: nil,
		Only:     name,
	})
	if err != nil {
		return fmt.Errorf("cannot launch session: %w", err)
	}

	// Update config.toml [sessions] with the new session_id
	configPath := filepath.Join(os.Getenv("HOME"), ".agentlink", "config.toml")
	if sid, ok := sessions[name]; ok && sid != "" {
		if err := api.UpdateSessionID(configPath, name, sid); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not record session_id: %v\n", err)
		}
	}

	fmt.Printf("✓ Session %q added\n", name)
	fmt.Printf("  Directory: %s\n", sessionDir)
	fmt.Println()
	fmt.Printf("Next step:\n")
	fmt.Printf("  agentlink attach %s    # enter the session\n", name)

	return nil
}

func RunSessionRemove(name string) error {
	cfg, creds, err := api.LoadAuth()
	if err != nil {
		return err
	}

	// Fetch current sessions from server
	currentSessions, err := fetchSessions(cfg, creds)
	if err != nil {
		return err
	}

	// Check session exists
	found := false
	for _, s := range currentSessions {
		if s == name {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("session %q not found on device", name)
	}

	if len(currentSessions) <= 1 {
		return fmt.Errorf("cannot remove the last session; use 'agentlink uninstall' instead")
	}

	// Remove from list
	var updatedSessions []string
	for _, s := range currentSessions {
		if s != name {
			updatedSessions = append(updatedSessions, s)
		}
	}

	// Call PATCH /agents/sessions
	if err := patchSessions(cfg, creds, updatedSessions); err != nil {
		return err
	}

	// Remove local directory
	sessionDir := filepath.Join(cfg.BaseDir, name)
	if err := os.RemoveAll(sessionDir); err != nil {
		return fmt.Errorf("warning: session removed from server but could not remove %s: %w", sessionDir, err)
	}

	fmt.Printf("✓ Session %q removed\n", name)
	return nil
}

func RunUninstall() error {
	cfg, creds, err := api.LoadAuth()
	if err != nil {
		return err
	}

	// Kill tmux sessions (best-effort)
	killSessionSessions(cfg.BaseDir)

	// Revoke this device's session on the server before wiping local state, so
	// the stored credential can never be reused. If the server rejects the
	// request, keep local files intact so the user can retry.
	resp, err := api.APIDo(cfg, creds, "POST", "/api/auth/device-logout", nil)
	if err != nil {
		return err
	}
	resp.Body.Close()

	// Delete work directory
	if cfg.BaseDir != "" {
		if err := os.RemoveAll(cfg.BaseDir); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not remove %s: %v\n", cfg.BaseDir, err)
		}
	}

	// Clean up local .agentlink directory
	agentlinkDir := filepath.Join(os.Getenv("HOME"), ".agentlink")
	if err := os.RemoveAll(agentlinkDir); err != nil {
		return fmt.Errorf("warning: device unregistered from server but could not remove %s: %w", agentlinkDir, err)
	}

	fmt.Println("✓ Device unregistered and local files cleaned")
	fmt.Println("  To also remove agentlink from PATH, edit ~/.bashrc or ~/.zshrc")
	return nil
}

// killSessionSessions kills tmux sessions for each subdirectory in baseDir.
// Best-effort; errors are silently ignored.
func killSessionSessions(baseDir string) {
	if baseDir == "" {
		return
	}
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		exec.Command("tmux", "kill-session", "-t", name).Run()
		exec.Command("tmux", "kill-session", "-t", name+"-poller").Run()
	}
}

func RunAttach(session string) error {
	cfg, err := api.LoadConfig()
	if err != nil {
		return err
	}

	// Check tmux is available
	if _, err := exec.LookPath("tmux"); err != nil {
		return fmt.Errorf("tmux is required but not found in PATH")
	}

	// Check directory exists
	sessionDir := filepath.Join(cfg.BaseDir, session)
	if _, err := os.Stat(sessionDir); err != nil {
		return fmt.Errorf("session directory %q not found; use 'agentlink session add %s' first", sessionDir, session)
	}

	// Check if tmux session exists
	hasSession := exec.Command("tmux", "has-session", "-t", session).Run() == nil

	if hasSession {
		cmd := exec.Command("tmux", "attach", "-t", session)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}

	launcher := adapter.NewLauncher(cfg.Agent)
	name, args := launcher.Command()
	cmdArgs := append([]string{"new-session", "-c", sessionDir, name}, args...)
	cmd := exec.Command("tmux", cmdArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// -- helpers --

func checkTmux() error {
	if _, err := exec.LookPath("tmux"); err != nil {
		return fmt.Errorf("tmux is required but not found in PATH")
	}
	return nil
}

// fetchSessions returns the session list for THIS device within the current
// team. The v2 /agents endpoint returns the whole team roster, so it selects
// the entry whose device_id matches the local config; a device that has not
// yet appeared (no heartbeat) simply has no sessions.
func fetchSessions(cfg *api.AgentConfig, creds *api.AgentCredentials) ([]string, error) {
	path, err := api.TeamPath(cfg, "/agents")
	if err != nil {
		return nil, err
	}
	resp, err := api.APIDo(cfg, creds, "GET", path, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var list struct {
		Agents []struct {
			DeviceID string   `json:"device_id"`
			Sessions []string `json:"sessions"`
		} `json:"agents"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("cannot parse response: %w", err)
	}

	for _, a := range list.Agents {
		if a.DeviceID == cfg.DeviceID {
			return a.Sessions, nil
		}
	}
	return nil, nil
}

func patchSessions(cfg *api.AgentConfig, creds *api.AgentCredentials, sessions []string) error {
	path, err := api.TeamPath(cfg, "/agents/sessions")
	if err != nil {
		return err
	}
	resp, err := api.APIDo(cfg, creds, "PATCH", path, map[string][]string{
		"sessions": sessions,
	})
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}
