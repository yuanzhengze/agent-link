package rt

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSync_snapshotWritesFiles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/teams/tm_x/projects/p1/snapshot" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Device ds_test" {
			t.Errorf("expected Device auth, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"head_commit": "deadbeef",
			"files": []map[string]string{
				{"path": "index.html", "content": "<h1>hi</h1>"},
				{"path": "css/style.css", "content": "body{color:red}"},
			},
		})
	}))
	defer srv.Close()

	dir := t.TempDir()
	s := &Syncer{
		Project:       "p1",
		LocalDir:      dir,
		TeamID:        "tm_x",
		Server:        srv.URL,
		DeviceSession: "ds_test",
		Stdout:        io.Discard,
	}

	if err := s.pullSnapshot(); err != nil {
		t.Fatalf("pullSnapshot: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "index.html"))
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	if string(got) != "<h1>hi</h1>" {
		t.Errorf("index.html: expected %q, got %q", "<h1>hi</h1>", got)
	}

	got, err = os.ReadFile(filepath.Join(dir, "css", "style.css"))
	if err != nil {
		t.Fatalf("read css/style.css: %v", err)
	}
	if string(got) != "body{color:red}" {
		t.Errorf("css/style.css: expected %q, got %q", "body{color:red}", got)
	}
}

func TestSync_localChangeCallsApply(t *testing.T) {
	var mu sync.Mutex
	var applyBody map[string]string
	applyCalls := 0

	var lockBody map[string]string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/teams/tm_x/locks/acquire", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		json.NewDecoder(r.Body).Decode(&lockBody)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"owner": map[string]string{"label": "u@dev1/main"}})
	})
	mux.HandleFunc("/api/teams/tm_x/projects/p1/apply", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		applyCalls++
		json.NewDecoder(r.Body).Decode(&applyBody)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"head_commit": "abc123"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.html"), []byte("<p>local edit</p>"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &Syncer{
		Project:       "p1",
		LocalDir:      dir,
		Session:       "main",
		Device:        "dev1",
		DeviceID:      "d_1",
		UserID:        "u_1",
		TeamID:        "tm_x",
		Server:        srv.URL,
		DeviceSession: "ds_test",
		Stdout:        io.Discard,
	}

	if err := s.handleLocalChange(context.Background(), "a.html"); err != nil {
		t.Fatalf("handleLocalChange: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if applyCalls != 1 {
		t.Fatalf("expected exactly 1 apply call, got %d", applyCalls)
	}
	if lockBody["project_id"] != "p1" {
		t.Errorf("expected lock project_id=p1, got %q", lockBody["project_id"])
	}
	if _, ok := lockBody["session"]; ok {
		t.Errorf("lock body must not contain session; got %v", lockBody)
	}
	if _, ok := applyBody["session"]; ok {
		t.Errorf("apply body must not contain session; got %v", applyBody)
	}
	if applyBody["path"] != "a.html" {
		t.Errorf("expected path=a.html, got %q", applyBody["path"])
	}
	if applyBody["content"] != "<p>local edit</p>" {
		t.Errorf("expected content=%q, got %q", "<p>local edit</p>", applyBody["content"])
	}
}

// TestSyncWSUsesDeviceHeaderNotQueryCredential proves the sync WebSocket dial
// carries the Device credential in the Authorization header (plus the local
// session header) and never leaks it into the URL/query, hitting the
// team-scoped /api/teams/<team>/ws?project=<id> endpoint.
func TestSyncWSUsesDeviceHeaderNotQueryCredential(t *testing.T) {
	var (
		mu                                  sync.Mutex
		gotPath, gotQuery, gotAuth, gotSess string
		seen                                = make(chan struct{}, 1)
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		gotSess = r.Header.Get("X-Agentlink-Session")
		mu.Unlock()
		select {
		case seen <- struct{}{}:
		default:
		}
		// Refuse the upgrade so wsLoop's dial fails fast and it backs off.
		http.Error(w, "no upgrade", http.StatusNotImplemented)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	s := &Syncer{
		Project:       "p1",
		TeamID:        "tm_x",
		Session:       "worker",
		DeviceSession: "ds_secret",
		Server:        srv.URL,
		Stdout:        io.Discard,
		Ctx:           ctx,
	}
	s.initDefaults()

	done := make(chan struct{})
	go func() {
		s.wsLoop()
		close(done)
	}()

	select {
	case <-seen:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("ws dial never reached the server")
	}
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if gotPath != "/api/teams/tm_x/ws" {
		t.Errorf("expected /api/teams/tm_x/ws, got %s", gotPath)
	}
	if gotAuth != "Device ds_secret" {
		t.Errorf("expected Device auth header, got %q", gotAuth)
	}
	if gotSess != "worker" {
		t.Errorf("expected X-Agentlink-Session=worker, got %q", gotSess)
	}
	if !strings.Contains(gotQuery, "project=p1") {
		t.Errorf("expected project=p1 in query, got %q", gotQuery)
	}
	if strings.Contains(gotQuery, "ds_secret") || strings.Contains(gotQuery, "token") {
		t.Errorf("credential/token must never appear in the ws query: %q", gotQuery)
	}
}

func TestSync_wsWritesRemoteFile(t *testing.T) {
	dir := t.TempDir()
	s := &Syncer{
		Project:  "p1",
		LocalDir: dir,
		Session:  "main",
		Device:   "dev1",
		Stdout:   io.Discard,
	}

	ev := syncEvent{
		Type:      "file_changed",
		ProjectID: "p1",
		Path:      "a.html",
		Content:   "<h1>x</h1>",
		By:        syncEventOwner{UserID: "u_other", DeviceID: "otherdev", SessionName: "other", Label: "other@otherdev/other"},
	}
	if err := s.handleRemoteEvent(ev); err != nil {
		t.Fatalf("handleRemoteEvent: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "a.html"))
	if err != nil {
		t.Fatalf("read a.html: %v", err)
	}
	if string(got) != "<h1>x</h1>" {
		t.Errorf("expected %q, got %q", "<h1>x</h1>", got)
	}
}

func TestSync_ignoresEchoedChange(t *testing.T) {
	applyCalls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api/teams/tm_x/locks/acquire", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"owner": "dev1:main"})
	})
	mux.HandleFunc("/api/teams/tm_x/projects/p1/apply", func(w http.ResponseWriter, r *http.Request) {
		applyCalls++
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"head_commit": "abc123"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	s := &Syncer{
		Project:       "p1",
		LocalDir:      dir,
		Session:       "main",
		Device:        "dev1",
		DeviceID:      "d_1",
		UserID:        "u_1",
		TeamID:        "tm_x",
		Server:        srv.URL,
		DeviceSession: "ds_test",
		Stdout:        io.Discard,
	}

	// A remote (other device) event writes the file and records its hash
	// as "the last thing we wrote", simulating the WS write-back path.
	ev := syncEvent{
		Type:      "file_changed",
		ProjectID: "p1",
		Path:      "b.html",
		Content:   "<p>remote content</p>",
		By:        syncEventOwner{UserID: "u_other", DeviceID: "otherdev", SessionName: "other", Label: "other@otherdev/other"},
	}
	if err := s.handleRemoteEvent(ev); err != nil {
		t.Fatalf("handleRemoteEvent: %v", err)
	}

	// fsnotify would now fire for b.html since it changed on disk. The
	// content on disk is identical to what handleRemoteEvent just wrote,
	// so this must be recognized as an echo and NOT trigger an apply.
	if err := s.handleLocalChange(context.Background(), "b.html"); err != nil {
		t.Fatalf("handleLocalChange: %v", err)
	}

	if applyCalls != 0 {
		t.Fatalf("expected 0 apply calls for echoed change, got %d", applyCalls)
	}

	// Self-authored events (by == myOwner) must also be ignored, so our
	// own applies echoed back over WS don't get rewritten locally either.
	selfEv := syncEvent{
		Type:      "file_changed",
		ProjectID: "p1",
		Path:      "c.html",
		Content:   "<p>should not be written</p>",
		By:        s.myOwner(),
	}
	if err := s.handleRemoteEvent(selfEv); err != nil {
		t.Fatalf("handleRemoteEvent(self): %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "c.html")); !os.IsNotExist(err) {
		t.Errorf("expected c.html to NOT be written for self-authored event, stat err=%v", err)
	}
}

// TestSync_flushDirtyAppliesSequentially exercises the drain logic added
// to fix #3 (concurrent same-path applies racing at the server) and #2
// (edits lost on shutdown): pre-populate the dirty set with two paths and
// call flushDirty directly (as flushWorker does, both on every debounce
// signal and once more on shutdown), then assert both were applied with
// their on-disk content and the dirty set ends up empty.
func TestSync_flushDirtyAppliesSequentially(t *testing.T) {
	var mu sync.Mutex
	appliedContent := map[string]string{}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/teams/tm_x/locks/acquire", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"owner": "dev1:main"})
	})
	mux.HandleFunc("/api/teams/tm_x/projects/p1/apply", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		appliedContent[body["path"]] = body["content"]
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"head_commit": "abc123"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.html"), []byte("content-a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.html"), []byte("content-b"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &Syncer{
		Project:       "p1",
		LocalDir:      dir,
		Session:       "main",
		Device:        "dev1",
		DeviceID:      "d_1",
		UserID:        "u_1",
		TeamID:        "tm_x",
		Server:        srv.URL,
		DeviceSession: "ds_test",
		Stdout:        io.Discard,
	}
	s.initDefaults()

	s.mu.Lock()
	s.dirty["a.html"] = struct{}{}
	s.dirty["b.html"] = struct{}{}
	s.mu.Unlock()

	s.flushDirty(context.Background())

	mu.Lock()
	if appliedContent["a.html"] != "content-a" {
		t.Errorf("a.html: expected apply content %q, got %q", "content-a", appliedContent["a.html"])
	}
	if appliedContent["b.html"] != "content-b" {
		t.Errorf("b.html: expected apply content %q, got %q", "content-b", appliedContent["b.html"])
	}
	mu.Unlock()

	s.mu.Lock()
	dirtyLen := len(s.dirty)
	s.mu.Unlock()
	if dirtyLen != 0 {
		t.Errorf("expected dirty set to be emptied after flushDirty, got %d entries", dirtyLen)
	}
}

// TestSync_flushDirtyAppliesCurrentContentOnRedirty proves the property
// behind the #3 fix deterministically, without relying on real goroutine
// timing: a path that is marked dirty and flushed, edited again, and
// marked dirty and flushed a second time, always uploads the CURRENT
// on-disk content at the time of the second drain (v2), never a stale
// snapshot captured earlier. This is what makes concurrent same-path
// applies impossible to reorder into a stale-content-wins outcome — every
// drain re-reads the file fresh, so the most recent edit always wins.
func TestSync_flushDirtyAppliesCurrentContentOnRedirty(t *testing.T) {
	var mu sync.Mutex
	var appliedContents []string

	mux := http.NewServeMux()
	mux.HandleFunc("/api/teams/tm_x/locks/acquire", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"owner": "dev1:main"})
	})
	mux.HandleFunc("/api/teams/tm_x/projects/p1/apply", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		appliedContents = append(appliedContents, body["content"])
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"head_commit": "abc123"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "a.html")
	if err := os.WriteFile(path, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &Syncer{
		Project:       "p1",
		LocalDir:      dir,
		Session:       "main",
		Device:        "dev1",
		DeviceID:      "d_1",
		UserID:        "u_1",
		TeamID:        "tm_x",
		Server:        srv.URL,
		DeviceSession: "ds_test",
		Stdout:        io.Discard,
	}
	s.initDefaults()

	s.mu.Lock()
	s.dirty["a.html"] = struct{}{}
	s.mu.Unlock()
	s.flushDirty(context.Background())

	if err := os.WriteFile(path, []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.dirty["a.html"] = struct{}{}
	s.mu.Unlock()
	s.flushDirty(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if len(appliedContents) != 2 {
		t.Fatalf("expected 2 apply calls, got %d: %v", len(appliedContents), appliedContents)
	}
	if appliedContents[0] != "v1" {
		t.Errorf("first apply: expected %q, got %q", "v1", appliedContents[0])
	}
	if appliedContents[1] != "v2" {
		t.Errorf("second apply: expected CURRENT content %q, got %q", "v2", appliedContents[1])
	}
}

// TestSync_finalFlushUsesLiveContext proves the fix for the CRITICAL bug:
// flushWorker's shutdown-time final drain must use a still-LIVE context
// for its network calls, not s.Ctx (which is already cancelled by the
// time that drain runs — its cancellation is what triggered shutdown in
// the first place). Building the acquire/apply requests with an
// already-cancelled context makes every one of them fail immediately
// with "context canceled" before reaching the server, silently dropping
// every edit debounced but not yet applied at shutdown.
//
// This constructs a Syncer whose Ctx is already cancelled (mirroring the
// daemon's state exactly when flushWorker's ctx.Done() branch runs), puts
// a dirty edit in s.dirty, and shows that:
//   - flushDirty(s.ctx()) — i.e. draining with the cancelled context, as
//     the pre-fix code effectively did — reaches the server ZERO times
//     (locks in the root cause).
//   - flushDirty(context.Background()) — i.e. draining with the fresh,
//     live context flushWorker now passes for the final drain — reaches
//     the server and applies the correct content (proves the fix).
func TestSync_finalFlushUsesLiveContext(t *testing.T) {
	var mu sync.Mutex
	acquireCalls := 0
	applyCalls := 0
	var appliedContent string

	mux := http.NewServeMux()
	mux.HandleFunc("/api/teams/tm_x/locks/acquire", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		acquireCalls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"owner": "dev1:main"})
	})
	mux.HandleFunc("/api/teams/tm_x/projects/p1/apply", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		applyCalls++
		appliedContent = body["content"]
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"head_commit": "abc123"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.html"), []byte("pending edit"), 0o644); err != nil {
		t.Fatal(err)
	}

	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel() // Ctx is already cancelled, exactly as it is when flushWorker's ctx.Done() branch runs.

	s := &Syncer{
		Project:       "p1",
		LocalDir:      dir,
		Session:       "main",
		Device:        "dev1",
		DeviceID:      "d_1",
		UserID:        "u_1",
		TeamID:        "tm_x",
		Server:        srv.URL,
		DeviceSession: "ds_test",
		Stdout:        io.Discard,
		Ctx:           cancelledCtx,
	}
	s.initDefaults()

	// Root cause, locked in: draining with the daemon's own (cancelled)
	// context reaches the server zero times — every request fails
	// immediately with "context canceled" before it goes out.
	s.mu.Lock()
	s.dirty["a.html"] = struct{}{}
	s.mu.Unlock()
	s.flushDirty(s.ctx())

	mu.Lock()
	gotAcquire, gotApply := acquireCalls, applyCalls
	mu.Unlock()
	if gotAcquire != 0 || gotApply != 0 {
		t.Fatalf("flushDirty(s.ctx()) [cancelled]: expected 0 acquire/apply calls, got acquire=%d apply=%d", gotAcquire, gotApply)
	}

	// The fix: draining with a fresh, live context (what flushWorker's
	// shutdown branch now passes) reaches the server and applies the
	// pending edit, even though s.Ctx itself is cancelled.
	s.mu.Lock()
	s.dirty["a.html"] = struct{}{}
	s.mu.Unlock()
	s.flushDirty(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if applyCalls != 1 {
		t.Fatalf("flushDirty(live ctx): expected exactly 1 apply call, got %d", applyCalls)
	}
	if appliedContent != "pending edit" {
		t.Errorf("flushDirty(live ctx): expected applied content %q, got %q", "pending edit", appliedContent)
	}
}

// TestSync_runFlushesOnShutdown is the Run()-level version of the same
// proof: it starts Run() for real, with a debounce interval set so large
// that the normal timer-driven flush can never fire before the test
// cancels Ctx, so the only way the edit can reach the server is via
// flushWorker's shutdown-time final drain.
func TestSync_runFlushesOnShutdown(t *testing.T) {
	var mu sync.Mutex
	var appliedContent string
	applyCalls := 0
	snapshotServed := make(chan struct{}, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/teams/tm_x/projects/p1/snapshot", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"head_commit": "deadbeef",
			"files":       []map[string]string{},
		})
		select {
		case snapshotServed <- struct{}{}:
		default:
		}
	})
	mux.HandleFunc("/api/teams/tm_x/locks/acquire", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"owner": "dev1:main"})
	})
	mux.HandleFunc("/api/teams/tm_x/projects/p1/apply", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		applyCalls++
		appliedContent = body["content"]
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"head_commit": "abc123"})
	})
	mux.HandleFunc("/api/teams/tm_x/ws", func(w http.ResponseWriter, r *http.Request) {
		// No real WS upgrade needed for this test; just refuse politely
		// so wsLoop's dial fails fast and it backs off without noise.
		http.Error(w, "not implemented", http.StatusNotImplemented)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	s := &Syncer{
		Project:       "p1",
		LocalDir:      dir,
		Session:       "main",
		Device:        "dev1",
		DeviceID:      "d_1",
		UserID:        "u_1",
		TeamID:        "tm_x",
		Server:        srv.URL,
		DeviceSession: "ds_test",
		Stdout:        io.Discard,
		Ctx:           ctx,
		debounce:      time.Hour, // never fires on its own during this test
	}

	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- s.Run()
	}()

	select {
	case <-snapshotServed:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("timed out waiting for initial snapshot pull")
	}

	// The snapshotServed signal fires (on the server, mid-handler) before
	// Run has necessarily finished creating and arming the fsnotify
	// watcher back on the client side, so a single write right after it
	// can race watcher setup and be missed entirely. Rewrite the file
	// repeatedly (harmless: same content, and dirty-marking is
	// idempotent) until the watcher demonstrably picks it up, bounded
	// well under debounce (1h) so it can never race the normal flush
	// timer.
	targetPath := filepath.Join(dir, "shutdown.html")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := os.WriteFile(targetPath, []byte("edited before shutdown"), 0o644); err != nil {
			cancel()
			t.Fatal(err)
		}
		s.mu.Lock()
		_, dirty := s.dirty["shutdown.html"]
		s.mu.Unlock()
		if dirty {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("timed out waiting for fsnotify to mark shutdown.html dirty")
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()

	select {
	case err := <-runErrCh:
		if err != nil && err != context.Canceled {
			t.Fatalf("Run(): unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Run() to return after cancel")
	}

	mu.Lock()
	defer mu.Unlock()
	if applyCalls != 1 {
		t.Fatalf("expected exactly 1 apply call from the shutdown drain, got %d", applyCalls)
	}
	if appliedContent != "edited before shutdown" {
		t.Errorf("expected applied content %q, got %q", "edited before shutdown", appliedContent)
	}
}
