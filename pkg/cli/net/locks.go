package api

import (
	"encoding/json"
	"fmt"
	"net/url"
	"time"
)

func RunLockAcquire(project, path string) error {
	cfg, creds, err := LoadAuth()
	if err != nil {
		return err
	}

	session, err := FindCurrentSession()
	if err != nil {
		return err
	}

	endpoint, err := TeamPath(cfg, "/locks/acquire")
	if err != nil {
		return err
	}
	resp, err := APIDoWithSession(cfg, creds, session, "POST", endpoint, map[string]string{
		"project_id": project,
		"path":       path,
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var result LockResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("cannot parse response: %w", err)
	}

	fmt.Printf("✓ locked %s:%s as %s\n", project, path, result.Owner.Label)
	return nil
}

func RunLockRelease(project, path string) error {
	cfg, creds, err := LoadAuth()
	if err != nil {
		return err
	}

	session, err := FindCurrentSession()
	if err != nil {
		return err
	}

	endpoint, err := TeamPath(cfg, "/locks/release")
	if err != nil {
		return err
	}
	resp, err := APIDoWithSession(cfg, creds, session, "POST", endpoint, map[string]string{
		"project_id": project,
		"path":       path,
	})
	if err != nil {
		return err
	}
	resp.Body.Close()

	fmt.Printf("✓ released %s:%s\n", project, path)
	return nil
}

func RunLockList(project string) error {
	cfg, creds, err := LoadAuth()
	if err != nil {
		return err
	}

	suffix := "/locks?project_id=" + url.QueryEscape(project)
	path, err := TeamPath(cfg, suffix)
	if err != nil {
		return err
	}
	resp, err := APIDo(cfg, creds, "GET", path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var result LockListResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("cannot parse response: %w", err)
	}

	if len(result.Locks) == 0 {
		fmt.Println("No locks")
		return nil
	}

	for i, l := range result.Locks {
		if i > 0 {
			fmt.Println("---")
		}
		fmt.Printf("Path:       %s\n", l.Path)
		fmt.Printf("Owner:      %s\n", l.Owner.Label)
		fmt.Printf("Acquired:   %s\n", time.Unix(l.AcquiredAt, 0).UTC().Format(time.RFC3339))
		fmt.Printf("Expires:    %s\n", time.Unix(l.LeaseExpiresAt, 0).UTC().Format(time.RFC3339))
	}

	return nil
}

// lockOwner is the structured, non-secret identity of a lock holder returned by
// the v2 API.
type lockOwner struct {
	UserID      string `json:"user_id"`
	Username    string `json:"username"`
	DeviceID    string `json:"device_id"`
	DeviceName  string `json:"device_name"`
	SessionName string `json:"session_name"`
	Label       string `json:"label"`
}

type LockResult struct {
	Owner lockOwner `json:"owner"`
}

type lockInfo struct {
	Path           string    `json:"path"`
	Owner          lockOwner `json:"owner"`
	AcquiredAt     int64     `json:"acquired_at"`
	LeaseExpiresAt int64     `json:"lease_expires_at"`
}

type LockListResult struct {
	Locks []lockInfo `json:"locks"`
}
