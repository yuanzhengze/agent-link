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
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/fsnotify/fsnotify"
	api "github.com/team/agentlink/pkg/cli/net"
)

// syncDebounce is how long watchLoop waits for the directory tree to go
// quiet before waking flushWorker to drain the dirty set, so a burst of
// writes (editor save, build tool) collapses into a single apply per path.
const syncDebounce = 300 * time.Millisecond

// syncReconnectBackoff is the delay between WebSocket reconnect attempts.
const syncReconnectBackoff = 2 * time.Second

// syncFinalFlushTimeout bounds the shutdown-time final drain (see
// flushWorker), so a graceful shutdown can't hang forever if the server
// is unreachable when the daemon is asked to exit.
const syncFinalFlushTimeout = 10 * time.Second

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
	// debounce is how long watchLoop waits for the directory tree to go
	// quiet before waking flushWorker (see syncDebounce). Defaults to
	// syncDebounce; overridable in tests that need to force an edit to
	// only be flushed by the shutdown path, never the normal timer.
	debounce time.Duration

	mu sync.Mutex
	// lastWrittenHash records the content hash of the last write WE made
	// to a given relative path (from a WS remote event, or the initial
	// snapshot). handleLocalChange compares against this before applying,
	// so our own write-back is never re-uploaded.
	lastWrittenHash map[string]string
	// dirty is the set of relative paths with a pending local edit not
	// yet drained by flushDirty. watchLoop adds to it (and arms the
	// debounce timer) on qualifying fsnotify events; flushDirty snapshots
	// and clears it. Guarded by mu.
	dirty map[string]struct{}
	// flushCh signals flushWorker that dirty has content to drain.
	// Buffered size 1 so watchLoop's debounce-timer callback (signalFlush)
	// never blocks: a pending signal already covers whatever is in dirty
	// by the time the worker gets to it.
	flushCh chan struct{}
}

func (s *Syncer) initDefaults() {
	if s.Stdout == nil {
		s.Stdout = io.Discard
	}
	if s.httpDo == nil {
		s.httpDo = http.DefaultClient.Do
	}
	if s.debounce <= 0 {
		s.debounce = syncDebounce
	}
	s.mu.Lock()
	if s.lastWrittenHash == nil {
		s.lastWrittenHash = make(map[string]string)
	}
	if s.dirty == nil {
		s.dirty = make(map[string]struct{})
	}
	if s.flushCh == nil {
		s.flushCh = make(chan struct{}, 1)
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

// handleLocalChange is invoked, for a relative path that fsnotify
// reported as changed, exclusively by flushDirty draining the dirty set
// (see flushDirty / flushWorker) — never directly from a timer callback —
// so at most one handleLocalChange for a given rel is ever in flight. It
// skips the upload entirely if the on-disk content matches what we most
// recently wrote ourselves (a WS echo), otherwise acquires the file lock
// and applies the new content.
//
// ctx is threaded through explicitly (rather than using s.ctx()) so that
// flushWorker's shutdown-time final drain can pass a still-live context:
// s.ctx() is already cancelled by the time that drain runs (its
// cancellation is what triggered the shutdown), and building the HTTP
// requests below with an already-cancelled context would make every one
// of them fail immediately with "context canceled" before reaching the
// server — silently dropping the very edits shutdown is supposed to
// flush. See flushWorker/flushDirty for the two contexts in play.
func (s *Syncer) handleLocalChange(ctx context.Context, rel string) error {
	s.initDefaults()

	full := filepath.Join(s.LocalDir, filepath.FromSlash(rel))
	content, err := os.ReadFile(full)
	if err != nil {
		return err
	}
	h := hash(string(content))

	// Known v1 limitation (not fixed here, by design): if a genuine local
	// edit races an incoming WS write to this SAME path, handleRemoteEvent
	// may set lastWrittenHash[rel] to the WS content between our read
	// above and this check, making our own (different, newer-on-disk-only
	// in the sense of "not yet uploaded") edit look like an echo — so it
	// is silently dropped: no apply, no warning, no error. This is
	// mitigated in practice by the write-lock discipline (agents are
	// expected to acquire the file lock before editing, so a concurrent
	// WS write to a path we're actively editing should be rare); a
	// complete fix needs content versioning or mtime comparison, which is
	// out of scope for v1.
	s.mu.Lock()
	echo := s.lastWrittenHash[rel] == h
	s.mu.Unlock()
	if echo {
		return nil
	}

	conflict, err := s.acquireLock(ctx, rel)
	if err != nil {
		return err
	}
	if conflict {
		fmt.Fprintf(s.Stdout, "sync: skip %s — locked by another session\n", rel)
		return nil
	}

	conflict, err = s.applyOne(ctx, rel, string(content))
	if err != nil {
		return err
	}
	if conflict {
		fmt.Fprintf(s.Stdout, "sync: skip %s — held by another session\n", rel)
		return nil
	}
	return nil
}

// flushDirty drains the dirty set into a local slice — snapshotting and
// clearing it under mu, then releasing the lock before doing any I/O —
// and calls handleLocalChange for each path SEQUENTIALLY. Because this is
// the only path that ever invokes handleLocalChange for locally-observed
// changes (watchLoop just marks paths dirty and arms a debounce timer; it
// never calls handleLocalChange itself), there is never more than one
// handleLocalChange in flight for the same rel — the fix for #3 (two
// concurrent applies for one path racing at the server, last-arrival-wins,
// silently reverting to stale content). Each handleLocalChange call reads
// the file fresh at drain time, so a path that was edited again after
// being marked dirty (but before this drain) still uploads its current
// content, not a stale snapshot.
//
// flushWorker calls this both on every debounce signal and once more,
// finally, on shutdown (ctx.Done()) — that final call is what flushes any
// edit debounced but not yet applied, fixing #2 (edits lost on shutdown).
// The ctx passed in is used for the network calls made while draining:
// the normal path passes s.ctx(), but flushWorker's shutdown-time call
// passes a fresh, still-live context (s.ctx() is already cancelled by
// then), so the final drain's requests actually reach the server instead
// of failing immediately with "context canceled".
func (s *Syncer) flushDirty(ctx context.Context) {
	s.initDefaults()

	s.mu.Lock()
	rels := make([]string, 0, len(s.dirty))
	for rel := range s.dirty {
		rels = append(rels, rel)
	}
	s.dirty = make(map[string]struct{})
	s.mu.Unlock()

	for _, rel := range rels {
		if err := s.handleLocalChange(ctx, rel); err != nil {
			fmt.Fprintf(s.Stdout, "sync: apply %s failed: %s\n", rel, err)
		}
	}
}

// acquireLock requests the file lock for rel. conflict is true on a 409
// (someone else holds it); the caller should skip the apply, not error out.
func (s *Syncer) acquireLock(ctx context.Context, rel string) (conflict bool, err error) {
	body, err := json.Marshal(syncLockAcquireRequest{Project: s.Project, Session: s.Session, Path: rel})
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", s.Server+"/locks/acquire", bytes.NewReader(body))
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
		io.Copy(io.Discard, resp.Body)
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
func (s *Syncer) applyOne(ctx context.Context, rel, content string) (conflict bool, err error) {
	body, err := json.Marshal(syncApplyRequest{Session: s.Session, Path: rel, Content: content})
	if err != nil {
		return false, err
	}
	url := fmt.Sprintf("%s/projects/%s/apply", s.Server, s.Project)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
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
		io.Copy(io.Discard, resp.Body)
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
// changes (fsnotify, debounced through a single flushWorker so per-path
// applies never run concurrently — see flushDirty) and remote changes
// (WebSocket, with reconnect) until Ctx is cancelled. On cancellation,
// flushWorker performs one final drain of any not-yet-applied debounced
// edits before Run returns.
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

	if err := addWatchDirs(watcher, s.LocalDir); err != nil {
		fmt.Fprintf(s.Stdout, "sync: watch setup failed: %s\n", err)
		watcher.Close()
		return err
	}

	var wg sync.WaitGroup
	wg.Add(3)

	go func() {
		defer wg.Done()
		s.watchLoop(watcher)
	}()

	go func() {
		defer wg.Done()
		s.wsLoop()
	}()

	go func() {
		defer wg.Done()
		s.flushWorker(s.ctx())
	}()

	<-s.ctx().Done()
	watcher.Close()
	wg.Wait()
	return s.ctx().Err()
}

// flushWorker is the single dedicated goroutine that drains the dirty set
// (see flushDirty's doc comment for why this must be the only caller of
// handleLocalChange on the local-change path). It wakes on every debounce
// signal and, on ctx cancellation, performs one FINAL flushDirty before
// returning so any edit that was debounced but hadn't fired yet is still
// applied on graceful shutdown (#2). It never holds s.mu across the
// flushDirty call (flushDirty takes and releases mu internally, well
// before doing I/O), and flushCh is a non-blocking, buffered-1 send from
// the timer callback, so neither side can deadlock: the worker is always
// either blocked in select (able to observe ctx.Done() immediately) or
// busy running flushDirty (which itself cannot block on flushCh or mu).
//
// The normal (<-s.flushCh) path drains using s.ctx(): that context is
// still live at that point, since ctx cancellation is what would trigger
// the OTHER branch. The shutdown (<-ctx.Done()) path, by contrast, is
// only reached once ctx (== s.ctx(), passed in by Run) is ALREADY
// cancelled — so draining with that same context would make every
// request in the final flush fail immediately with "context canceled",
// never reaching the server. Instead it drains with a fresh context
// bounded by syncFinalFlushTimeout, not derived from ctx, so those final
// applies actually go out, while still bounding shutdown in case the
// server is unreachable.
func (s *Syncer) flushWorker(ctx context.Context) {
	for {
		select {
		case <-s.flushCh:
			s.flushDirty(s.ctx())
		case <-ctx.Done():
			finalCtx, cancel := context.WithTimeout(context.Background(), syncFinalFlushTimeout)
			s.flushDirty(finalCtx)
			cancel()
			return
		}
	}
}

// watchLoop consumes fsnotify events, ignoring hidden paths and directory
// events (except recursively adding newly-created directory trees to the
// watch set), and debounces bursts of writes across all paths behind a
// SINGLE timer: a qualifying event marks its relative path dirty and
// (re)arms the timer, whose callback only signals flushWorker (via
// signalFlush) — it never calls handleLocalChange itself. That single
// worker is what serializes applies per path (see flushDirty), fixing #3.
func (s *Syncer) watchLoop(watcher *fsnotify.Watcher) {
	var timer *time.Timer

	stop := func() {
		if timer != nil {
			timer.Stop()
		}
	}

	for {
		select {
		case <-s.ctx().Done():
			stop()
			return

		case ev, ok := <-watcher.Events:
			if !ok {
				stop()
				return
			}
			if isHiddenPath(s.LocalDir, ev.Name) {
				continue
			}

			info, statErr := os.Stat(ev.Name)
			if statErr == nil && info.IsDir() {
				if ev.Op&fsnotify.Create != 0 {
					// Recurse: a moved-in or mkdir -p'd tree can bring
					// nested subdirs with it, and those need watching
					// too, not just the top-level dir (#4).
					if addErr := addWatchDirs(watcher, ev.Name); addErr != nil {
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

			s.mu.Lock()
			s.dirty[rel] = struct{}{}
			s.mu.Unlock()

			if timer != nil {
				timer.Stop()
			}
			timer = time.AfterFunc(s.debounce, s.signalFlush)

		case werr, ok := <-watcher.Errors:
			if !ok {
				stop()
				return
			}
			fmt.Fprintf(s.Stdout, "sync: watcher error: %s\n", werr)
		}
	}
}

// signalFlush wakes flushWorker to drain the dirty set. The send is
// non-blocking (flushCh is buffered size 1): if a signal is already
// pending, the worker hasn't drained yet, and whatever caused this signal
// is already reflected in the dirty set the pending wake-up will see.
func (s *Syncer) signalFlush() {
	select {
	case s.flushCh <- struct{}{}:
	default:
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
//
// Ctx is wired to Ctrl-C / SIGTERM via signal.NotifyContext, so an
// operator-initiated shutdown cancels it, which unblocks Run(): the
// watcher closes and flushWorker performs its final drain (see
// flushWorker) before the process exits — without this, s.ctx() would
// fall back to context.Background(), which never cancels, and the
// shutdown/flush path would never engage in production.
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	s := &Syncer{
		Project:  project,
		LocalDir: localDir,
		Session:  session,
		Device:   cfg.Device,
		Server:   cfg.Server,
		APIKey:   creds.APIKey,
		Stdout:   os.Stdout,
		Ctx:      ctx,
	}
	s.initDefaults()
	if err := s.Run(); err != nil && err != context.Canceled {
		return err
	}
	return nil
}
