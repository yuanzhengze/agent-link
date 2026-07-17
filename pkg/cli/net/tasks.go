package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

func RunTaskSend(target, taskID, content string, interrupt bool, title string) error {
	cfg, creds, err := LoadAuth()
	if err != nil {
		return err
	}

	session, err := FindCurrentSession()
	if err != nil {
		return err
	}

	path, err := TeamPath(cfg, "/tasks")
	if err != nil {
		return err
	}

	body, _ := json.Marshal(map[string]any{
		"to":        resolveTarget(cfg, target),
		"task_id":   taskID,
		"title":     title,
		"content":   content,
		"interrupt": interrupt,
	})

	req, err := http.NewRequest("POST", cfg.Server+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("cannot create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Device "+creds.DeviceSession)
	req.Header.Set("X-Agentlink-Session", session)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("cannot connect to server %s: %w", cfg.Server, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusConflict {
		var errResp struct {
			Error           string               `json:"error"`
			RecipientStatus *recipientStatusJSON `json:"recipient_status,omitempty"`
		}
		json.Unmarshal(respBody, &errResp)

		fmt.Printf("✗ %s\n", errResp.Error)
		displayRecipientStatus(errResp.RecipientStatus)
		return nil
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(respBody, &e) == nil && e.Error != "" {
			return fmt.Errorf("server returned %d: %s", resp.StatusCode, e.Error)
		}
		return fmt.Errorf("server returned %d", resp.StatusCode)
	}

	var sendResp struct {
		ID              string               `json:"id"`
		TaskID          string               `json:"task_id,omitempty"`
		RecipientStatus *recipientStatusJSON `json:"recipient_status,omitempty"`
	}
	json.Unmarshal(respBody, &sendResp)
	if sendResp.TaskID != "" {
		taskID = sendResp.TaskID
	}

	fmt.Printf("✓ Task %s sent to %s\n", taskID, resolveTarget(cfg, target))
	displayRecipientStatus(sendResp.RecipientStatus)
	fmt.Print("  Result will be delivered via notification when done — no need to poll.\n")
	return nil
}

func RunTaskResult(taskID, status, result string) error {
	cfg, creds, err := LoadAuth()
	if err != nil {
		return err
	}

	path, err := taskActionPath(cfg, taskID, "/result")
	if err != nil {
		return err
	}
	resp, err := APIDo(cfg, creds, "POST", path, map[string]string{
		"status": status,
		"result": result,
	})
	if err != nil {
		return err
	}
	resp.Body.Close()

	verb := "completed"
	if status == "suspended" {
		verb = "suspended"
	}
	fmt.Printf("✓ Task %s %s\n", taskID, verb)
	return nil
}

func RunTaskResume(taskID, content string) error {
	cfg, creds, err := LoadAuth()
	if err != nil {
		return err
	}

	path, err := taskActionPath(cfg, taskID, "/resume")
	if err != nil {
		return err
	}
	resp, err := APIDo(cfg, creds, "POST", path, map[string]string{
		"content": content,
	})
	if err != nil {
		return err
	}
	resp.Body.Close()

	fmt.Printf("✓ Task %s resumed\n", taskID)
	return nil
}

func RunTaskCancel(taskID string) error {
	cfg, creds, err := LoadAuth()
	if err != nil {
		return err
	}

	path, err := taskActionPath(cfg, taskID, "/cancel")
	if err != nil {
		return err
	}
	resp, err := APIDo(cfg, creds, "POST", path, nil)
	if err != nil {
		return err
	}
	resp.Body.Close()

	fmt.Printf("✓ Task %s cancelled\n", taskID)
	return nil
}

func RunTaskReopen(taskID, reason string) error {
	cfg, creds, err := LoadAuth()
	if err != nil {
		return err
	}

	path, err := taskActionPath(cfg, taskID, "/reopen")
	if err != nil {
		return err
	}
	resp, err := APIDo(cfg, creds, "POST", path, map[string]string{
		"reason": reason,
	})
	if err != nil {
		return err
	}
	resp.Body.Close()

	fmt.Printf("✓ Task %s reopened\n", taskID)
	return nil
}

type taskItem struct {
	TaskID      string `json:"task_id"`
	Status      string `json:"status"`
	AssignedTo  string `json:"assigned_to"`
	IssuedBy    string `json:"issued_by"`
	Content     string `json:"content"`
	Result      string `json:"result"`
	IssuedAt    string `json:"issued_at"`
	CompletedAt string `json:"completed_at"`
}

func RunTaskList() error {
	cfg, creds, err := LoadAuth()
	if err != nil {
		return err
	}

	session, err := FindCurrentSession()
	if err != nil {
		return err
	}

	path, err := TeamPath(cfg, "/tasks")
	if err != nil {
		return err
	}
	resp, err := APIDoWithSession(cfg, creds, session, "GET", path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var result struct {
		Received []taskItem `json:"received"`
		Sent     []taskItem `json:"sent"`
	}
	json.NewDecoder(resp.Body).Decode(&result)

	if len(result.Received) == 0 && len(result.Sent) == 0 {
		fmt.Println("No active tasks")
		return nil
	}

	printGroup := func(label string, items []taskItem) {
		if len(items) == 0 {
			return
		}
		fmt.Printf("\n%s:\n", label)
		for _, t := range items {
			fmt.Printf("  %-12s  %-15s  %s", t.TaskID, t.Status, t.Content)
			if len(t.Content) > 30 {
				fmt.Printf("...")
			}
			if t.CompletedAt != "" {
				fmt.Printf("  (completed)")
			}
			fmt.Println()
		}
	}

	printGroup("Received tasks", result.Received)
	printGroup("Sent tasks", result.Sent)

	return nil
}

func RunTaskStatus(taskID string) error {
	cfg, creds, err := LoadAuth()
	if err != nil {
		return err
	}

	path, err := taskActionPath(cfg, taskID, "")
	if err != nil {
		return err
	}
	resp, err := APIDo(cfg, creds, "GET", path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var result taskItem
	json.NewDecoder(resp.Body).Decode(&result)

	fmt.Printf("Task:      %s\n", result.TaskID)
	fmt.Printf("Status:    %s\n", result.Status)
	fmt.Printf("Assigned:  %s\n", result.AssignedTo)
	fmt.Printf("Issued by: %s\n", result.IssuedBy)
	fmt.Printf("Content:   %s\n", result.Content)
	if result.Result != "" {
		fmt.Printf("Result:    %s\n", result.Result)
	}
	fmt.Printf("Issued:    %s\n", result.IssuedAt)
	if result.CompletedAt != "" {
		fmt.Printf("Completed: %s\n", result.CompletedAt)
	}

	return nil
}

// taskActionPath builds a team-scoped task path for the given task id and action
// suffix (e.g. "/result", "/resume", or "" for status).
func taskActionPath(cfg *AgentConfig, taskID, action string) (string, error) {
	return TeamPath(cfg, "/tasks/"+url.PathEscape(taskID)+action)
}
