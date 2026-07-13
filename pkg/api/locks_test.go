package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func doLockAcquire(t *testing.T, apiKey, project, session, path, taskID string) (*http.Response, LockAcquireResponse) {
	t.Helper()
	body := fmt.Sprintf(`{"project":%q,"session":%q,"path":%q,"task_id":%q}`, project, session, path, taskID)
	req, _ := http.NewRequest("POST", ts.URL+"/locks/acquire", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var lr LockAcquireResponse
	json.NewDecoder(resp.Body).Decode(&lr)
	resp.Body.Close()
	return resp, lr
}

func doLockRelease(t *testing.T, apiKey, project, session, path string, force bool) (*http.Response, map[string]any) {
	t.Helper()
	body := fmt.Sprintf(`{"project":%q,"session":%q,"path":%q,"force":%v}`, project, session, path, force)
	req, _ := http.NewRequest("POST", ts.URL+"/locks/release", strings.NewReader(body))
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

func TestLockAcquire_free(t *testing.T) {
	apiKey := registerProjectTestDevice(t, "lock-owner-1")

	resp, lr := doLockAcquire(t, apiKey, "proj1", "main", "index.html", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	wantOwner := "lock-owner-1:main"
	if lr.Owner != wantOwner {
		t.Errorf("expected owner=%s, got %s", wantOwner, lr.Owner)
	}

	ctx := context.Background()
	t.Cleanup(func() {
		testRdb.Del(ctx, "agentlink:lock:proj1:index.html")
		testRdb.Del(ctx, "agentlink:locks:lock-owner-1:main")
	})

	data, err := testRdb.HGetAll(ctx, "agentlink:lock:proj1:index.html").Result()
	if err != nil {
		t.Fatal(err)
	}
	if data["owner"] != wantOwner {
		t.Errorf("redis owner mismatch: got %q, want %q", data["owner"], wantOwner)
	}
	if data["acquired_at"] == "" {
		t.Error("expected non-empty acquired_at")
	}
	if data["lease_expires_at"] == "" {
		t.Error("expected non-empty lease_expires_at")
	}

	isMember, err := testRdb.SIsMember(ctx, "agentlink:locks:lock-owner-1:main", "proj1|index.html").Result()
	if err != nil {
		t.Fatal(err)
	}
	if !isMember {
		t.Error("expected member proj1|index.html in locks set")
	}
}

func TestLockAcquire_heldByOther_409(t *testing.T) {
	apiKeyA := registerProjectTestDevice(t, "lock-a")
	apiKeyB := registerProjectTestDevice(t, "lock-b")

	ctx := context.Background()
	t.Cleanup(func() {
		testRdb.Del(ctx, "agentlink:lock:proj2:file2.html")
		testRdb.Del(ctx, "agentlink:locks:lock-a:main")
	})

	resp, _ := doLockAcquire(t, apiKeyA, "proj2", "main", "file2.html", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for A, got %d", resp.StatusCode)
	}

	resp2, lr2 := doLockAcquire(t, apiKeyB, "proj2", "main", "file2.html", "")
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 for B, got %d", resp2.StatusCode)
	}
	wantOwner := "lock-a:main"
	if lr2.Owner != wantOwner {
		t.Errorf("expected body.owner=%s, got %s", wantOwner, lr2.Owner)
	}
}

func TestLockAcquire_reacquire_refresh(t *testing.T) {
	apiKey := registerProjectTestDevice(t, "lock-c")

	ctx := context.Background()
	t.Cleanup(func() {
		testRdb.Del(ctx, "agentlink:lock:proj3:file3.html")
		testRdb.Del(ctx, "agentlink:locks:lock-c:main")
	})

	resp, _ := doLockAcquire(t, apiKey, "proj3", "main", "file3.html", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// Artificially lower the lease so a refresh is observable.
	lowExpiry := time.Now().Unix() + 5
	if err := testRdb.HSet(ctx, "agentlink:lock:proj3:file3.html", "lease_expires_at", lowExpiry).Err(); err != nil {
		t.Fatal(err)
	}

	resp2, _ := doLockAcquire(t, apiKey, "proj3", "main", "file3.html", "")
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on reacquire, got %d", resp2.StatusCode)
	}

	newExpiryStr, err := testRdb.HGet(ctx, "agentlink:lock:proj3:file3.html", "lease_expires_at").Result()
	if err != nil {
		t.Fatal(err)
	}
	newExpiry, _ := strconv.ParseInt(newExpiryStr, 10, 64)
	if newExpiry <= lowExpiry {
		t.Errorf("expected lease_expires_at to be refreshed beyond %d, got %d", lowExpiry, newExpiry)
	}
}

func TestLockAcquire_expired_reclaim(t *testing.T) {
	apiKeyA := registerProjectTestDevice(t, "lock-d")
	apiKeyB := registerProjectTestDevice(t, "lock-e")

	ctx := context.Background()
	t.Cleanup(func() {
		testRdb.Del(ctx, "agentlink:lock:proj4:file4.html")
		testRdb.Del(ctx, "agentlink:locks:lock-d:main")
		testRdb.Del(ctx, "agentlink:locks:lock-e:main")
	})

	resp, _ := doLockAcquire(t, apiKeyA, "proj4", "main", "file4.html", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for A, got %d", resp.StatusCode)
	}

	// Force the lease into the past.
	if err := testRdb.HSet(ctx, "agentlink:lock:proj4:file4.html", "lease_expires_at", time.Now().Unix()-10).Err(); err != nil {
		t.Fatal(err)
	}

	resp2, lr2 := doLockAcquire(t, apiKeyB, "proj4", "main", "file4.html", "")
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for B reclaiming expired lock, got %d", resp2.StatusCode)
	}
	wantOwner := "lock-e:main"
	if lr2.Owner != wantOwner {
		t.Errorf("expected owner=%s, got %s", wantOwner, lr2.Owner)
	}

	oldIsMember, err := testRdb.SIsMember(ctx, "agentlink:locks:lock-d:main", "proj4|file4.html").Result()
	if err != nil {
		t.Fatal(err)
	}
	if oldIsMember {
		t.Error("expected member to be removed from old owner's locks set after reclaim")
	}

	newIsMember, err := testRdb.SIsMember(ctx, "agentlink:locks:lock-e:main", "proj4|file4.html").Result()
	if err != nil {
		t.Fatal(err)
	}
	if !newIsMember {
		t.Error("expected member to be present in new owner's locks set after reclaim")
	}
}

func TestLockRelease_owner(t *testing.T) {
	apiKey := registerProjectTestDevice(t, "lock-f")

	ctx := context.Background()
	t.Cleanup(func() {
		testRdb.Del(ctx, "agentlink:lock:proj5:file5.html")
		testRdb.Del(ctx, "agentlink:locks:lock-f:main")
	})

	resp, _ := doLockAcquire(t, apiKey, "proj5", "main", "file5.html", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 acquiring, got %d", resp.StatusCode)
	}

	resp2, m := doLockRelease(t, apiKey, "proj5", "main", "file5.html", false)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 releasing, got %d", resp2.StatusCode)
	}
	if ok, _ := m["ok"].(bool); !ok {
		t.Errorf("expected ok=true, got %+v", m)
	}

	exists, err := testRdb.Exists(ctx, "agentlink:lock:proj5:file5.html").Result()
	if err != nil {
		t.Fatal(err)
	}
	if exists != 0 {
		t.Error("expected lock key to be deleted")
	}

	isMember, err := testRdb.SIsMember(ctx, "agentlink:locks:lock-f:main", "proj5|file5.html").Result()
	if err != nil {
		t.Fatal(err)
	}
	if isMember {
		t.Error("expected member to be removed from locks set")
	}
}

func TestLockRelease_nonOwner_409(t *testing.T) {
	apiKeyA := registerProjectTestDevice(t, "lock-g")
	apiKeyB := registerProjectTestDevice(t, "lock-h")

	ctx := context.Background()
	t.Cleanup(func() {
		testRdb.Del(ctx, "agentlink:lock:proj6:file6.html")
		testRdb.Del(ctx, "agentlink:locks:lock-g:main")
	})

	resp, _ := doLockAcquire(t, apiKeyA, "proj6", "main", "file6.html", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for A, got %d", resp.StatusCode)
	}

	resp2, m := doLockRelease(t, apiKeyB, "proj6", "main", "file6.html", false)
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 for non-owner release, got %d", resp2.StatusCode)
	}
	wantOwner := "lock-g:main"
	if owner, _ := m["owner"].(string); owner != wantOwner {
		t.Errorf("expected body.owner=%s, got %v", wantOwner, m["owner"])
	}

	exists, err := testRdb.Exists(ctx, "agentlink:lock:proj6:file6.html").Result()
	if err != nil {
		t.Fatal(err)
	}
	if exists == 0 {
		t.Error("expected lock to still exist after failed non-owner release")
	}
}

func TestLockRelease_force(t *testing.T) {
	apiKeyA := registerProjectTestDevice(t, "lock-i")
	apiKeyB := registerProjectTestDevice(t, "lock-j")

	ctx := context.Background()
	t.Cleanup(func() {
		testRdb.Del(ctx, "agentlink:lock:proj7:file7.html")
		testRdb.Del(ctx, "agentlink:locks:lock-i:main")
	})

	resp, _ := doLockAcquire(t, apiKeyA, "proj7", "main", "file7.html", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for A, got %d", resp.StatusCode)
	}

	resp2, m := doLockRelease(t, apiKeyB, "proj7", "main", "file7.html", true)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for forced release, got %d", resp2.StatusCode)
	}
	if ok, _ := m["ok"].(bool); !ok {
		t.Errorf("expected ok=true, got %+v", m)
	}

	exists, err := testRdb.Exists(ctx, "agentlink:lock:proj7:file7.html").Result()
	if err != nil {
		t.Fatal(err)
	}
	if exists != 0 {
		t.Error("expected lock key to be deleted after forced release")
	}

	isMember, err := testRdb.SIsMember(ctx, "agentlink:locks:lock-i:main", "proj7|file7.html").Result()
	if err != nil {
		t.Fatal(err)
	}
	if isMember {
		t.Error("expected member to be removed from real owner's locks set after force release")
	}
}

// TestLockAcquire_concurrent races 10 distinct owners against a single
// project/path lock. Atomicity must come from the Lua script (single Redis
// command execution), not from a Go-side mutex — run with -race to confirm
// no data races exist in the HTTP/Redis path itself.
func TestLockAcquire_concurrent(t *testing.T) {
	const n = 10
	apiKeys := make([]string, n)
	for i := 0; i < n; i++ {
		apiKeys[i] = registerProjectTestDevice(t, fmt.Sprintf("lock-race-%d", i))
	}

	ctx := context.Background()
	t.Cleanup(func() {
		testRdb.Del(ctx, "agentlink:lock:race-proj:race.html")
		for i := 0; i < n; i++ {
			testRdb.Del(ctx, fmt.Sprintf("agentlink:locks:lock-race-%d:main", i))
		}
	})

	var wg sync.WaitGroup
	var mu sync.Mutex
	statusCounts := map[int]int{}

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(apiKey string) {
			defer wg.Done()
			resp, _ := doLockAcquire(t, apiKey, "race-proj", "main", "race.html", "")
			mu.Lock()
			statusCounts[resp.StatusCode]++
			mu.Unlock()
		}(apiKeys[i])
	}
	wg.Wait()

	if statusCounts[http.StatusOK] != 1 {
		t.Errorf("expected exactly 1 winner (200), got %d; full counts: %+v", statusCounts[http.StatusOK], statusCounts)
	}
	if statusCounts[http.StatusConflict] != n-1 {
		t.Errorf("expected %d losers (409), got %d; full counts: %+v", n-1, statusCounts[http.StatusConflict], statusCounts)
	}
}

func TestLockList(t *testing.T) {
	apiKey := registerProjectTestDevice(t, "lock-k")

	ctx := context.Background()
	t.Cleanup(func() {
		testRdb.Del(ctx, "agentlink:lock:proj8:a.html")
		testRdb.Del(ctx, "agentlink:lock:proj8:b.html")
		testRdb.Del(ctx, "agentlink:locks:lock-k:main")
	})

	for _, p := range []string{"a.html", "b.html"} {
		resp, _ := doLockAcquire(t, apiKey, "proj8", "main", p, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("setup: expected 200 acquiring %s, got %d", p, resp.StatusCode)
		}
	}

	req, _ := http.NewRequest("GET", ts.URL+"/locks/list?project=proj8", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var lr LockListResponse
	json.NewDecoder(resp.Body).Decode(&lr)
	if len(lr.Locks) != 2 {
		t.Fatalf("expected 2 locks, got %d", len(lr.Locks))
	}

	found := map[string]bool{}
	for _, l := range lr.Locks {
		found[l.Path] = true
		if l.Owner != "lock-k:main" {
			t.Errorf("expected owner=lock-k:main for %s, got %s", l.Path, l.Owner)
		}
		if l.AcquiredAt == 0 {
			t.Errorf("expected non-zero acquired_at for %s", l.Path)
		}
		if l.LeaseExpiresAt == 0 {
			t.Errorf("expected non-zero lease_expires_at for %s", l.Path)
		}
	}
	if !found["a.html"] || !found["b.html"] {
		t.Errorf("expected both a.html and b.html in list, got %+v", lr.Locks)
	}
}
