package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// registerProjectTestDevice registers a throwaway device for project tests
// and returns its API key. Cleans up the device record on test completion.
func registerProjectTestDevice(t *testing.T, device string) string {
	t.Helper()
	body := fmt.Sprintf(`{"device":%q,"sessions":["main"],"register_password":"test-password"}`, device)
	resp, err := http.Post(ts.URL+"/agents/register", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("failed to register test device %s: status %d", device, resp.StatusCode)
	}

	var rr RegisterResponse
	json.NewDecoder(resp.Body).Decode(&rr)

	t.Cleanup(func() {
		ctx := context.Background()
		hash, _ := testRdb.HGet(ctx, "agentlink:device:"+device, "api_key_hash").Result()
		testRdb.Del(ctx, "agentlink:device:"+device)
		if hash != "" {
			testRdb.Del(ctx, "agentlink:api_key:"+hash)
		}
	})

	return rr.APIKey
}

func TestCreateProject_ok(t *testing.T) {
	apiKey := registerProjectTestDevice(t, "proj-owner-1")

	body := `{"name":"demo prototype"}`
	req, _ := http.NewRequest("POST", ts.URL+"/projects", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var pr ProjectResponse
	json.NewDecoder(resp.Body).Decode(&pr)

	if pr.ID == "" {
		t.Fatal("expected non-empty id")
	}
	if len(pr.ID) != 8 {
		t.Errorf("expected id length 8, got %d (%q)", len(pr.ID), pr.ID)
	}
	if pr.Name != "demo prototype" {
		t.Errorf("expected name=demo prototype, got %q", pr.Name)
	}
	if pr.HeadCommit == "" {
		t.Error("expected non-empty head_commit")
	}

	t.Cleanup(func() {
		ctx := context.Background()
		testRdb.Del(ctx, "agentlink:project:"+pr.ID)
		testRdb.SRem(ctx, "agentlink:projects", pr.ID)
	})

	// Disk assertions: git work tree with a first commit + seed file.
	dir := filepath.Join(testDataDir, "work", pr.ID)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Errorf("expected .git dir at %s: %v", dir, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "index.html")); err != nil {
		t.Errorf("expected index.html at %s: %v", dir, err)
	}

	// Redis assertions.
	ctx := context.Background()
	data, err := testRdb.HGetAll(ctx, "agentlink:project:"+pr.ID).Result()
	if err != nil {
		t.Fatal(err)
	}
	if data["id"] != pr.ID {
		t.Errorf("redis id mismatch: got %q, want %q", data["id"], pr.ID)
	}
	if data["name"] != "demo prototype" {
		t.Errorf("redis name mismatch: got %q", data["name"])
	}
	if data["created_at"] == "" {
		t.Error("redis created_at should not be empty")
	}
	if data["head_commit"] != pr.HeadCommit {
		t.Errorf("redis head_commit mismatch: got %q, want %q", data["head_commit"], pr.HeadCommit)
	}

	isMember, err := testRdb.SIsMember(ctx, "agentlink:projects", pr.ID).Result()
	if err != nil {
		t.Fatal(err)
	}
	if !isMember {
		t.Error("expected project id to be a member of agentlink:projects set")
	}
}

func TestCreateProject_missingName_400(t *testing.T) {
	apiKey := registerProjectTestDevice(t, "proj-owner-2")

	body := `{}`
	req, _ := http.NewRequest("POST", ts.URL+"/projects", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestListProjects(t *testing.T) {
	apiKey := registerProjectTestDevice(t, "proj-owner-3")

	var ids []string
	for i := 0; i < 2; i++ {
		body := fmt.Sprintf(`{"name":"list-test-%d"}`, i)
		req, _ := http.NewRequest("POST", ts.URL+"/projects", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("setup: expected 200 creating project %d, got %d", i, resp.StatusCode)
		}
		var pr ProjectResponse
		json.NewDecoder(resp.Body).Decode(&pr)
		resp.Body.Close()
		ids = append(ids, pr.ID)
	}

	t.Cleanup(func() {
		ctx := context.Background()
		for _, id := range ids {
			testRdb.Del(ctx, "agentlink:project:"+id)
			testRdb.SRem(ctx, "agentlink:projects", id)
		}
	})

	req, _ := http.NewRequest("GET", ts.URL+"/projects", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var lr ListProjectsResponse
	json.NewDecoder(resp.Body).Decode(&lr)

	if len(lr.Projects) != 2 {
		t.Fatalf("expected 2 projects, got %d", len(lr.Projects))
	}

	found := map[string]bool{}
	for _, p := range lr.Projects {
		found[p.ID] = true
		if p.HeadCommit == "" {
			t.Errorf("expected non-empty head_commit for project %s", p.ID)
		}
		if p.CreatedAt == "" {
			t.Errorf("expected non-empty created_at for project %s", p.ID)
		}
	}
	for _, id := range ids {
		if !found[id] {
			t.Errorf("expected created project %s to appear in list", id)
		}
	}
}
