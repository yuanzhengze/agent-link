package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// setupWSTest mounts a dedicated Hub + httptest.Server (isolated from the
// shared testSrv/ts, which other tests such as apply_test.go swap s.hub on)
// and registers a test device via the existing registration helper to
// obtain a valid API key for the WS ?token= query param.
func setupWSTest(t *testing.T) (hub *Hub, wsBase string, apiKey string) {
	t.Helper()

	hub = NewHub(testRdb)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws", hub.handleWS)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	wsBase = "ws" + strings.TrimPrefix(server.URL, "http")
	apiKey = registerProjectTestDevice(t, "ws-test-"+strings.ToLower(strconv.FormatInt(time.Now().UnixNano(), 36)))
	return hub, wsBase, apiKey
}

// dialWS dials the test WS server for the given project/token and fails
// the test on error.
func dialWS(t *testing.T, wsBase, project, token string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsBase+"/ws?project="+project+"&token="+token, nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	t.Cleanup(func() { c.CloseNow() })
	return c
}

// readEvent reads exactly one message off c within a short timeout and
// decodes it as an Event, failing the test if nothing arrives in time.
func readEvent(t *testing.T, c *websocket.Conn) Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("expected to read a message, got error: %v", err)
	}
	var ev Event
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatalf("failed to decode event JSON: %v (raw=%s)", err, data)
	}
	return ev
}

// expectNoMessage asserts that no message arrives on c within timeout.
func expectNoMessage(t *testing.T, c *websocket.Conn, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, data, err := c.Read(ctx)
	if err == nil {
		t.Fatalf("expected no message, but got one: %s", data)
	}
}

func TestWS_receivesBroadcast(t *testing.T) {
	hub, wsBase, apiKey := setupWSTest(t)
	c := dialWS(t, wsBase, "p1", apiKey)

	ev := Event{
		Type:       "file_changed",
		Project:    "p1",
		Path:       "a.html",
		Content:    "<h1>hi</h1>",
		HeadCommit: "deadbeef",
		By:         "dev:main",
		At:         time.Now().UTC().Format(time.RFC3339),
	}

	// Give the server a moment to complete registration before broadcasting.
	waitForSubscriber(t, hub, "p1", 1)
	hub.Broadcast("p1", ev)

	got := readEvent(t, c)
	if got != ev {
		t.Errorf("expected event %+v, got %+v", ev, got)
	}
}

// TestWS_broadcastReachesAllSubscribers guards against the regression where
// Broadcast wrote to subscribers sequentially on the caller's goroutine: with
// two live subscribers on the same project, a single Broadcast call must
// deliver the event to BOTH within a bounded read timeout. This does not
// attempt to simulate a slow/dead peer (buffer-fill timing to force that
// deterministically is flaky); it only proves the fan-out reaches every
// subscriber, which is sufficient to catch a reintroduced sequential loop
// via readEvent's bounded per-connection timeout.
func TestWS_broadcastReachesAllSubscribers(t *testing.T) {
	hub, wsBase, apiKey := setupWSTest(t)
	c1 := dialWS(t, wsBase, "p1", apiKey)
	c2 := dialWS(t, wsBase, "p1", apiKey)

	waitForSubscriber(t, hub, "p1", 2)

	ev := Event{
		Type:       "file_changed",
		Project:    "p1",
		Path:       "b.html",
		Content:    "<h1>hi again</h1>",
		HeadCommit: "cafebabe",
		By:         "dev:main",
		At:         time.Now().UTC().Format(time.RFC3339),
	}
	hub.Broadcast("p1", ev)

	got1 := readEvent(t, c1)
	got2 := readEvent(t, c2)
	if got1 != ev {
		t.Errorf("client 1: expected event %+v, got %+v", ev, got1)
	}
	if got2 != ev {
		t.Errorf("client 2: expected event %+v, got %+v", ev, got2)
	}
}

func TestWS_projectFilter(t *testing.T) {
	hub, wsBase, apiKey := setupWSTest(t)
	c1 := dialWS(t, wsBase, "p1", apiKey)

	waitForSubscriber(t, hub, "p1", 1)
	hub.Broadcast("p2", Event{Type: "file_changed", Project: "p2", At: time.Now().UTC().Format(time.RFC3339)})

	expectNoMessage(t, c1, 500*time.Millisecond)
}

func TestWS_badToken_401(t *testing.T) {
	_, wsBase, _ := setupWSTest(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, wsBase+"/ws?project=p1&token=sk_live_bad-token", nil)
	if err == nil {
		t.Fatal("expected dial to fail with a bad token")
	}
	if resp == nil {
		t.Fatalf("expected a handshake response, got none (err=%v)", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", resp.StatusCode)
	}
}

// waitForSubscriber polls until the hub reports want subscribers registered
// for project, so Broadcast in the test doesn't race the handler's
// registration (which happens asynchronously relative to Dial returning).
func waitForSubscriber(t *testing.T, hub *Hub, project string, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		hub.mu.RLock()
		n := len(hub.subs[project])
		hub.mu.RUnlock()
		if n >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d subscriber(s) on project %q", want, project)
}
