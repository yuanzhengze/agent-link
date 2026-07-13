package api

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// doPreview issues an unauthenticated GET to /preview/{project}/{path} (no
// Authorization header — the whole point of the route) and returns the
// response plus its fully-read body.
func doPreview(t *testing.T, project, path string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.Get(ts.URL + "/preview/" + project + "/" + path)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp, body
}

func TestPreview_servesFile(t *testing.T) {
	apiKey := registerProjectTestDevice(t, "preview-owner-1")
	projectID := createTestProject(t, apiKey, "preview-test-1")

	// Default path ("") should resolve to index.html, the seed file every
	// new project is created with.
	resp, body := doPreview(t, projectID, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "New Project") {
		t.Errorf("expected seed index.html content in body, got %q", body)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("expected text/html content type, got %q", ct)
	}
}

func TestPreview_injectsLiveReloadIntoHTML(t *testing.T) {
	apiKey := registerProjectTestDevice(t, "preview-owner-2")
	projectID := createTestProject(t, apiKey, "preview-test-2")

	resp, body := doPreview(t, projectID, "index.html")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", resp.StatusCode, body)
	}

	s := string(body)
	if !strings.Contains(s, "new WebSocket") {
		t.Errorf("expected injected script to contain 'new WebSocket', got body=%s", s)
	}
	if !strings.Contains(s, projectID) {
		t.Errorf("expected injected script to reference project id %q, got body=%s", projectID, s)
	}
	if !strings.Contains(s, "file_changed") {
		t.Errorf("expected injected script to match the file_changed event type, got body=%s", s)
	}
}

func TestPreview_nonHtmlNotInjected(t *testing.T) {
	apiKey := registerProjectTestDevice(t, "preview-owner-3")
	projectID := createTestProject(t, apiKey, "preview-test-3")

	t.Cleanup(func() {
		ctx := context.Background()
		testRdb.Del(ctx, "agentlink:lock:"+projectID+":style.css")
		testRdb.Del(ctx, "agentlink:locks:preview-owner-3:main")
	})

	respAcq, _ := doLockAcquire(t, apiKey, projectID, "main", "style.css", "")
	if respAcq.StatusCode != http.StatusOK {
		t.Fatalf("setup: expected 200 acquiring lock on style.css, got %d", respAcq.StatusCode)
	}
	cssContent := "body { color: red; }"
	respApply, _ := doApply(t, apiKey, projectID, "main", "style.css", cssContent)
	if respApply.StatusCode != http.StatusOK {
		t.Fatalf("setup: expected 200 applying style.css, got %d", respApply.StatusCode)
	}

	resp, body := doPreview(t, projectID, "style.css")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", resp.StatusCode, body)
	}
	if string(body) != cssContent {
		t.Errorf("expected raw css content %q, got %q", cssContent, string(body))
	}
	if strings.Contains(string(body), "<script") || strings.Contains(string(body), "new WebSocket") {
		t.Errorf("expected no injected script in non-html response, got %q", body)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "css") {
		t.Errorf("expected a css content-type, got %q", ct)
	}
}

func TestPreview_pathTraversal_404(t *testing.T) {
	apiKey := registerProjectTestDevice(t, "preview-owner-4")
	projectID := createTestProject(t, apiKey, "preview-test-4")

	// %2e%2e survives Go's ServeMux path-cleaning redirect (which only
	// triggers on literal ".." segments) and arrives at the handler as a
	// raw ".." path segment, exercising safeApplyPath's traversal check
	// rather than just the mux's own dot-segment redirect.
	resp, body := doPreview(t, projectID, "%2e%2e/%2e%2e/etc/passwd")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d, body=%s", resp.StatusCode, body)
	}
}

func TestPreview_missingProject_404(t *testing.T) {
	resp, body := doPreview(t, "does-not-exist", "index.html")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d, body=%s", resp.StatusCode, body)
	}
}

func TestPreview_missingFile_404(t *testing.T) {
	apiKey := registerProjectTestDevice(t, "preview-owner-5")
	projectID := createTestProject(t, apiKey, "preview-test-5")

	resp, body := doPreview(t, projectID, "nope.html")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d, body=%s", resp.StatusCode, body)
	}
}

// TestPreviewToken_cannotWrite proves the read-only preview token injected
// into preview pages carries no write capability: it must be rejected by a
// write endpoint (POST /apply) protected by authMiddleware, yet still be
// accepted by GET /ws, which only ever subscribes and never writes.
func TestPreviewToken_cannotWrite(t *testing.T) {
	apiKey := registerProjectTestDevice(t, "preview-owner-6")
	projectID := createTestProject(t, apiKey, "preview-test-6")

	previewToken := testSrv.previewToken
	if previewToken == "" {
		t.Fatal("expected server to have a non-empty preview token")
	}

	body := `{"session":"main","path":"hack.html","content":"pwned"}`
	req, _ := http.NewRequest("POST", ts.URL+"/projects/"+projectID+"/apply", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+previewToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 using preview token against /apply (write path), got %d", resp.StatusCode)
	}

	wsBase := "ws" + strings.TrimPrefix(ts.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsBase+"/ws?project="+projectID+"&token="+previewToken, nil)
	if err != nil {
		t.Fatalf("expected ws dial with preview token to succeed, got err=%v", err)
	}
	c.CloseNow()
}
