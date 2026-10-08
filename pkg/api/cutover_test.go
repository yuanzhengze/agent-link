package api

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// TestLegacyRegisterRouteIsGone proves the v1 device-registration endpoint no
// longer exists — it must never return 200.
func TestLegacyRegisterRouteIsGone(t *testing.T) {
	resp, err := http.Post(ts.URL+"/agents/register", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("POST /agents/register must not return 200, got %d", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected 404 or 405 for a removed route, got %d", resp.StatusCode)
	}
}

// TestLegacyBearerAPIKeyIsRejected proves a v1 Bearer API key is not accepted on
// a v2 team route.
func TestLegacyBearerAPIKeyIsRejected(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/teams/tm_x/projects", nil)
	req.Header.Set("Authorization", "Bearer sk_live_deadbeefdeadbeef")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a Bearer API key must be rejected with 401 on v2 routes, got %d", resp.StatusCode)
	}
}

// TestLegacyBusinessRoutesAreGone proves the v1 business and WebSocket routes no
// longer exist (never 200; only 404/405 from the static catch-all).
func TestLegacyBusinessRoutesAreGone(t *testing.T) {
	cases := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/projects"},
		{http.MethodPost, "/projects"},
		{http.MethodPost, "/locks/acquire"},
		{http.MethodPost, "/locks/release"},
		{http.MethodGet, "/locks/list"},
		{http.MethodGet, "/ws"},
		{http.MethodGet, "/whoami"},
		{http.MethodGet, "/inbox/pull"},
		{http.MethodPost, "/messages/send"},
		{http.MethodPost, "/tasks/send"},
		{http.MethodPost, "/agents/heartbeat"},
		{http.MethodGet, "/agents/list"},
	}
	for _, c := range cases {
		req, _ := http.NewRequest(c.method, ts.URL+c.path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", c.method, c.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Errorf("%s %s must not return 200 after cutover", c.method, c.path)
		}
		if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s %s expected 404 or 405, got %d", c.method, c.path, resp.StatusCode)
		}
	}
}

// TestPreviewTokenNoLongerExists proves the Server no longer carries the v1
// preview-token, broadcaster, or register-password fields removed at cutover.
func TestPreviewTokenNoLongerExists(t *testing.T) {
	typ := reflect.TypeOf(Server{})
	for _, name := range []string{"previewToken", "hub", "registerPassword"} {
		if _, ok := typ.FieldByName(name); ok {
			t.Errorf("Server must not retain legacy field %q after cutover", name)
		}
	}
}
