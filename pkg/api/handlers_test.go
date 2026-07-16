package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/team/agentlink/pkg/redis"
)

// Shared test fixtures for the api package. The v2 handlers authenticate per
// route (cookie/device + team role), so ts serves the mux directly with no
// global middleware.
var testRdb *redis.Client
var ts *httptest.Server
var testDataDir string
var testSrv *Server

func TestMain(m *testing.M) {
	rdb, err := redis.NewClient("localhost:6379")
	if err != nil {
		fmt.Println("redis not available, skipping api tests")
		os.Exit(0)
	}
	testRdb = rdb

	cleanupTestData()

	testDataDir, err = os.MkdirTemp("", "agentlink-api-test-")
	if err != nil {
		fmt.Println("failed to create temp data dir:", err)
		os.Exit(1)
	}

	testSrv = New("", testDataDir, rdb)
	ts = httptest.NewServer(testSrv.mux)

	code := m.Run()

	ts.Close()
	cleanupTestData()
	os.RemoveAll(testDataDir)
	rdb.Close()
	os.Exit(code)
}

func cleanupTestData() {
	ctx := context.Background()
	keys, _ := testRdb.Keys(ctx, "agentlink:v2:*").Result()
	if len(keys) > 0 {
		testRdb.Del(ctx, keys...)
	}
}

func TestHealth(t *testing.T) {
	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	var hr HealthResponse
	json.NewDecoder(resp.Body).Decode(&hr)
	if !hr.Ok {
		t.Errorf("expected ok=true, got %+v", hr)
	}
	if hr.Redis != "connected" {
		t.Errorf("expected redis=connected, got %s", hr.Redis)
	}
}

func TestDeviceNameRegex(t *testing.T) {
	tests := []struct {
		name  string
		valid bool
	}{
		{"valid-name", true},
		{"valid123", true},
		{"a", false},                     // too short
		{"ab", true},                     // minimum length
		{"", false},                      // empty
		{"UPPERCASE", false},             // uppercase
		{"has space", false},             // space
		{"has@symbol", false},            // special char
		{strings.Repeat("a", 33), false}, // too long
		{strings.Repeat("a", 32), true},  // max length
		{"1start-with-digit", false},     // starts with digit
		{"_start-underscore", false},     // starts with underscore
		{"valid-with_underscore", true},  // underscore ok
		{"valid-with-dash", true},        // dash ok
		{"valid_with_123", true},         // combined
	}
	for _, tc := range tests {
		got := deviceNameRE.MatchString(tc.name)
		if got != tc.valid {
			t.Errorf("deviceNameRE.MatchString(%q) = %v, want %v", tc.name, got, tc.valid)
		}
	}
}
