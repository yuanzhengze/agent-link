package api

import (
	"encoding/json"
	"fmt"
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

	resp, err := APIDo(cfg, creds, "POST", "/locks/acquire", map[string]string{
		"project": project,
		"session": session,
		"path":    path,
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var result struct {
		Owner string `json:"owner"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("cannot parse response: %w", err)
	}

	fmt.Printf("✓ locked %s:%s as %s\n", project, path, result.Owner)
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

	resp, err := APIDo(cfg, creds, "POST", "/locks/release", map[string]string{
		"project": project,
		"session": session,
		"path":    path,
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

	path := fmt.Sprintf("/locks/list?project=%s", project)
	resp, err := APIDo(cfg, creds, "GET", path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	type lockInfo struct {
		Path           string `json:"path"`
		Owner          string `json:"owner"`
		AcquiredAt     int64  `json:"acquired_at"`
		LeaseExpiresAt int64  `json:"lease_expires_at"`
	}
	var result struct {
		Locks []lockInfo `json:"locks"`
	}
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
		fmt.Printf("Owner:      %s\n", l.Owner)
		fmt.Printf("Acquired:   %s\n", time.Unix(l.AcquiredAt, 0).UTC().Format(time.RFC3339))
		fmt.Printf("Expires:    %s\n", time.Unix(l.LeaseExpiresAt, 0).UTC().Format(time.RFC3339))
	}

	return nil
}
