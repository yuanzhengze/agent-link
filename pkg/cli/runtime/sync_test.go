package rt

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestSync_snapshotWritesFiles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/projects/p1/snapshot" {
			t.Errorf("unexpected path: %s", r.URL.Path)
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
		Project:  "p1",
		LocalDir: dir,
		Server:   srv.URL,
		APIKey:   "sk_test",
		Stdout:   io.Discard,
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

	mux := http.NewServeMux()
	mux.HandleFunc("/locks/acquire", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"owner": "dev1:main"})
	})
	mux.HandleFunc("/projects/p1/apply", func(w http.ResponseWriter, r *http.Request) {
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
		Project:  "p1",
		LocalDir: dir,
		Session:  "main",
		Device:   "dev1",
		Server:   srv.URL,
		APIKey:   "sk_test",
		Stdout:   io.Discard,
	}

	if err := s.handleLocalChange("a.html"); err != nil {
		t.Fatalf("handleLocalChange: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if applyCalls != 1 {
		t.Fatalf("expected exactly 1 apply call, got %d", applyCalls)
	}
	if applyBody["session"] != "main" {
		t.Errorf("expected session=main, got %q", applyBody["session"])
	}
	if applyBody["path"] != "a.html" {
		t.Errorf("expected path=a.html, got %q", applyBody["path"])
	}
	if applyBody["content"] != "<p>local edit</p>" {
		t.Errorf("expected content=%q, got %q", "<p>local edit</p>", applyBody["content"])
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
		Type:    "file_changed",
		Project: "p1",
		Path:    "a.html",
		Content: "<h1>x</h1>",
		By:      "otherdev:other",
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
	mux.HandleFunc("/locks/acquire", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"owner": "dev1:main"})
	})
	mux.HandleFunc("/projects/p1/apply", func(w http.ResponseWriter, r *http.Request) {
		applyCalls++
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"head_commit": "abc123"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	s := &Syncer{
		Project:  "p1",
		LocalDir: dir,
		Session:  "main",
		Device:   "dev1",
		Server:   srv.URL,
		APIKey:   "sk_test",
		Stdout:   io.Discard,
	}

	// A remote (other device) event writes the file and records its hash
	// as "the last thing we wrote", simulating the WS write-back path.
	ev := syncEvent{
		Type:    "file_changed",
		Project: "p1",
		Path:    "b.html",
		Content: "<p>remote content</p>",
		By:      "otherdev:other",
	}
	if err := s.handleRemoteEvent(ev); err != nil {
		t.Fatalf("handleRemoteEvent: %v", err)
	}

	// fsnotify would now fire for b.html since it changed on disk. The
	// content on disk is identical to what handleRemoteEvent just wrote,
	// so this must be recognized as an echo and NOT trigger an apply.
	if err := s.handleLocalChange("b.html"); err != nil {
		t.Fatalf("handleLocalChange: %v", err)
	}

	if applyCalls != 0 {
		t.Fatalf("expected 0 apply calls for echoed change, got %d", applyCalls)
	}

	// Self-authored events (by == myOwner) must also be ignored, so our
	// own applies echoed back over WS don't get rewritten locally either.
	selfEv := syncEvent{
		Type:    "file_changed",
		Project: "p1",
		Path:    "c.html",
		Content: "<p>should not be written</p>",
		By:      s.myOwner(),
	}
	if err := s.handleRemoteEvent(selfEv); err != nil {
		t.Fatalf("handleRemoteEvent(self): %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "c.html")); !os.IsNotExist(err) {
		t.Errorf("expected c.html to NOT be written for self-authored event, stat err=%v", err)
	}
}
