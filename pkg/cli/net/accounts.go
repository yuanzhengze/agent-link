package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/term"
)

// AccountIO carries the streams and password reader an account command uses, so
// tests can inject a scripted password source and capture output. A password is
// only ever read through Password and is never echoed or printed.
type AccountIO struct {
	In       io.Reader
	Out      io.Writer
	Err      io.Writer
	Password func() ([]byte, error)
}

func (a AccountIO) out() io.Writer {
	if a.Out != nil {
		return a.Out
	}
	return os.Stdout
}

func (a AccountIO) readPassword() ([]byte, error) {
	if a.Password != nil {
		return a.Password()
	}
	return term.ReadPassword(int(os.Stdin.Fd()))
}

// authUser mirrors the server's sanitized user view.
type authUser struct {
	ID                 string `json:"id"`
	Username           string `json:"username"`
	MustChangePassword bool   `json:"must_change_password"`
}

type deviceLoginResult struct {
	User             authUser `json:"user"`
	DeviceID         string   `json:"device_id"`
	DeviceCredential string   `json:"device_credential"`
}

// RunRegister creates an account, then performs a device login with the same
// in-memory password so a device credential is stored locally. Config and
// credentials are written only after both calls succeed.
func RunRegister(server, username, device string, aio AccountIO) error {
	server = normalizeServerURL(server)
	if strings.TrimSpace(username) == "" {
		return errors.New("username is required")
	}
	device = deviceOrDefault(device)

	password, err := promptNewPassword(aio, "Password: ", "Confirm password: ")
	if err != nil {
		return err
	}

	if err := postAuthJSON(server, "/api/auth/register", map[string]string{
		"username": username,
		"password": password,
	}, nil); err != nil {
		return err
	}

	dl, err := deviceLogin(server, username, password, device)
	if err != nil {
		return err
	}
	if err := saveAccount(server, username, device, dl); err != nil {
		return err
	}
	fmt.Fprintf(aio.out(), "Registered and logged in as %s (device %s).\n", username, device)
	return selectTeamAfterLogin()
}

// RunLogin performs a device login. If the server requires a password change,
// it prompts for a new password, changes it with the restricted device session,
// then logs in again before storing the credential.
func RunLogin(server, username, device string, aio AccountIO) error {
	server = normalizeServerURL(server)
	if strings.TrimSpace(username) == "" {
		return errors.New("username is required")
	}
	device = deviceOrDefault(device)

	pw, err := aio.readPassword()
	if err != nil {
		return fmt.Errorf("cannot read password: %w", err)
	}
	fmt.Fprintln(aio.out())
	password := string(pw)

	dl, err := deviceLogin(server, username, password, device)
	if err != nil {
		return err
	}

	if dl.User.MustChangePassword {
		newPassword, err := promptNewPassword(aio, "New password: ", "Confirm new password: ")
		if err != nil {
			return err
		}
		if err := changePassword(server, dl.DeviceCredential, password, newPassword); err != nil {
			return err
		}
		password = newPassword
		dl, err = deviceLogin(server, username, password, device)
		if err != nil {
			return err
		}
	}

	if err := saveAccount(server, username, device, dl); err != nil {
		return err
	}
	fmt.Fprintf(aio.out(), "Logged in as %s (device %s).\n", username, device)
	return selectTeamAfterLogin()
}

// RunLogout revokes the device session on the server and deletes the local
// credential. The account config (server/user/device/team) is preserved so the
// user can log back in without re-entering it.
func RunLogout(aio AccountIO) error {
	cfg, creds, err := LoadAuth()
	if err != nil {
		return err
	}
	if _, err := APIDo(cfg, creds, http.MethodPost, "/api/auth/device-logout", nil); err != nil {
		return err
	}
	if err := os.Remove(CredentialsFilePath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("cannot delete local credential: %w", err)
	}
	fmt.Fprintln(aio.out(), "Logged out.")
	return nil
}

// deviceLogin authenticates a username/password against a device name and
// returns the device credential.
func deviceLogin(server, username, password, device string) (deviceLoginResult, error) {
	var dl deviceLoginResult
	if err := postAuthJSON(server, "/api/auth/device-login", map[string]string{
		"username":    username,
		"password":    password,
		"device_name": device,
	}, &dl); err != nil {
		return deviceLoginResult{}, err
	}
	if dl.DeviceCredential == "" {
		return deviceLoginResult{}, errors.New("server did not return a device credential")
	}
	return dl, nil
}

// changePassword updates the password using a restricted device session, as
// returned by a must-change device login.
func changePassword(server, deviceCredential, current, next string) error {
	body, err := json.Marshal(map[string]string{
		"current_password": current,
		"new_password":     next,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, server+"/api/auth/change-password", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Device "+deviceCredential)
	return doAuthRequest(req, nil)
}

// postAuthJSON posts an unauthenticated JSON request to an auth endpoint and,
// when out is non-nil, decodes the response body.
func postAuthJSON(server, path string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, server+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return doAuthRequest(req, out)
}

func doAuthRequest(req *http.Request, out any) error {
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("cannot connect to server: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return fmt.Errorf("server returned %d: %s", resp.StatusCode, e.Error)
		}
		return fmt.Errorf("server returned %d", resp.StatusCode)
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("cannot decode response: %w", err)
		}
	}
	return nil
}

// saveAccount merges the account identity into any existing runtime config,
// preserving base_dir/agent/poll/sessions, then writes config and credentials.
func saveAccount(server, username, device string, dl deviceLoginResult) error {
	cfgPath := ConfigFilePath()
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		return err
	}

	cfg := AgentConfig{Agent: "claude", Poll: PollConfig{Enabled: true, Interval: DefaultPollInterval}}
	if data, err := os.ReadFile(cfgPath); err == nil {
		cfg = *parseAgentConfig(string(data))
	}
	cfg.Server = server
	cfg.UserID = dl.User.ID
	cfg.Username = username
	cfg.DeviceID = dl.DeviceID
	cfg.Device = device

	if err := WriteAccountConfig(cfgPath, cfg); err != nil {
		return err
	}
	return WriteCredentials(CredentialsFilePath(), AgentCredentials{DeviceSession: dl.DeviceCredential})
}

// selectTeamAfterLogin picks a current team if none is selected yet: it keeps a
// still-valid selection, otherwise selects the first membership. Missing teams
// are non-fatal (a freshly-registered user simply has none).
func selectTeamAfterLogin() error {
	cfg, creds, err := LoadAuth()
	if err != nil {
		return nil
	}
	teams, err := fetchTeams(cfg, creds)
	if err != nil || len(teams) == 0 {
		return nil
	}
	for _, t := range teams {
		if t.ID == cfg.CurrentTeam {
			return nil
		}
	}
	return SetCurrentTeam(ConfigFilePath(), teams[0].ID)
}

// promptNewPassword reads a password and its confirmation, rejecting a mismatch.
// The password is never echoed or printed.
func promptNewPassword(aio AccountIO, prompt, confirm string) (string, error) {
	fmt.Fprint(aio.out(), prompt)
	first, err := aio.readPassword()
	if err != nil {
		return "", fmt.Errorf("cannot read password: %w", err)
	}
	fmt.Fprintln(aio.out())
	fmt.Fprint(aio.out(), confirm)
	second, err := aio.readPassword()
	if err != nil {
		return "", fmt.Errorf("cannot read password: %w", err)
	}
	fmt.Fprintln(aio.out())
	if !bytes.Equal(first, second) {
		return "", errors.New("passwords do not match")
	}
	return string(first), nil
}

func deviceOrDefault(device string) string {
	device = strings.TrimSpace(device)
	if device != "" {
		return device
	}
	if host, err := os.Hostname(); err == nil && host != "" {
		return host
	}
	return "device"
}

// normalizeServerURL trims trailing slashes and defaults the scheme to https.
func normalizeServerURL(server string) string {
	server = strings.TrimSpace(server)
	server = strings.TrimRight(server, "/")
	if server == "" {
		return server
	}
	if !strings.HasPrefix(server, "http://") && !strings.HasPrefix(server, "https://") {
		server = "https://" + server
	}
	return server
}
