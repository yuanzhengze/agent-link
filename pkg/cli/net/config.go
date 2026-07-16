package api

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const DefaultPollInterval = 5

type PollConfig struct {
	Enabled  bool
	Interval int
}

// AgentConfig is the v2 CLI configuration. It records the account identity
// (user/device), the currently-selected team, the local runtime preferences,
// and the per-session id map. CurrentTeam is intentionally optional so a freshly
// registered user can still run `team create`/`team join` before selecting one.
type AgentConfig struct {
	Server      string
	UserID      string
	Username    string
	DeviceID    string
	Device      string
	CurrentTeam string
	BaseDir     string
	Agent       string
	Poll        PollConfig
	Sessions    map[string]string
}

// AgentCredentials stores the single opaque Device Session used for
// `Authorization: Device`. The legacy APIKey field is retained only so the not-
// yet-migrated v1 net/runtime callers keep compiling during the CLI cutover; it
// is never written by v2 code and is removed once every caller uses APIDo.
type AgentCredentials struct {
	DeviceSession string `json:"device_session"`
	APIKey        string `json:"api_key,omitempty"`
}

// parseAgentConfig builds an AgentConfig from raw config.toml content without
// validating required fields, so both LoadConfig and SetCurrentTeam can share it.
func parseAgentConfig(content string) *AgentConfig {
	cfg := &AgentConfig{
		Server:      ReadTOML(content, "server"),
		UserID:      ReadTOML(content, "user_id"),
		Username:    ReadTOML(content, "username"),
		DeviceID:    ReadTOML(content, "device_id"),
		Device:      ReadTOML(content, "device"),
		CurrentTeam: ReadTOML(content, "current_team"),
		BaseDir:     ReadTOML(content, "base_dir"),
		Agent:       ReadTOML(content, "agent"),
		Poll: PollConfig{
			Enabled:  ReadTOMLBool(content, "poll.enabled", true),
			Interval: ReadTOMLInt(content, "poll.interval", DefaultPollInterval),
		},
		Sessions: ReadTOMLSection(content, "sessions"),
	}
	if cfg.Agent == "" {
		cfg.Agent = "claude"
	}
	return cfg
}

func LoadConfig() (*AgentConfig, error) {
	path := filepath.Join(os.Getenv("HOME"), ".agentlink", "config.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config file not found at %s", path)
	}

	cfg := parseAgentConfig(string(data))
	if cfg.Server == "" || cfg.DeviceID == "" || cfg.Device == "" {
		return nil, fmt.Errorf("invalid config file at %s: missing server, device_id, or device", path)
	}
	return cfg, nil
}

func LoadCredentials() (*AgentCredentials, error) {
	path := filepath.Join(os.Getenv("HOME"), ".agentlink", "credentials.json")
	return LoadCredentialsAt(path)
}

// LoadCredentialsAt reads and validates a credentials file at an explicit path.
// It requires a device_session and deliberately rejects a legacy api_key-only
// file so a stale v1 credential can never authenticate the v2 client.
func LoadCredentialsAt(path string) (*AgentCredentials, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("credentials file not found at %s", path)
	}

	var creds AgentCredentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return nil, fmt.Errorf("invalid credentials file at %s: %w", path, err)
	}
	if creds.DeviceSession == "" {
		return nil, fmt.Errorf("credentials file at %s is missing device_session", path)
	}
	return &creds, nil
}

// WriteCredentials atomically writes the device session credential with mode
// 0600, so a partial write can never leave a truncated or world-readable file.
func WriteCredentials(path string, creds AgentCredentials) error {
	data, err := json.MarshalIndent(AgentCredentials{DeviceSession: creds.DeviceSession}, "", "  ")
	if err != nil {
		return fmt.Errorf("cannot encode credentials: %w", err)
	}
	return writeFileAtomic(path, append(data, '\n'), 0o600)
}

// WriteAccountConfig atomically writes the full v2 config.toml (account, team,
// runtime, and sessions) with mode 0600.
func WriteAccountConfig(path string, cfg AgentConfig) error {
	return writeFileAtomic(path, []byte(buildAccountConfig(cfg)), 0o600)
}

// SetCurrentTeam rewrites only current_team while preserving every other
// account/runtime field and the sessions map.
func SetCurrentTeam(path, teamID string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("cannot read config: %w", err)
	}
	cfg := parseAgentConfig(string(data))
	cfg.CurrentTeam = teamID
	return WriteAccountConfig(path, *cfg)
}

// buildAccountConfig renders a v2 config.toml. Empty account/team fields are
// still written (as empty strings) so the file shape is stable and predictable.
func buildAccountConfig(cfg AgentConfig) string {
	interval := cfg.Poll.Interval
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	pollVal := "true"
	if !cfg.Poll.Enabled {
		pollVal = "false"
	}
	agent := cfg.Agent
	if agent == "" {
		agent = "claude"
	}
	content := fmt.Sprintf(`server = %q
user_id = %q
username = %q
device_id = %q
device = %q
current_team = %q
base_dir = %q
agent = %q

[poll]
enabled = %s
interval = %d
`, cfg.Server, cfg.UserID, cfg.Username, cfg.DeviceID, cfg.Device,
		cfg.CurrentTeam, cfg.BaseDir, agent, pollVal, interval)

	if len(cfg.Sessions) > 0 {
		content += "\n" + BuildSessionsSection(cfg.Sessions)
	}
	return content
}

// writeFileAtomic writes to a temp file in the same directory then renames it
// into place, so readers never observe a partially-written config/credential.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("cannot create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return fmt.Errorf("cannot chmod temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("cannot write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("cannot close temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("cannot rename temp file: %w", err)
	}
	return nil
}

func FindCurrentSession() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}

	dir := cwd
	for {
		path := filepath.Join(dir, ".agentlink.toml")
		if data, err := os.ReadFile(path); err == nil {
			session := ReadTOML(string(data), "session")
			if session != "" {
				return session, nil
			}
			return "", fmt.Errorf("session key not found in %s", path)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf(".agentlink.toml not found from %s upward", cwd)
}

func ReadTOML(content, key string) string {
	var section string
	var lookupKey string
	if idx := strings.Index(key, "."); idx >= 0 {
		section = key[:idx]
		lookupKey = key[idx+1:]
	} else {
		lookupKey = key
	}

	prefix := lookupKey + " = "
	inSection := section == ""
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			trimmed := strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
			inSection = section != "" && trimmed == section
			continue
		}
		if !inSection {
			continue
		}
		if strings.HasPrefix(line, prefix) {
			val := strings.TrimPrefix(line, prefix)
			val = strings.Trim(val, `"`)
			return val
		}
	}
	return ""
}

func ReadTOMLBool(content, key string, defaultVal bool) bool {
	v := ReadTOML(content, key)
	if v == "" {
		return defaultVal
	}
	return v == "true"
}

func ReadTOMLInt(content, key string, defaultVal int) int {
	v := ReadTOML(content, key)
	if v == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return defaultVal
	}
	return n
}

// readTOMLSection parses all key = "value" pairs under [section] into a map.
// Returns nil if the section is absent. Used for [sessions] in config.toml.
func ReadTOMLSection(content, section string) map[string]string {
	result := map[string]string{}
	inSection := false
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			trimmed := strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
			inSection = trimmed == section
			continue
		}
		if !inSection {
			continue
		}
		if idx := strings.Index(line, " = "); idx >= 0 {
			key := strings.TrimSpace(line[:idx])
			val := strings.Trim(strings.TrimSpace(line[idx+3:]), `"`)
			result[key] = val
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// updateSessionID rewrites the [sessions] entry for sessionName with a new
// session_id, preserving all other config fields. If [sessions] is absent,
// it is appended.
func UpdateSessionID(configPath, sessionName, sessionID string) error {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("cannot read config: %w", err)
	}
	content := string(data)

	sessions := ReadTOMLSection(content, "sessions")
	if sessions == nil {
		sessions = map[string]string{}
	}
	sessions[sessionName] = sessionID

	// Rebuild: keep everything up to [sessions], then rewrite [sessions].
	var rebuilt strings.Builder
	var beforeSessions strings.Builder
	inSessions := false
	sessionsWritten := false

	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			sectionName := strings.TrimSuffix(strings.TrimPrefix(trimmed, "["), "]")
			if sectionName == "sessions" {
				inSessions = true
				if !sessionsWritten {
					beforeSessions.WriteString(BuildSessionsSection(sessions))
					sessionsWritten = true
				}
				continue
			}
			if inSessions {
				inSessions = false
			}
		}
		if inSessions {
			continue
		}
		beforeSessions.WriteString(line)
		beforeSessions.WriteString("\n")
	}

	rebuilt.WriteString(beforeSessions.String())
	if !sessionsWritten {
		rebuilt.WriteString(BuildSessionsSection(sessions))
	}

	return os.WriteFile(configPath, []byte(rebuilt.String()), 0600)
}

func BuildSessionsSection(sessions map[string]string) string {
	var b strings.Builder
	b.WriteString("[sessions]\n")
	for _, name := range sortedSessionKeys(sessions) {
		fmt.Fprintf(&b, "%s = %q\n", name, sessions[name])
	}
	return b.String()
}

func sortedSessionKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// WriteConfigTOML writes the agent-level config file. When sessions is
// non-empty, appends a [sessions] segment with the recorded session_ids.
func WriteConfigTOML(path, server, device, baseDir, agent string, noPoll bool, sessions map[string]string) error {
	pollVal := "true"
	if noPoll {
		pollVal = "false"
	}
	content := fmt.Sprintf(`server = %q
device = %q
base_dir = %q
agent = %q

[poll]
enabled = %s
interval = 5
`, server, device, baseDir, agent, pollVal)

	if len(sessions) > 0 {
		content += "\n" + BuildSessionsSection(sessions)
	}
	return os.WriteFile(path, []byte(content), 0600)
}

// WriteSessionTOML writes the per-session .agentlink.toml marker file.
func WriteSessionTOML(path, session, device string) error {
	content := fmt.Sprintf(`session = %q
device = %q
`, session, device)
	return os.WriteFile(path, []byte(content), 0600)
}
