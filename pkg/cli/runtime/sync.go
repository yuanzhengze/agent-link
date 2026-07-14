package rt

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/fsnotify/fsnotify"
	api "github.com/team/agentlink/pkg/cli/net"
)

// syncDebounce is how long handleLocalChange waits for a relative path to
// go quiet before applying it, so a burst of writes (editor save, build
// tool) collapses into a single apply.
const syncDebounce = 300 * time.Millisecond

// syncReconnectBackoff is the delay between WebSocket reconnect attempts.
const syncReconnectBackoff = 2 * time.Second

// syncEvent mirrors the server's pkg/api.Event shape (apply.go). Defined
// locally rather than importing pkg/api, since the CLI is a separate
// client of the wire protocol, not a consumer of server internals.
type syncEvent struct {
	Type       string `json:"type"`
	Project    string `json:"project"`
	Path       string `json:"path"`
	Content    string `json:"content"`
	HeadCommit string `json:"head_commit"`
	By         string `json:"by"`
	At         string `json:"at"`
}

// Syncer keeps a local directory in sync with a cowork project on the
// server: it pulls a snapshot on start, pushes local edits via /apply
// (after acquiring the file lock), and applies others' edits received over
// WebSocket — without those writes bouncing back through fsnotify and
// re-applying (see lastWrittenHash / myOwner).
type Syncer struct {
	Project  string
	LocalDir string
	Session  string
	Device   string
	Server   string
	APIKey   string

	// Log output. Defaults to io.Discard.
	Stdout io.Writer

	// Cancellation. When nil, context.Background() is used.
	Ctx context.Context

	// Overridable for testing.
	httpDo func(req *http.Request) (*http.Response, error)

	mu sync.Mutex
	// lastWrittenHash records the content hash of the last write WE made
	// to a given relative path (from a WS remote event, or the initial
	// snapshot). handleLocalChange compares against this before applying,
	// so our own write-back is never re-uploaded.
	lastWrittenHash map[string]string
}

func (s *Syncer) initDefaults() {
	if s.Stdout == nil {
		s.Stdout = io.Discard
	}
	if s.httpDo == nil {
		s.httpDo = http.DefaultClient.Do
	}
	s.mu.Lock()
	if s.lastWrittenHash == nil {
		s.lastWrittenHash = make(map[string]string)
	}
	s.mu.Unlock()
}

func (s *Syncer) ctx() context.Context {
	if s.Ctx != nil {
		return s.Ctx
	}
	return context.Background()
}

// myOwner is the device:session identifier the server stamps onto events
// caused by our own applies, so we can recognize and ignore the echo.
func (s *Syncer) myOwner() string {
	return s.Device + ":" + s.Session
}

func hash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// -- snapshot --

type syncSnapshotFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type syncSnapshotResponse struct {
	HeadCommit string             `json:"head_commit"`
	Files      []syncSnapshotFile `json:"files"`
}

// pullSnapshot fetches the full current state of the project and seeds
// LocalDir with it. Every file written here is recorded in
// lastWrittenHash so the fsnotify watcher started right after treats
// these initial writes as echoes, not local edits to upload.
func (s *Syncer) pullSnapshot() error {
	s.initDefaults()

	url := fmt.Sprintf("%s/projects/%s/snapshot", s.Server, s.Project)
	req, err := http.NewRequestWithContext(s.ctx(), "GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.APIKey)

	resp, err := s.httpDo(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return decodeSyncError(resp)
	}

	var snap syncSnapshotResponse
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range snap.Files {
		full := filepath.Join(s.LocalDir, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, []byte(f.Content), 0o644); err != nil {
			return err
		}
		s.lastWrittenHash[f.Path] = hash(f.Content)
	}
	return nil
}

// -- local -> server --

type syncLockAcquireRequest struct {
	Project string `json:"project"`
	Session string `json:"session"`
	Path    string `json:"path"`
}

type syncLockAcquireResponse struct {
	Owner string `json:"owner"`
}

type syncApplyRequest struct {
	Session string `json:"session"`
	Path    string `json:"path"`
	Content string `json:"content"`
}

type syncApplyResponse struct {
	HeadCommit string `json:"head_commit"`
}

// handleLocalChange is invoked (after debounce) for a relative path that
// fsnotify reported as changed. It skips the upload entirely if the
// on-disk content matches what we most recently wrote ourselves (a WS
// echo), otherwise acquires the file lock and applies the new content.
func (s *Syncer) handleLocalChange(rel string) error {
	s.initDefaults()

	full := filepath.Join(s.LocalDir, filepath.FromSlash(rel))
	content, err := os.ReadFile(full)
	if err != nil {
		return err
	}
	h := hash(string(content))

	s.mu.Lock()
	echo := s.lastWrittenHash[rel] == h
	s.mu.Unlock()
	if echo {
		return nil
	}

	conflict, err := s.acquireLock(rel)
	if err != nil {
		return err
	}
	if conflict {
		fmt.Fprintf(s.Stdout, "sync: skip %s — locked by another session\n", rel)
		return nil
	}

	conflict, err = s.applyOne(rel, string(content))
	if err != nil {
		return err
	}
	if conflict {
		fmt.Fprintf(s.Stdout, "sync: skip %s — held by another session\n", rel)
		return nil
	}
	return nil
}

// acquireLock requests the file lock for rel. conflict is true on a 409
// (someone else holds it); the caller should skip the apply, not error out.
func (s *Syncer) acquireLock(rel string) (conflict bool, err error) {
	body, err := json.Marshal(syncLockAcquireRequest{Project: s.Project, Session: s.Session, Path: rel})
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(s.ctx(), "POST", s.Server+"/locks/acquire", bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.APIKey)

	resp, err := s.httpDo(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusConflict {
		return true, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, decodeSyncError(resp)
	}
	var lr syncLockAcquireResponse
	json.NewDecoder(resp.Body).Decode(&lr)
	return false, nil
}

// applyOne uploads rel's new content. conflict is true on a 409 (the lock
// was lost or held by someone else between acquire and apply).
func (s *Syncer) applyOne(rel, content string) (conflict bool, err error) {
	body, err := json.Marshal(syncApplyRequest{Session: s.Session, Path: rel, Content: content})
	if err != nil {
		return false, err
	}
	url := fmt.Sprintf("%s/projects/%s/apply", s.Server, s.Project)
	req, err := http.NewRequestWithContext(s.ctx(), "POST", url, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.APIKey)

	resp, err := s.httpDo(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusConflict {
		return true, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, decodeSyncError(resp)
	}
	var ar syncApplyResponse
	json.NewDecoder(resp.Body).Decode(&ar)
	return false, nil
}

func decodeSyncError(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		return fmt.Errorf("server %d: %s", resp.StatusCode, e.Error)
	}
	return fmt.Errorf("server %d", resp.StatusCode)
}

// -- server -> local --

// handleRemoteEvent applies a WS-delivered change to the local filesystem.
// Two cases are ignored entirely, both to avoid write-back loops:
//   - non file_changed events (nothing to write)
//   - events stamped with our own device:session (our apply, echoed back
//     to us as a subscriber of our own project — writing it again would
//     be a no-op at best and a redundant fsnotify trigger at worst)
//
// For everything else, lastWrittenHash[ev.Path] is set BEFORE the write so
// that even if fsnotify fires before this function returns, the racing
// handleLocalChange call still sees the correct "this is an echo" hash.
func (s *Syncer) handleRemoteEvent(ev syncEvent) error {
	s.initDefaults()

	if ev.Type != "file_changed" {
		return nil
	}
	if ev.By == s.myOwner() {
		return nil
	}

	s.mu.Lock()
	s.lastWrittenHash[ev.Path] = hash(ev.Content)
	s.mu.Unlock()

	full := filepath.Join(s.LocalDir, filepath.FromSlash(ev.Path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	return os.WriteFile(full, []byte(ev.Content), 0o644)
}

// -- Run: fsnotify + WS plumbing (thin; not unit-tested) --

// Run pulls the initial snapshot, then blocks watching for local file
// changes (fsnotify, debounced) and remote changes (WebSocket, with
// reconnect) until Ctx is cancelled.
func (s *Syncer) Run() error {
	s.initDefaults()

	if err := s.pullSnapshot(); err != nil {
		fmt.Fprintf(s.Stdout, "sync: initial snapshot failed: %s\n", err)
		return err
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()

	if err := addWatchDirs(watcher, s.LocalDir); err != nil {
		fmt.Fprintf(s.Stdout, "sync: watch setup failed: %s\n", err)
		return err
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		s.watchLoop(watcher)
	}()

	go func() {
		defer wg.Done()
		s.wsLoop()
	}()

	<-s.ctx().Done()
	watcher.Close()
	wg.Wait()
	return s.ctx().Err()
}

// watchLoop consumes fsnotify events, ignoring hidden paths and directory
// events (except adding newly-created directories to the watch set), and
// debounces bursts of writes per relative path before calling
// handleLocalChange.
func (s *Syncer) watchLoop(watcher *fsnotify.Watcher) {
	var tmu sync.Mutex
	timers := map[string]*time.Timer{}

	stopAll := func() {
		tmu.Lock()
		for _, t := range timers {
			t.Stop()
		}
		tmu.Unlock()
	}

	for {
		select {
		case <-s.ctx().Done():
			stopAll()
			return

		case ev, ok := <-watcher.Events:
			if !ok {
				stopAll()
				return
			}
			if isHiddenPath(s.LocalDir, ev.Name) {
				continue
			}

			info, statErr := os.Stat(ev.Name)
			if statErr == nil && info.IsDir() {
				if ev.Op&fsnotify.Create != 0 {
					if addErr := watcher.Add(ev.Name); addErr != nil {
						fmt.Fprintf(s.Stdout, "sync: watch %s failed: %s\n", ev.Name, addErr)
					}
				}
				continue
			}
			if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
				continue
			}

			rel, relErr := filepath.Rel(s.LocalDir, ev.Name)
			if relErr != nil {
				continue
			}
			rel = filepath.ToSlash(rel)

			tmu.Lock()
			if t, exists := timers[rel]; exists {
				t.Stop()
			}
			timers[rel] = time.AfterFunc(syncDebounce, func() {
				if err := s.handleLocalChange(rel); err != nil {
					fmt.Fprintf(s.Stdout, "sync: apply %s failed: %s\n", rel, err)
				}
			})
			tmu.Unlock()

		case werr, ok := <-watcher.Errors:
			if !ok {
				stopAll()
				return
			}
			fmt.Fprintf(s.Stdout, "sync: watcher error: %s\n", werr)
		}
	}
}

// addWatchDirs walks root and adds every non-hidden directory (including
// root) to the watcher, so fsnotify sees writes anywhere in the tree.
func addWatchDirs(watcher *fsnotify.Watcher, root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if path != root && isHiddenPath(root, path) {
			return filepath.SkipDir
		}
		return watcher.Add(path)
	})
}

// isHiddenPath reports whether any path segment of path (relative to
// base) starts with "." — this covers dotfiles and the .git directory.
func isHiddenPath(base, path string) bool {
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return true
	}
	for _, seg := range strings.Split(rel, string(filepath.Separator)) {
		if strings.HasPrefix(seg, ".") {
			return true
		}
	}
	return false
}

// wsLoop dials the project's WebSocket feed and reconnects (with a fixed
// backoff, honoring Ctx cancellation) whenever the dial or read fails.
func (s *Syncer) wsLoop() {
	wsBase := "ws" + strings.TrimPrefix(s.Server, "http")
	url := fmt.Sprintf("%s/ws?project=%s&token=%s", wsBase, s.Project, s.APIKey)

	for {
		if s.ctx().Err() != nil {
			return
		}

		conn, _, err := websocket.Dial(s.ctx(), url, nil)
		if err != nil {
			fmt.Fprintf(s.Stdout, "sync: ws dial failed: %s\n", err)
			if !s.sleepOrDone(syncReconnectBackoff) {
				return
			}
			continue
		}

		s.wsReadLoop(conn)
		conn.CloseNow()

		if !s.sleepOrDone(syncReconnectBackoff) {
			return
		}
	}
}

// wsReadLoop reads events off conn until it errors or Ctx is cancelled.
func (s *Syncer) wsReadLoop(conn *websocket.Conn) {
	for {
		_, data, err := conn.Read(s.ctx())
		if err != nil {
			if s.ctx().Err() != nil {
				return
			}
			fmt.Fprintf(s.Stdout, "sync: ws read error: %s\n", err)
			return
		}

		var ev syncEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			fmt.Fprintf(s.Stdout, "sync: bad event payload: %s\n", err)
			continue
		}
		if err := s.handleRemoteEvent(ev); err != nil {
			fmt.Fprintf(s.Stdout, "sync: apply remote event for %s failed: %s\n", ev.Path, err)
		}
	}
}

// sleepOrDone waits for d, returning false early (and immediately) if Ctx
// is cancelled first.
func (s *Syncer) sleepOrDone(d time.Duration) bool {
	select {
	case <-s.ctx().Done():
		return false
	case <-time.After(d):
		return true
	}
}

// -- entry point --

// RunSync loads the CLI's stored auth/session, ensures localDir exists,
// and runs the sync daemon until Ctx (or the process) is cancelled.
func RunSync(project, localDir string) error {
	cfg, creds, err := api.LoadAuth()
	if err != nil {
		return err
	}

	session, err := api.FindCurrentSession()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(localDir, 0o755); err != nil {
		return err
	}

	s := &Syncer{
		Project:  project,
		LocalDir: localDir,
		Session:  session,
		Device:   cfg.Device,
		Server:   cfg.Server,
		APIKey:   creds.APIKey,
		Stdout:   os.Stdout,
	}
	s.initDefaults()
	return s.Run()
}
