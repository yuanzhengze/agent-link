package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/team/agentlink/pkg/auth"
	"github.com/team/agentlink/pkg/redis"
)

func TestBearerRejectedOnV2Routes(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	req, _ := http.NewRequest(http.MethodGet, authV2TS.URL+"/api/auth/me", nil)
	req.Header.Set("Authorization", "Bearer sk_live_"+strings.Repeat("a", 64))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("Bearer on v2 route expected 401, got %d", resp.StatusCode)
	}
}

func TestDeviceAuthDoesNotFallBackToCookie(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	username := "fallback" + strings.Repeat("m", 6)
	regResp, _ := registerAuthUser(t, username, "correct horse battery staple")

	req, _ := http.NewRequest(http.MethodGet, authV2TS.URL+"/api/auth/me", nil)
	req.Header.Set("Cookie", withCookies(regResp))
	req.Header.Set("Authorization", "Bearer sk_live_"+strings.Repeat("b", 64))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("malformed Authorization must not fall back to cookie, got %d", resp.StatusCode)
	}
}

func TestDeviceActorName(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	username := "actorname" + strings.Repeat("n", 3)
	registerAuthUser(t, username, "correct horse battery staple")

	resp, body := authJSON(t, http.MethodPost, "/api/auth/device-login", map[string]string{
		"username":    username,
		"password":    "correct horse battery staple",
		"device_name": "workstation-alpha",
	}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("device-login failed: %d %s", resp.StatusCode, body)
	}
	var result map[string]any
	json.Unmarshal(body, &result)
	credential, _ := result["device_credential"].(string)

	probeResp, probeBody := authJSON(t, http.MethodGet, "/api/auth/test-actor", nil, map[string]string{
		"Authorization": "Device " + credential,
	})
	if probeResp.StatusCode != http.StatusOK {
		t.Fatalf("test-actor expected 200, got %d body=%s", probeResp.StatusCode, probeBody)
	}
	var actor map[string]any
	json.Unmarshal(probeBody, &actor)
	if actor["device_name"] != "workstation-alpha" {
		t.Fatalf("device_name = %v; want workstation-alpha", actor["device_name"])
	}
	if actor["client_type"] != "device" {
		t.Fatalf("client_type = %v; want device", actor["client_type"])
	}
}

func TestOversizedJSONRejected(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	big := strings.Repeat("x", 65*1024)
	req, _ := http.NewRequest(http.MethodPost, authV2TS.URL+"/api/auth/register", strings.NewReader(`{"username":"biguser","password":"`+big+`"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized body expected 400, got %d", resp.StatusCode)
	}
}

func TestUnknownJSONFieldsRejected(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	resp, body := authJSON(t, http.MethodPost, "/api/auth/register", map[string]any{
		"username":          "unkuser" + strings.Repeat("o", 5),
		"password":          "correct horse battery staple",
		"extra_field":       "nope",
		"register_password": "legacy",
	}, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown fields expected 400, got %d body=%s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), "legacy") {
		t.Fatal("error must not echo secrets")
	}
}

func TestExpiredDeviceLogoutIdempotent(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	username := "expdev" + strings.Repeat("p", 7)
	registerAuthUser(t, username, "correct horse battery staple")

	resp, body := authJSON(t, http.MethodPost, "/api/auth/device-login", map[string]string{
		"username":    username,
		"password":    "correct horse battery staple",
		"device_name": "expired-box",
	}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatal(body)
	}
	var result map[string]any
	json.Unmarshal(body, &result)
	credential, _ := result["device_credential"].(string)
	deviceID, _ := result["device_id"].(string)

	// Simulate natural session expiry while binding remains.
	store := auth.NewStore(authV2Rdb)
	ctx := context.Background()
	sessionHash := auth.SecretHash(credential)
	_ = authV2Rdb.Del(ctx, "agentlink:v2:device_session:"+sessionHash)

	resp1, _ := authJSON(t, http.MethodPost, "/api/auth/device-logout", nil, map[string]string{
		"Authorization": "Device " + credential,
	})
	if resp1.StatusCode != http.StatusNoContent {
		t.Fatalf("expired session logout expected 204, got %d", resp1.StatusCode)
	}
	resp2, _ := authJSON(t, http.MethodPost, "/api/auth/device-logout", nil, map[string]string{
		"Authorization": "Device " + credential,
	})
	if resp2.StatusCode != http.StatusNoContent {
		t.Fatalf("second logout expected 204, got %d", resp2.StatusCode)
	}
	if _, err := store.Device(ctx, deviceID); err == nil {
		t.Fatal("device record should be removed after logout")
	}
}

func TestCSRFHeaderMustMatchCookieAndHash(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	username := "hashcsrf" + strings.Repeat("q", 4)
	regResp, _ := registerAuthUser(t, username, "correct horse battery staple")
	csrf := cookieByName(regResp.Cookies(), csrfCookieName)

	req, _ := http.NewRequest(http.MethodPost, authV2TS.URL+"/api/auth/logout", nil)
	req.Header.Set("Cookie", withCookies(regResp))
	req.Header.Set("Origin", authTestOrigin)
	req.Header.Set("X-CSRF-Token", csrf.Value+"tampered")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("tampered csrf expected 403, got %d body=%s", resp.StatusCode, body)
	}
}

func TestPublicURLOriginMismatchOnRegister(t *testing.T) {
	rdb, err := redis.NewClientDB("localhost:6379", 1)
	if err != nil {
		t.Skip("redis not available")
	}
	keys, _ := rdb.Keys(context.Background(), "agentlink:v2:*").Result()
	if len(keys) > 0 {
		rdb.Del(context.Background(), keys...)
	}

	srv := NewWithOptions(ServerOptions{
		DataDir:   t.TempDir(),
		Redis:     rdb,
		PublicURL: "https://app.example.com",
	})
	ts := httptest.NewServer(srv.authMiddleware(srv.mux))
	defer ts.Close()

	body := map[string]string{
		"username": "pubuser" + strings.Repeat("r", 5),
		"password": "correct horse battery staple",
	}
	data, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/auth/register", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example.com")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin register expected 403, got %d", resp.StatusCode)
	}
}
