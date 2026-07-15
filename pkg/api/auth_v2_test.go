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

var (
	authV2Rdb      *redis.Client
	authV2Srv      *Server
	authV2TS       *httptest.Server
	authTestOrigin string
)

func setupAuthV2TestServer(t *testing.T) {
	t.Helper()
	if authV2TS != nil {
		return
	}
	rdb, err := redis.NewClientDB("localhost:6379", 1)
	if err != nil {
		t.Skip("redis not available")
	}
	authV2Rdb = rdb
	authV2Srv = NewWithOptions(ServerOptions{
		DataDir:      t.TempDir(),
		Redis:        rdb,
		CookieSecure: false,
		PublicURL:    "http://127.0.0.1:0",
	})
	authV2TS = httptest.NewServer(authV2Srv.authMiddleware(authV2Srv.mux))
	authTestOrigin = authV2TS.URL
	authV2Srv.publicOrigin = authTestOrigin
	t.Cleanup(func() {
		authV2TS.Close()
		authV2TS = nil
		authTestOrigin = ""
	})
}

func cleanupAuthV2Keys(t *testing.T) {
	t.Helper()
	if authV2Rdb == nil {
		return
	}
	ctx := context.Background()
	keys, _ := authV2Rdb.Keys(ctx, "agentlink:v2:*").Result()
	if len(keys) > 0 {
		authV2Rdb.Del(ctx, keys...)
	}
}

func cookieByName(cookies []*http.Cookie, name string) *http.Cookie {
	for _, c := range cookies {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func authJSON(t *testing.T, method, path string, body any, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	setupAuthV2TestServer(t)

	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, authV2TS.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, respBody
}

func registerAuthUser(t *testing.T, username, password string) (*http.Response, map[string]any) {
	t.Helper()
	resp, body := authJSON(t, http.MethodPost, "/api/auth/register", map[string]string{
		"username": username,
		"password": password,
	}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register %s: expected 200, got %d body=%s", username, resp.StatusCode, body)
	}
	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	return resp, result
}

func loginAuthUser(t *testing.T, username, password string, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	resp, body := authJSON(t, http.MethodPost, "/api/auth/login", map[string]string{
		"username": username,
		"password": password,
	}, headers)
	return resp, body
}

func withCookies(resp *http.Response) string {
	var parts []string
	for _, c := range resp.Cookies() {
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}

func TestAuthRegisterSetsSessionAndCSRFCookies(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	username := "reguser" + strings.Repeat("a", 8)
	resp, body := authJSON(t, http.MethodPost, "/api/auth/register", map[string]string{
		"username": username,
		"password": "correct horse battery staple",
	}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, body)
	}

	session := cookieByName(resp.Cookies(), sessionCookieName)
	csrf := cookieByName(resp.Cookies(), csrfCookieName)
	if session == nil || !session.HttpOnly || session.Value == "" {
		t.Fatal("missing HttpOnly al_session")
	}
	if csrf == nil || csrf.HttpOnly || csrf.Value == "" {
		t.Fatal("missing readable al_csrf")
	}
	if session.Path != "/" || csrf.Path != "/" {
		t.Fatalf("cookie paths want /, got session=%q csrf=%q", session.Path, csrf.Path)
	}
	if session.SameSite != http.SameSiteLaxMode || csrf.SameSite != http.SameSiteLaxMode {
		t.Fatal("cookies should be SameSite=Lax")
	}

	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	user, ok := result["user"].(map[string]any)
	if !ok {
		t.Fatalf("expected user object, got %v", result)
	}
	if _, exists := user["password_phc"]; exists {
		t.Fatal("response must not contain password_phc")
	}
	if _, exists := user["PasswordPHC"]; exists {
		t.Fatal("response must not contain PasswordPHC")
	}
}

func TestAuthLoginWrongPasswordIsGeneric401(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	username := "loginuser" + strings.Repeat("b", 6)
	registerAuthUser(t, username, "correct horse battery staple")

	resp, bodyBytes := loginAuthUser(t, username, "wrong password!!", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", resp.StatusCode, bodyBytes)
	}
	var errResp map[string]string
	if err := json.Unmarshal(bodyBytes, &errResp); err != nil {
		t.Fatal(err)
	}
	if errResp["error"] != "invalid credentials" {
		t.Fatalf("expected generic invalid credentials, got %q", errResp["error"])
	}
	if strings.Contains(string(bodyBytes), "wrong password") {
		t.Fatal("error must not echo password")
	}
}

func TestAuthBrowserLoginRejectsCrossOrigin(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	username := "origuser" + strings.Repeat("c", 6)
	registerAuthUser(t, username, "correct horse battery staple")

	resp, body := loginAuthUser(t, username, "correct horse battery staple", map[string]string{
		"Origin": "http://evil.example",
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d body=%s", resp.StatusCode, body)
	}
}

func TestAuthMeRotatesCSRF(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	username := "meuser" + strings.Repeat("d", 7)
	regResp, _ := registerAuthUser(t, username, "correct horse battery staple")
	oldCSRF := cookieByName(regResp.Cookies(), csrfCookieName)
	if oldCSRF == nil {
		t.Fatal("missing csrf cookie after register")
	}

	req, _ := http.NewRequest(http.MethodGet, authV2TS.URL+"/api/auth/me", nil)
	req.Header.Set("Cookie", withCookies(regResp))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, body)
	}

	newCSRF := cookieByName(resp.Cookies(), csrfCookieName)
	if newCSRF == nil || newCSRF.Value == "" {
		t.Fatal("me should set new csrf cookie")
	}
	if newCSRF.Value == oldCSRF.Value {
		t.Fatal("csrf should rotate on me")
	}
}

func TestAuthCookieWriteRejectsMissingCSRF(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	username := "csrfuser" + strings.Repeat("e", 5)
	regResp, _ := registerAuthUser(t, username, "correct horse battery staple")

	req, _ := http.NewRequest(http.MethodPost, authV2TS.URL+"/api/auth/logout", nil)
	req.Header.Set("Cookie", withCookies(regResp))
	req.Header.Set("Origin", authTestOrigin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 without csrf, got %d body=%s", resp.StatusCode, body)
	}
}

func TestAuthDeviceLoginAndLogout(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	username := "devuser" + strings.Repeat("f", 6)
	registerAuthUser(t, username, "correct horse battery staple")

	resp, body := authJSON(t, http.MethodPost, "/api/auth/device-login", map[string]string{
		"username":    username,
		"password":    "correct horse battery staple",
		"device_name": "my-laptop",
	}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("device-login expected 200, got %d body=%s", resp.StatusCode, body)
	}
	if len(resp.Cookies()) > 0 {
		t.Fatal("device-login must not set web cookies")
	}

	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	credential, _ := result["device_credential"].(string)
	if !strings.HasPrefix(credential, "ds_") {
		t.Fatalf("expected ds_ credential, got %q", credential)
	}
	user, _ := result["user"].(map[string]any)
	if _, exists := user["password_phc"]; exists {
		t.Fatal("device-login must not leak password_phc")
	}

	logoutResp, logoutBody := authJSON(t, http.MethodPost, "/api/auth/device-logout", nil, map[string]string{
		"Authorization": "Device " + credential,
	})
	if logoutResp.StatusCode != http.StatusNoContent {
		t.Fatalf("device-logout expected 204, got %d body=%s", logoutResp.StatusCode, logoutBody)
	}

	meResp, _ := authJSON(t, http.MethodGet, "/api/auth/me", nil, map[string]string{
		"Authorization": "Device " + credential,
	})
	if meResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked device should be 401, got %d", meResp.StatusCode)
	}
}

func TestAuthLogoutRevokesCookieSession(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	username := "logoutuser" + strings.Repeat("g", 3)
	regResp, _ := registerAuthUser(t, username, "correct horse battery staple")
	csrf := cookieByName(regResp.Cookies(), csrfCookieName)

	req, _ := http.NewRequest(http.MethodPost, authV2TS.URL+"/api/auth/logout", nil)
	req.Header.Set("Cookie", withCookies(regResp))
	req.Header.Set("Origin", authTestOrigin)
	req.Header.Set("X-CSRF-Token", csrf.Value)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout expected 204, got %d", resp.StatusCode)
	}
	if c := cookieByName(resp.Cookies(), sessionCookieName); c == nil || c.MaxAge != -1 {
		t.Fatal("logout should clear session cookie")
	}

	meReq, _ := http.NewRequest(http.MethodGet, authV2TS.URL+"/api/auth/me", nil)
	meReq.Header.Set("Cookie", withCookies(regResp))
	meResp, err := http.DefaultClient.Do(meReq)
	if err != nil {
		t.Fatal(err)
	}
	meResp.Body.Close()
	if meResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked session should be 401, got %d", meResp.StatusCode)
	}
}

func TestAuthChangePasswordRevokesAllSessions(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	username := "chpwuser" + strings.Repeat("h", 4)
	oldPassword := "correct horse battery staple"
	newPassword := "new horse battery staple"
	regResp, _ := registerAuthUser(t, username, oldPassword)

	_, deviceBody := authJSON(t, http.MethodPost, "/api/auth/device-login", map[string]string{
		"username":    username,
		"password":    oldPassword,
		"device_name": "pw-device",
	}, nil)
	var deviceResult map[string]any
	if err := json.Unmarshal(deviceBody, &deviceResult); err != nil {
		t.Fatal(err)
	}
	deviceCred, _ := deviceResult["device_credential"].(string)

	csrf := cookieByName(regResp.Cookies(), csrfCookieName)
	changeReq, _ := http.NewRequest(http.MethodPost, authV2TS.URL+"/api/auth/change-password", strings.NewReader(`{"current_password":"`+oldPassword+`","new_password":"`+newPassword+`"}`))
	changeReq.Header.Set("Content-Type", "application/json")
	changeReq.Header.Set("Cookie", withCookies(regResp))
	changeReq.Header.Set("Origin", authTestOrigin)
	changeReq.Header.Set("X-CSRF-Token", csrf.Value)
	changeResp, err := http.DefaultClient.Do(changeReq)
	if err != nil {
		t.Fatal(err)
	}
	changeResp.Body.Close()
	if changeResp.StatusCode != http.StatusNoContent {
		t.Fatalf("change-password expected 204, got %d", changeResp.StatusCode)
	}

	meReq, _ := http.NewRequest(http.MethodGet, authV2TS.URL+"/api/auth/me", nil)
	meReq.Header.Set("Cookie", withCookies(regResp))
	meResp, _ := http.DefaultClient.Do(meReq)
	meResp.Body.Close()
	if meResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("web session should be revoked after change-password, got %d", meResp.StatusCode)
	}

	devMeResp, _ := authJSON(t, http.MethodGet, "/api/auth/me", nil, map[string]string{
		"Authorization": "Device " + deviceCred,
	})
	if devMeResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("device session should be revoked after change-password, got %d", devMeResp.StatusCode)
	}
}

func TestAuthMustChangePasswordSessionIsRestricted(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	username := "mustchuser" + strings.Repeat("i", 3)
	registerAuthUser(t, username, "correct horse battery staple")

	store := auth.NewStore(authV2Rdb)
	user, err := store.UserByUsername(context.Background(), username)
	if err != nil {
		t.Fatal(err)
	}
	tempPassword, err := authV2Srv.authService.ResetPassword(context.Background(), username)
	if err != nil {
		t.Fatal(err)
	}
	_ = user

	loginResp, _ := loginAuthUser(t, username, tempPassword, nil)
	if loginResp.StatusCode != http.StatusOK {
		t.Fatalf("login with temp password expected 200, got %d", loginResp.StatusCode)
	}

	// Protected probe route registered only in tests via middleware test helper.
	resp, body := authJSON(t, http.MethodGet, "/api/auth/test-protected", nil, map[string]string{
		"Cookie": withCookies(loginResp),
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("restricted session expected 403, got %d body=%s", resp.StatusCode, body)
	}
	var errResp map[string]string
	json.Unmarshal(body, &errResp)
	if errResp["error"] != "password change required" {
		t.Fatalf("expected password change required, got %q", errResp["error"])
	}

	meResp, _ := authJSON(t, http.MethodGet, "/api/auth/me", nil, map[string]string{
		"Cookie": withCookies(loginResp),
	})
	if meResp.StatusCode != http.StatusOK {
		t.Fatalf("me should be allowed, got %d", meResp.StatusCode)
	}
}

func TestAuthResponseOmitsPasswordPHC(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	username := "safeuser" + strings.Repeat("j", 5)
	resp, body := authJSON(t, http.MethodPost, "/api/auth/register", map[string]string{
		"username": username,
		"password": "correct horse battery staple",
	}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode)
	}
	if strings.Contains(string(body), "password_phc") || strings.Contains(string(body), "PasswordPHC") {
		t.Fatalf("register body leaks password hash: %s", body)
	}
}

func TestAuthCookieSecureFlag(t *testing.T) {
	rdb, err := redis.NewClientDB("localhost:6379", 1)
	if err != nil {
		t.Skip("redis not available")
	}
	cleanup := func() {
		keys, _ := rdb.Keys(context.Background(), "agentlink:v2:*").Result()
		if len(keys) > 0 {
			rdb.Del(context.Background(), keys...)
		}
	}
	cleanup()
	t.Cleanup(cleanup)

	secureSrv := NewWithOptions(ServerOptions{
		DataDir:      t.TempDir(),
		Redis:        rdb,
		CookieSecure: true,
		PublicURL:    "https://app.example.com",
	})
	ts := httptest.NewServer(secureSrv.authMiddleware(secureSrv.mux))
	defer ts.Close()

	resp, _ := authJSONOn(ts, http.MethodPost, "/api/auth/register", map[string]string{
		"username": "secuser" + strings.Repeat("k", 5),
		"password": "correct horse battery staple",
	}, nil)
	session := cookieByName(resp.Cookies(), sessionCookieName)
	if session == nil || !session.Secure {
		t.Fatal("Secure cookies expected when CookieSecure=true")
	}
}

func TestAuthRegisterDuplicateUsername409(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	username := "dupuser" + strings.Repeat("l", 6)
	registerAuthUser(t, username, "correct horse battery staple")
	resp, body := authJSON(t, http.MethodPost, "/api/auth/register", map[string]string{
		"username": username,
		"password": "another password here",
	}, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409, got %d body=%s", resp.StatusCode, body)
	}
}

func authJSONOn(ts *httptest.Server, method, path string, body any, headers map[string]string) (*http.Response, []byte) {
	var reader io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reader = bytes.NewReader(data)
	}
	req, _ := http.NewRequest(method, ts.URL+path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, _ := http.DefaultClient.Do(req)
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, respBody
}

func mustReadBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return body
}

