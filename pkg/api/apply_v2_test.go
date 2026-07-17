package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
)

func applyV2Web(t *testing.T, session *http.Response, teamID, projectID string, body map[string]any) (*http.Response, []byte) {
	t.Helper()
	return teamJSON(t, http.MethodPost, "/api/teams/"+teamID+"/projects/"+projectID+"/apply", body, session, nil)
}

func TestV2ApplyRequiresActorLock(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	session, _ := registerTeamUser(t, "applylock")
	team, _ := createTeamHTTP(t, session, "Apply Lock Team")
	teamID := team["id"].(string)
	project := createProjectV2HTTP(t, session, teamID, "Apply Lock Project")

	// Applying without holding the lock is rejected.
	resp, body := applyV2Web(t, session, teamID, project.ID, map[string]any{
		"path":    "index.html",
		"content": "<h1>nope</h1>",
	})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("apply without lock expected 409, got %d body=%s", resp.StatusCode, body)
	}

	// Acquire the lock, then apply succeeds.
	if lockResp, lockBody := acquireLockWebV2(t, session, teamID, map[string]any{
		"project_id": project.ID,
		"path":       "index.html",
	}); lockResp.StatusCode != http.StatusOK {
		t.Fatalf("acquire expected 200, got %d body=%s", lockResp.StatusCode, lockBody)
	}
	okResp, okBody := applyV2Web(t, session, teamID, project.ID, map[string]any{
		"path":    "index.html",
		"content": "<h1>yes</h1>",
	})
	if okResp.StatusCode != http.StatusOK {
		t.Fatalf("apply with lock expected 200, got %d body=%s", okResp.StatusCode, okBody)
	}
}

func TestV2ApplyRejectsCrossTeamProjectWith404(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	owner, _ := registerTeamUser(t, "applycrossown")
	teamA, _ := createTeamHTTP(t, owner, "Apply Cross A")
	teamAID := teamA["id"].(string)
	project := createProjectV2HTTP(t, owner, teamAID, "Apply Cross Project")

	outsider, _ := registerTeamUser(t, "applycrossout")
	teamB, _ := createTeamHTTP(t, outsider, "Apply Cross B")
	teamBID := teamB["id"].(string)

	resp, body := applyV2Web(t, outsider, teamBID, project.ID, map[string]any{
		"path":    "index.html",
		"content": "<h1>hijack</h1>",
	})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-team apply expected 404, got %d body=%s", resp.StatusCode, body)
	}
}

func TestV2ApplyCommitsAndUpdatesTeamProjectHead(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	session, _ := registerTeamUser(t, "applyhead")
	team, _ := createTeamHTTP(t, session, "Apply Head Team")
	teamID := team["id"].(string)
	project := createProjectV2HTTP(t, session, teamID, "Apply Head Project")

	beforeHead := authV2Rdb.HGet(context.Background(), projectV2Key(project.ID), "head_commit").Val()

	if lockResp, lockBody := acquireLockWebV2(t, session, teamID, map[string]any{
		"project_id": project.ID,
		"path":       "page.html",
	}); lockResp.StatusCode != http.StatusOK {
		t.Fatalf("acquire expected 200, got %d body=%s", lockResp.StatusCode, lockBody)
	}
	resp, body := applyV2Web(t, session, teamID, project.ID, map[string]any{
		"path":    "page.html",
		"content": "<h1>hello</h1>",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("apply expected 200, got %d body=%s", resp.StatusCode, body)
	}
	var applied ApplyResponseV2
	if err := json.Unmarshal(body, &applied); err != nil {
		t.Fatal(err)
	}
	if !validGitObjectID(applied.HeadCommit) {
		t.Fatalf("apply head_commit invalid: %q", applied.HeadCommit)
	}
	if applied.HeadCommit == beforeHead {
		t.Fatalf("apply head_commit did not advance from %q", beforeHead)
	}

	stored := authV2Rdb.HGet(context.Background(), projectV2Key(project.ID), "head_commit").Val()
	if stored != applied.HeadCommit {
		t.Fatalf("stored head_commit = %q; want %q", stored, applied.HeadCommit)
	}
	dir := authV2Srv.projectDirV2(teamID, project.ID)
	content, err := os.ReadFile(filepath.Join(dir, "page.html"))
	if err != nil || string(content) != "<h1>hello</h1>" {
		t.Fatalf("applied file content = %q err=%v", content, err)
	}
}

func TestV2TreeShowsStructuredLockOwner(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	session, userResult := registerTeamUser(t, "applytree")
	team, _ := createTeamHTTP(t, session, "Tree Team")
	teamID := team["id"].(string)
	project := createProjectV2HTTP(t, session, teamID, "Tree Project")
	username := userResult["user"].(map[string]any)["username"].(string)

	if lockResp, lockBody := acquireLockWebV2(t, session, teamID, map[string]any{
		"project_id": project.ID,
		"path":       "index.html",
	}); lockResp.StatusCode != http.StatusOK {
		t.Fatalf("acquire expected 200, got %d body=%s", lockResp.StatusCode, lockBody)
	}

	resp, body := teamJSON(t, http.MethodGet, "/api/teams/"+teamID+"/projects/"+project.ID+"/tree", nil, session, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tree expected 200, got %d body=%s", resp.StatusCode, body)
	}
	var tree TreeResponseV2
	if err := json.Unmarshal(body, &tree); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, entry := range tree.Files {
		if entry.Path != "index.html" {
			continue
		}
		found = true
		if !entry.Locked || entry.Owner == nil {
			t.Fatalf("index.html should be locked with owner: %+v", entry)
		}
		if entry.Owner.Username != username || entry.Owner.SessionName != "gui" {
			t.Fatalf("tree owner = %+v; want user %q session gui", entry.Owner, username)
		}
	}
	if !found {
		t.Fatalf("index.html missing from tree: %s", body)
	}
}

func TestV2SnapshotIsAtomicWithApply(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	session, _ := registerTeamUser(t, "applysnap")
	team, _ := createTeamHTTP(t, session, "Snapshot Team")
	teamID := team["id"].(string)
	project := createProjectV2HTTP(t, session, teamID, "Snapshot Project")

	if lockResp, lockBody := acquireLockWebV2(t, session, teamID, map[string]any{
		"project_id": project.ID,
		"path":       "counter.txt",
	}); lockResp.StatusCode != http.StatusOK {
		t.Fatalf("acquire expected 200, got %d body=%s", lockResp.StatusCode, lockBody)
	}

	const iterations = 12
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			resp, body := applyV2Web(t, session, teamID, project.ID, map[string]any{
				"path":    "counter.txt",
				"content": strconv.Itoa(i),
			})
			if resp.StatusCode != http.StatusOK {
				t.Errorf("apply %d expected 200, got %d body=%s", i, resp.StatusCode, body)
				return
			}
		}
	}()

	for i := 0; i < iterations*2; i++ {
		resp, body := teamJSON(t, http.MethodGet, "/api/teams/"+teamID+"/projects/"+project.ID+"/snapshot", nil, session, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("snapshot expected 200, got %d body=%s", resp.StatusCode, body)
		}
		var snap SnapshotResponseV2
		if err := json.Unmarshal(body, &snap); err != nil {
			t.Fatal(err)
		}
		if !validGitObjectID(snap.HeadCommit) {
			t.Fatalf("snapshot head_commit invalid: %q", snap.HeadCommit)
		}
		for _, f := range snap.Files {
			if f.Path == "counter.txt" {
				// The committed content must always be a fully-written integer,
				// never a partial/torn read.
				if _, err := strconv.Atoi(f.Content); err != nil {
					t.Fatalf("snapshot saw torn counter content %q: %v", f.Content, err)
				}
			}
		}
	}
	wg.Wait()
}

func TestV2ApplyRejectsCaseInsensitiveGitTraversal(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	session, _ := registerTeamUser(t, "applygit")
	team, _ := createTeamHTTP(t, session, "Apply Git Team")
	teamID := team["id"].(string)
	project := createProjectV2HTTP(t, session, teamID, "Apply Git Project")

	for _, path := range []string{".GIT/hooks/pre-commit", "../escape.txt", ".git/config"} {
		resp, body := applyV2Web(t, session, teamID, project.ID, map[string]any{
			"path":    path,
			"content": "x",
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("apply path %q expected 400, got %d body=%s", path, resp.StatusCode, body)
		}
	}
}
