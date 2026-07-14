package api

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestServeGUI_indexOk verifies the embedded dashboard is served at GET /
// with no Authorization header, and that the response contains the app's
// mount point marker.
func TestServeGUI_indexOk(t *testing.T) {
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)

	if !strings.Contains(body, `id="app"`) {
		t.Errorf("expected body to contain the app mount point id=\"app\", got: %s", body)
	}
	if !strings.Contains(body, "<title>cowork</title>") {
		t.Errorf("expected body to contain the page title, got: %s", body)
	}
}

// TestServeGUI_appJsOk verifies app.js is served with no auth and a
// JavaScript content type.
func TestServeGUI_appJsOk(t *testing.T) {
	resp, err := http.Get(ts.URL + "/app.js")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "javascript") {
		t.Errorf("expected Content-Type to contain javascript, got %q", ct)
	}
}

// TestServeGUI_styleCssOk verifies style.css is served with no auth.
func TestServeGUI_styleCssOk(t *testing.T) {
	resp, err := http.Get(ts.URL + "/style.css")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

// TestServeGUI_apiStillRequiresAuth locks in that the new GET / route and
// static-asset skipAuth exemptions did not over-broaden: data endpoints
// like GET /projects must still 401 without a Bearer token.
func TestServeGUI_apiStillRequiresAuth(t *testing.T) {
	resp, err := http.Get(ts.URL + "/projects")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for GET /projects without a token, got %d", resp.StatusCode)
	}
}
