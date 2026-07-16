package api

import (
	"encoding/json"
	"fmt"
	"strings"
)

func RunPing() error {
	cfg, creds, err := LoadAuth()
	if err != nil {
		return err
	}

	path, err := TeamPath(cfg, "/agents/heartbeat")
	if err != nil {
		return err
	}
	resp, err := APIDo(cfg, creds, "POST", path, nil)
	if err != nil {
		return err
	}
	resp.Body.Close()

	fmt.Println("✓ Heartbeat sent")
	return nil
}

type agentInfo struct {
	UserID     string   `json:"user_id"`
	Username   string   `json:"username"`
	DeviceID   string   `json:"device_id"`
	DeviceName string   `json:"device_name"`
	ClientType string   `json:"client_type"`
	Sessions   []string `json:"sessions"`
	LastSeen   string   `json:"last_seen"`
	Online     bool     `json:"online"`
}

type agentListResponse struct {
	Agents []agentInfo `json:"agents"`
}

// RunList lists every device in the active team. The v2 API always returns the
// full team roster, so the `all` flag is accepted for CLI compatibility but no
// longer changes the request.
func RunList(all bool) error {
	cfg, creds, err := LoadAuth()
	if err != nil {
		return err
	}

	path, err := TeamPath(cfg, "/agents")
	if err != nil {
		return err
	}
	resp, err := APIDo(cfg, creds, "GET", path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var list agentListResponse
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return fmt.Errorf("cannot parse response: %w", err)
	}

	if len(list.Agents) == 0 {
		fmt.Println("No agents")
		return nil
	}

	for i, a := range list.Agents {
		if i > 0 {
			fmt.Println("---")
		}
		status := "offline"
		if a.Online {
			status = "online"
		}
		sessions := strings.Join(a.Sessions, ", ")
		if sessions == "" {
			sessions = "(none)"
		}
		device := a.DeviceName
		if device == "" {
			device = a.DeviceID
		}
		fmt.Printf("User:       %s\n", a.Username)
		fmt.Printf("Device:     %s\n", device)
		fmt.Printf("Sessions:   %s\n", sessions)
		fmt.Printf("Status:     %s\n", status)
		if a.LastSeen != "" {
			fmt.Printf("Last seen:  %s\n", a.LastSeen)
		}
	}

	return nil
}
