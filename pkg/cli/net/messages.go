package api

import (
	"encoding/json"
	"fmt"
	"strings"
)

type recipientStatusJSON struct {
	DeviceID string `json:"device_id"`
	Session  string `json:"session"`
	Current  string `json:"current"`
}

func displayRecipientStatus(status *recipientStatusJSON) {
	if status == nil {
		return
	}
	if status.DeviceID == "" && status.Session == "" {
		return
	}
	fmt.Printf("\n%s %s session 当前状态: %s\n", status.DeviceID, status.Session, status.Current)
}

// resolveTarget normalizes a target to "device:session". A bare session name is
// interpreted as another session on the caller's own device.
func resolveTarget(cfg *AgentConfig, target string) string {
	if strings.Contains(target, ":") {
		return target
	}
	return cfg.DeviceID + ":" + target
}

func RunSend(target, content string, interrupt bool, title string) error {
	cfg, creds, err := LoadAuth()
	if err != nil {
		return err
	}

	session, err := FindCurrentSession()
	if err != nil {
		return err
	}

	path, err := TeamPath(cfg, "/messages")
	if err != nil {
		return err
	}
	resp, err := APIDoWithSession(cfg, creds, session, "POST", path, map[string]any{
		"to":        resolveTarget(cfg, target),
		"interrupt": interrupt,
		"title":     title,
		"content":   content,
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var result struct {
		ID              string               `json:"id"`
		RecipientStatus *recipientStatusJSON `json:"recipient_status,omitempty"`
	}
	json.NewDecoder(resp.Body).Decode(&result)

	fmt.Printf("✓ 消息已投递（ID: %s）\n", result.ID)
	displayRecipientStatus(result.RecipientStatus)

	return nil
}

func RunPull(all bool) error {
	cfg, creds, err := LoadAuth()
	if err != nil {
		return err
	}

	session, err := FindCurrentSession()
	if err != nil {
		return err
	}

	limit := 1
	if all {
		limit = 10
	}

	suffix := fmt.Sprintf("/inbox?limit=%d", limit)
	path, err := TeamPath(cfg, suffix)
	if err != nil {
		return err
	}
	resp, err := APIDoWithSession(cfg, creds, session, "GET", path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var result struct {
		Items []struct {
			ID          string `json:"id"`
			Type        string `json:"type"`
			FromDevice  string `json:"from_device"`
			FromSession string `json:"from_session"`
			Content     string `json:"content"`
			CreatedAt   string `json:"created_at"`
			TaskID      string `json:"task_id,omitempty"`
		} `json:"items"`
	}
	json.NewDecoder(resp.Body).Decode(&result)

	if len(result.Items) == 0 {
		fmt.Println("No messages")
		return nil
	}

	for _, msg := range result.Items {
		fmt.Printf("[%s] %s from %s:%s — %s\n", msg.Type, msg.ID, msg.FromDevice, msg.FromSession, msg.CreatedAt)
		if msg.Type == "task" && msg.TaskID != "" {
			fmt.Printf("  Task ID: %s\n", msg.TaskID)
		}
		fmt.Println(msg.Content)
		fmt.Println("---")
	}

	return nil
}
