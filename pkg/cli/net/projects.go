package api

import (
	"encoding/json"
	"fmt"
)

func RunProjectCreate(name string) error {
	cfg, creds, err := LoadAuth()
	if err != nil {
		return err
	}

	path, err := TeamPath(cfg, "/projects")
	if err != nil {
		return err
	}
	resp, err := APIDo(cfg, creds, "POST", path, map[string]string{"name": name})
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var result struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		HeadCommit string `json:"head_commit"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("cannot parse response: %w", err)
	}

	fmt.Printf("✓ Project created\n")
	fmt.Printf("ID:          %s\n", result.ID)
	fmt.Printf("Name:        %s\n", result.Name)
	fmt.Printf("Head commit: %s\n", result.HeadCommit)
	return nil
}

func RunProjectList() error {
	cfg, creds, err := LoadAuth()
	if err != nil {
		return err
	}

	path, err := TeamPath(cfg, "/projects")
	if err != nil {
		return err
	}
	resp, err := APIDo(cfg, creds, "GET", path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	type projectInfo struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		CreatedAt  string `json:"created_at"`
		HeadCommit string `json:"head_commit"`
	}
	var result struct {
		Projects []projectInfo `json:"projects"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("cannot parse response: %w", err)
	}

	if len(result.Projects) == 0 {
		fmt.Println("No projects")
		return nil
	}

	for i, p := range result.Projects {
		if i > 0 {
			fmt.Println("---")
		}
		fmt.Printf("ID:          %s\n", p.ID)
		fmt.Printf("Name:        %s\n", p.Name)
		fmt.Printf("Created:     %s\n", p.CreatedAt)
		fmt.Printf("Head commit: %s\n", p.HeadCommit)
	}

	return nil
}
