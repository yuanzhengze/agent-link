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

func doTree(t *testing.T, apiKey, project string) (*http.Response, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("GET", ts.URL+"/projects/"+project+"/tree", nil)
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

func doSnapshot(t *testing.T, apiKey, project string) (*http.Response, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("GET", ts.URL+"/projects/"+project+"/snapshot", nil)
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

// treeFilesByPath decodes a tree response's "files" array into a map keyed
// by path, for convenient per-file assertions.
func treeFilesByPath(t *testing.T, m map[string]any) map[string]map[string]any {
	t.Helper()
	raw, _ := m["files"].([]any)
	out := make(map[string]map[string]any, len(raw))
	for _, f := range raw {
		fm, ok := f.(map[string]any)
		if !ok {
			t.Fatalf("expected file entry to be an object, got %T (%v)", f, f)
		}
		path, _ := fm["path"].(string)
		out[path] = fm
	}
	return out
}

func TestTree_listsFilesWithLocks(t *testing.T) {
	apiKey := registerProjectTestDevice(t, "tree-owner-1")
	projectID := createTestProject(t, apiKey, "tree-test-1")

	t.Cleanup(func() {
		ctx := context.Background()
		testRdb.Del(ctx, "agentlink:lock:"+projectID+":page.html")
		testRdb.Del(ctx, "agentlink:lock:"+projectID+":about.html")
		testRdb.Del(ctx, "agentlink:locks:tree-owner-1:main")
	})

	// page.html: lock acquired and never released -> still held at tree time.
	respAcq1, _ := doLockAcquire(t, apiKey, projectID, "main", "page.html", "")
	if respAcq1.StatusCode != http.StatusOK {
		t.Fatalf("setup: expected 200 acquiring lock on page.html, got %d", respAcq1.StatusCode)
	}
	respApply1, _ := doApply(t, apiKey, projectID, "main", "page.html", "<h1>page</h1>")
	if respApply1.StatusCode != http.StatusOK {
		t.Fatalf("setup: expected 200 applying page.html, got %d", respApply1.StatusCode)
	}

	// about.html: lock acquired, applied, then released -> free at tree time.
	respAcq2, _ := doLockAcquire(t, apiKey, projectID, "main", "about.html", "")
	if respAcq2.StatusCode != http.StatusOK {
		t.Fatalf("setup: expected 200 acquiring lock on about.html, got %d", respAcq2.StatusCode)
	}
	respApply2, _ := doApply(t, apiKey, projectID, "main", "about.html", "<h1>about</h1>")
	if respApply2.StatusCode != http.StatusOK {
		t.Fatalf("setup: expected 200 applying about.html, got %d", respApply2.StatusCode)
	}
	respRel, _ := doLockRelease(t, apiKey, projectID, "main", "about.html", false)
	if respRel.StatusCode != http.StatusOK {
		t.Fatalf("setup: expected 200 releasing lock on about.html, got %d", respRel.StatusCode)
	}

	resp, m := doTree(t, apiKey, projectID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%+v", resp.StatusCode, m)
	}

	byPath := treeFilesByPath(t, m)
	wantOwner := "tree-owner-1:main"

	seed, ok := byPath["index.html"]
	if !ok {
		t.Fatalf("expected index.html in tree, got %+v", byPath)
	}
	if locked, _ := seed["locked"].(bool); locked {
		t.Errorf("expected index.html locked=false, got %v", seed["locked"])
	}

	page, ok := byPath["page.html"]
	if !ok {
		t.Fatalf("expected page.html in tree, got %+v", byPath)
	}
	if locked, _ := page["locked"].(bool); !locked {
		t.Errorf("expected page.html locked=true, got %v", page["locked"])
	}
	if owner, _ := page["owner"].(string); owner != wantOwner {
		t.Errorf("expected page.html owner=%s, got %v", wantOwner, page["owner"])
	}

	about, ok := byPath["about.html"]
	if !ok {
		t.Fatalf("expected about.html in tree, got %+v", byPath)
	}
	if locked, _ := about["locked"].(bool); locked {
		t.Errorf("expected about.html locked=false, got %v", about["locked"])
	}
	if owner, _ := about["owner"].(string); owner != "" {
		t.Errorf("expected about.html owner empty, got %v", about["owner"])
	}
}

func TestTree_skipsGitDir(t *testing.T) {
	apiKey := registerProjectTestDevice(t, "tree-owner-2")
	projectID := createTestProject(t, apiKey, "tree-test-2")

	resp, m := doTree(t, apiKey, projectID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%+v", resp.StatusCode, m)
	}

	byPath := treeFilesByPath(t, m)
	for path := range byPath {
		if path == ".git" || strings.HasPrefix(path, ".git/") {
			t.Errorf("expected no .git paths in tree, found %q", path)
		}
	}
}

func TestSnapshot_returnsAllContents(t *testing.T) {
	apiKey := registerProjectTestDevice(t, "snap-owner-1")
	projectID := createTestProject(t, apiKey, "snap-test-1")

	t.Cleanup(func() {
		ctx := context.Background()
		testRdb.Del(ctx, "agentlink:lock:"+projectID+":data.html")
		testRdb.Del(ctx, "agentlink:locks:snap-owner-1:main")
	})

	respAcq, _ := doLockAcquire(t, apiKey, projectID, "main", "data.html", "")
	if respAcq.StatusCode != http.StatusOK {
		t.Fatalf("setup: expected 200 acquiring lock on data.html, got %d", respAcq.StatusCode)
	}
	respApply, _ := doApply(t, apiKey, projectID, "main", "data.html", "<h1>data</h1>")
	if respApply.StatusCode != http.StatusOK {
		t.Fatalf("setup: expected 200 applying data.html, got %d", respApply.StatusCode)
	}

	resp, m := doSnapshot(t, apiKey, projectID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%+v", resp.StatusCode, m)
	}

	headCommit, _ := m["head_commit"].(string)
	if headCommit == "" {
		t.Error("expected non-empty head_commit")
	}

	filesRaw, _ := m["files"].([]any)
	byPath := make(map[string]string, len(filesRaw))
	for _, f := range filesRaw {
		fm, ok := f.(map[string]any)
		if !ok {
			t.Fatalf("expected file entry to be an object, got %T (%v)", f, f)
		}
		path, _ := fm["path"].(string)
		content, _ := fm["content"].(string)
		byPath[path] = content
	}

	dir := filepath.Join(testDataDir, "work", projectID)
	onDisk, err := os.ReadFile(filepath.Join(dir, "data.html"))
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := byPath["data.html"]; !ok || got != string(onDisk) {
		t.Errorf("expected data.html content=%q, got %q (present=%v)", string(onDisk), got, ok)
	}

	seedOnDisk, err := os.ReadFile(filepath.Join(dir, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := byPath["index.html"]; !ok || got != string(seedOnDisk) {
		t.Errorf("expected index.html content to match disk, got %q (present=%v)", got, ok)
	}
}

func TestTree_missingProject_404(t *testing.T) {
	apiKey := registerProjectTestDevice(t, "tree-owner-404")

	resp, m := doTree(t, apiKey, "does-not-exist")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d, body=%+v", resp.StatusCode, m)
	}
}

func TestSnapshot_missingProject_404(t *testing.T) {
	apiKey := registerProjectTestDevice(t, "snap-owner-404")

	resp, m := doSnapshot(t, apiKey, "does-not-exist")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d, body=%+v", resp.StatusCode, m)
	}
}
