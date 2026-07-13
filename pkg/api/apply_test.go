package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// mockBroadcaster records every Broadcast call for assertions. Task 4 will
// implement the real Hub; this stands in for it in apply tests.
type mockBroadcaster struct {
	mu     sync.Mutex
	events []Event
}

func (m *mockBroadcaster) Broadcast(project string, ev Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, ev)
}

func (m *mockBroadcaster) all() []Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Event, len(m.events))
	copy(out, m.events)
	return out
}

// createTestProject creates a project via the real HTTP endpoint and
// registers cleanup, returning the new project id.
func createTestProject(t *testing.T, apiKey, name string) string {
	t.Helper()
	body := fmt.Sprintf(`{"name":%q}`, name)
	req, _ := http.NewRequest("POST", ts.URL+"/projects", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("setup: failed to create project, status %d", resp.StatusCode)
	}
	var pr ProjectResponse
	json.NewDecoder(resp.Body).Decode(&pr)

	t.Cleanup(func() {
		ctx := context.Background()
		testRdb.Del(ctx, "agentlink:project:"+pr.ID)
		testRdb.SRem(ctx, "agentlink:projects", pr.ID)
	})

	return pr.ID
}

func doApply(t *testing.T, apiKey, project, session, path, content string) (*http.Response, map[string]any) {
	t.Helper()
	body := fmt.Sprintf(`{"session":%q,"path":%q,"content":%q}`, session, path, content)
	req, _ := http.NewRequest("POST", ts.URL+"/projects/"+project+"/apply", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.NewDecoder(resp.Body).Decode(&m)
	resp.Body.Close()
	return resp, m
}

// gitLogCount returns the number of commits in dir's git history.
func gitLogCount(t *testing.T, dir string) int {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "log", "--oneline").Output()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return 0
	}
	return len(strings.Split(trimmed, "\n"))
}

func TestApply_success(t *testing.T) {
	mock := &mockBroadcaster{}
	old := testSrv.hub
	testSrv.hub = mock
	t.Cleanup(func() { testSrv.hub = old })

	apiKey := registerProjectTestDevice(t, "apply-owner-1")
	projectID := createTestProject(t, apiKey, "apply-test-1")

	t.Cleanup(func() {
		ctx := context.Background()
		testRdb.Del(ctx, "agentlink:lock:"+projectID+":page.html")
		testRdb.Del(ctx, "agentlink:locks:apply-owner-1:main")
	})

	resp, _ := doLockAcquire(t, apiKey, projectID, "main", "page.html", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("setup: expected 200 acquiring lock, got %d", resp.StatusCode)
	}

	dir := filepath.Join(testDataDir, "work", projectID)
	beforeCount := gitLogCount(t, dir)

	resp2, m := doApply(t, apiKey, projectID, "main", "page.html", "<h1>hi</h1>")
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%+v", resp2.StatusCode, m)
	}
	headCommit, _ := m["head_commit"].(string)
	if headCommit == "" {
		t.Fatal("expected non-empty head_commit")
	}

	// File written to disk.
	data, err := os.ReadFile(filepath.Join(dir, "page.html"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "<h1>hi</h1>" {
		t.Errorf("expected file content <h1>hi</h1>, got %q", string(data))
	}

	// git log gained exactly one commit.
	afterCount := gitLogCount(t, dir)
	if afterCount != beforeCount+1 {
		t.Errorf("expected git log to gain 1 commit, before=%d after=%d", beforeCount, afterCount)
	}

	// head_commit updated in Redis.
	ctx := context.Background()
	redisHead, err := testRdb.HGet(ctx, "agentlink:project:"+projectID, "head_commit").Result()
	if err != nil {
		t.Fatal(err)
	}
	if redisHead != headCommit {
		t.Errorf("expected redis head_commit=%s, got %s", headCommit, redisHead)
	}

	// mock received the file_changed event with content.
	events := mock.all()
	if len(events) != 1 {
		t.Fatalf("expected 1 broadcast event, got %d", len(events))
	}
	ev := events[0]
	if ev.Type != "file_changed" {
		t.Errorf("expected type=file_changed, got %s", ev.Type)
	}
	if ev.Project != projectID {
		t.Errorf("expected project=%s, got %s", projectID, ev.Project)
	}
	if ev.Path != "page.html" {
		t.Errorf("expected path=page.html, got %s", ev.Path)
	}
	if ev.Content != "<h1>hi</h1>" {
		t.Errorf("expected content=<h1>hi</h1>, got %q", ev.Content)
	}
	if ev.HeadCommit != headCommit {
		t.Errorf("expected head_commit=%s, got %s", headCommit, ev.HeadCommit)
	}
	if ev.By != "apply-owner-1:main" {
		t.Errorf("expected by=apply-owner-1:main, got %s", ev.By)
	}
	if ev.At == "" {
		t.Error("expected non-empty at timestamp")
	}
}

func TestApply_notLocked_409(t *testing.T) {
	apiKey := registerProjectTestDevice(t, "apply-owner-2")
	projectID := createTestProject(t, apiKey, "apply-test-2")

	resp, m := doApply(t, apiKey, projectID, "main", "unlocked.html", "content")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409, got %d", resp.StatusCode)
	}
	if owner, _ := m["owner"].(string); owner != "" {
		t.Errorf("expected empty owner in conflict body (lock is free), got %q", owner)
	}

	// File must not have been written.
	dir := filepath.Join(testDataDir, "work", projectID)
	if _, err := os.Stat(filepath.Join(dir, "unlocked.html")); err == nil {
		t.Error("expected unlocked.html to not exist after rejected apply")
	}
}

func TestApply_lockedByOther_409(t *testing.T) {
	apiKeyA := registerProjectTestDevice(t, "apply-owner-3a")
	apiKeyB := registerProjectTestDevice(t, "apply-owner-3b")
	projectID := createTestProject(t, apiKeyA, "apply-test-3")

	t.Cleanup(func() {
		ctx := context.Background()
		testRdb.Del(ctx, "agentlink:lock:"+projectID+":shared.html")
		testRdb.Del(ctx, "agentlink:locks:apply-owner-3a:main")
	})

	resp, _ := doLockAcquire(t, apiKeyA, projectID, "main", "shared.html", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("setup: expected 200 acquiring lock for A, got %d", resp.StatusCode)
	}

	resp2, m := doApply(t, apiKeyB, projectID, "main", "shared.html", "hacked")
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409, got %d", resp2.StatusCode)
	}
	wantOwner := "apply-owner-3a:main"
	if owner, _ := m["owner"].(string); owner != wantOwner {
		t.Errorf("expected owner=%s, got %v", wantOwner, m["owner"])
	}
}

func TestApply_pathTraversal_400(t *testing.T) {
	apiKey := registerProjectTestDevice(t, "apply-owner-4")
	projectID := createTestProject(t, apiKey, "apply-test-4")

	resp, _ := doApply(t, apiKey, projectID, "main", "../x", "malicious")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestApply_concurrentDifferentFiles(t *testing.T) {
	apiKeyA := registerProjectTestDevice(t, "apply-owner-5a")
	apiKeyB := registerProjectTestDevice(t, "apply-owner-5b")
	projectID := createTestProject(t, apiKeyA, "apply-test-5")

	t.Cleanup(func() {
		ctx := context.Background()
		testRdb.Del(ctx, "agentlink:lock:"+projectID+":a.html")
		testRdb.Del(ctx, "agentlink:lock:"+projectID+":b.html")
		testRdb.Del(ctx, "agentlink:locks:apply-owner-5a:main")
		testRdb.Del(ctx, "agentlink:locks:apply-owner-5b:main")
	})

	resp, _ := doLockAcquire(t, apiKeyA, projectID, "main", "a.html", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("setup: expected 200 acquiring lock a for A, got %d", resp.StatusCode)
	}
	resp2, _ := doLockAcquire(t, apiKeyB, projectID, "main", "b.html", "")
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("setup: expected 200 acquiring lock b for B, got %d", resp2.StatusCode)
	}

	dir := filepath.Join(testDataDir, "work", projectID)
	beforeCount := gitLogCount(t, dir)

	var wg sync.WaitGroup
	results := make([]int, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		r, _ := doApply(t, apiKeyA, projectID, "main", "a.html", "content-a")
		results[0] = r.StatusCode
	}()
	go func() {
		defer wg.Done()
		r, _ := doApply(t, apiKeyB, projectID, "main", "b.html", "content-b")
		results[1] = r.StatusCode
	}()
	wg.Wait()

	if results[0] != http.StatusOK {
		t.Errorf("expected 200 for A, got %d", results[0])
	}
	if results[1] != http.StatusOK {
		t.Errorf("expected 200 for B, got %d", results[1])
	}

	afterCount := gitLogCount(t, dir)
	if afterCount != beforeCount+2 {
		t.Errorf("expected git log to gain 2 commits, before=%d after=%d", beforeCount, afterCount)
	}
}
