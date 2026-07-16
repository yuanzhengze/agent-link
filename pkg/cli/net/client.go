package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
)

// LoadAuth loads config and credentials in one call to reduce repetition.
func LoadAuth() (*AgentConfig, *AgentCredentials, error) {
	cfg, err := LoadConfig()
	if err != nil {
		return nil, nil, err
	}
	creds, err := LoadCredentials()
	if err != nil {
		return nil, nil, err
	}
	return cfg, creds, nil
}

// ConfigFilePath and CredentialsFilePath resolve the standard v2 CLI file
// locations under $HOME/.agentlink.
func ConfigFilePath() string {
	return filepath.Join(os.Getenv("HOME"), ".agentlink", "config.toml")
}

func CredentialsFilePath() string {
	return filepath.Join(os.Getenv("HOME"), ".agentlink", "credentials.json")
}

// TeamPath builds a team-scoped API path for the currently-selected team.
// Business commands require an active team; account/team-management commands do
// not and must not call this.
func TeamPath(cfg *AgentConfig, suffix string) (string, error) {
	if cfg.CurrentTeam == "" {
		return "", errors.New("no active team; run agentlink team use <team_id>")
	}
	return "/api/teams/" + url.PathEscape(cfg.CurrentTeam) + suffix, nil
}

// APIDo sends a Device-authenticated request to the agentlink server.
//
// The caller MUST close resp.Body when done.
func APIDo(cfg *AgentConfig, creds *AgentCredentials, method, path string, body any) (*http.Response, error) {
	return doDeviceRequest(cfg, creds, "", method, path, body)
}

// APIDoWithSession is APIDo plus the local Agent session header, used by the
// lock/apply/message/task flows where the server attributes the actor's session.
func APIDoWithSession(cfg *AgentConfig, creds *AgentCredentials, session, method, path string, body any) (*http.Response, error) {
	return doDeviceRequest(cfg, creds, session, method, path, body)
}

func doDeviceRequest(cfg *AgentConfig, creds *AgentCredentials, session, method, path string, body any) (*http.Response, error) {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("cannot encode request: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequest(method, cfg.Server+path, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("cannot create request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Device "+creds.DeviceSession)
	if session != "" {
		req.Header.Set("X-Agentlink-Session", session)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot connect to server %s: %w", cfg.Server, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(respBody, &e) == nil && e.Error != "" {
			return nil, fmt.Errorf("server returned %d: %s", resp.StatusCode, e.Error)
		}
		return nil, fmt.Errorf("server returned %d", resp.StatusCode)
	}

	return resp, nil
}
