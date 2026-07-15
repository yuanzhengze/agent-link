package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	authV2Srv.mux.Handle(
		"GET /api/auth/test-protected",
		authV2Srv.requireIdentity(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
		})),
	)
	authV2Srv.mux.Handle(
		"GET /api/auth/test-actor",
		authV2Srv.requireIdentity(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			actor, ok := ActorFromContext(r.Context())
			if !ok {
				writeError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{
				"user_id":     actor.UserID,
				"username":    actor.Username,
				"device_id":   actor.DeviceID,
				"device_name": actor.DeviceName,
				"client_type": actor.ClientType,
			})
		})),
	)
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

func TestAuthMeRotationInvalidatesPreviousCSRF(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	username := "rotatecsrf" + strings.Repeat("s", 3)
	regResp, _ := registerAuthUser(t, username, "correct horse battery staple")
	session := cookieByName(regResp.Cookies(), sessionCookieName)
	oldCSRF := cookieByName(regResp.Cookies(), csrfCookieName)
	if session == nil || oldCSRF == nil {
		t.Fatal("register response missing auth cookies")
	}

	meReq, _ := http.NewRequest(http.MethodGet, authV2TS.URL+"/api/auth/me", nil)
	meReq.Header.Set("Cookie", withCookies(regResp))
	meResp, err := http.DefaultClient.Do(meReq)
	if err != nil {
		t.Fatal(err)
	}
	meBody, _ := io.ReadAll(meResp.Body)
	meResp.Body.Close()
	if meResp.StatusCode != http.StatusOK {
		t.Fatalf("me expected 200, got %d body=%s", meResp.StatusCode, meBody)
	}
	newCSRF := cookieByName(meResp.Cookies(), csrfCookieName)
	if newCSRF == nil || newCSRF.Value == "" || newCSRF.Value == oldCSRF.Value {
		t.Fatal("me should return a distinct csrf cookie")
	}

	oldReq, _ := http.NewRequest(http.MethodPost, authV2TS.URL+"/api/auth/logout", nil)
	oldReq.Header.Set(
		"Cookie",
		sessionCookieName+"="+session.Value+"; "+csrfCookieName+"="+oldCSRF.Value,
	)
	oldReq.Header.Set("Origin", authTestOrigin)
	oldReq.Header.Set("X-CSRF-Token", oldCSRF.Value)
	oldResp, err := http.DefaultClient.Do(oldReq)
	if err != nil {
		t.Fatal(err)
	}
	oldBody, _ := io.ReadAll(oldResp.Body)
	oldResp.Body.Close()
	if oldResp.StatusCode != http.StatusForbidden {
		t.Fatalf("old csrf expected 403, got %d body=%s", oldResp.StatusCode, oldBody)
	}

	newReq, _ := http.NewRequest(http.MethodPost, authV2TS.URL+"/api/auth/logout", nil)
	newReq.Header.Set(
		"Cookie",
		sessionCookieName+"="+session.Value+"; "+csrfCookieName+"="+newCSRF.Value,
	)
	newReq.Header.Set("Origin", authTestOrigin)
	newReq.Header.Set("X-CSRF-Token", newCSRF.Value)
	newResp, err := http.DefaultClient.Do(newReq)
	if err != nil {
		t.Fatal(err)
	}
	newBody, _ := io.ReadAll(newResp.Body)
	newResp.Body.Close()
	if newResp.StatusCode != http.StatusNoContent {
		t.Fatalf("rotated csrf expected 204, got %d body=%s", newResp.StatusCode, newBody)
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
	for _, name := range []string{sessionCookieName, csrfCookieName} {
		if c := cookieByName(resp.Cookies(), name); c == nil || c.MaxAge != -1 {
			t.Fatalf("logout should clear %s cookie", name)
		}
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

	tempPassword, err := authV2Srv.authService.ResetPassword(context.Background(), username)
	if err != nil {
		t.Fatal(err)
	}

	assertMustChange := func(label string, body []byte) map[string]any {
		t.Helper()
		var result map[string]any
		if err := json.Unmarshal(body, &result); err != nil {
			t.Fatalf("%s response decode: %v", label, err)
		}
		user, ok := result["user"].(map[string]any)
		if !ok {
			t.Fatalf("%s response missing user: %s", label, body)
		}
		mustChange, ok := user["must_change_password"].(bool)
		if !ok || !mustChange {
			t.Fatalf("%s user.must_change_password = %v; want true", label, user["must_change_password"])
		}
		return result
	}

	loginResp, loginBody := loginAuthUser(t, username, tempPassword, nil)
	if loginResp.StatusCode != http.StatusOK {
		t.Fatalf("login with temp password expected 200, got %d body=%s", loginResp.StatusCode, loginBody)
	}
	assertMustChange("login", loginBody)

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

	meResp, meBody := authJSON(t, http.MethodGet, "/api/auth/me", nil, map[string]string{
		"Cookie": withCookies(loginResp),
	})
	if meResp.StatusCode != http.StatusOK {
		t.Fatalf("me should be allowed, got %d body=%s", meResp.StatusCode, meBody)
	}
	sessionCookie := cookieByName(loginResp.Cookies(), sessionCookieName)
	rotatedCSRF := cookieByName(meResp.Cookies(), csrfCookieName)
	if sessionCookie == nil || rotatedCSRF == nil || rotatedCSRF.Value == "" {
		t.Fatal("must-change web flow missing session or rotated csrf cookie")
	}

	webLogoutResp, webLogoutBody := authJSON(
		t,
		http.MethodPost,
		"/api/auth/logout",
		nil,
		map[string]string{
			"Cookie": sessionCookieName + "=" + sessionCookie.Value +
				"; " + csrfCookieName + "=" + rotatedCSRF.Value,
			"Origin":       authTestOrigin,
			"X-CSRF-Token": rotatedCSRF.Value,
		},
	)
	if webLogoutResp.StatusCode != http.StatusNoContent {
		t.Fatalf(
			"must-change web logout expected 204, got %d body=%s",
			webLogoutResp.StatusCode,
			webLogoutBody,
		)
	}
	for _, name := range []string{sessionCookieName, csrfCookieName} {
		if cookie := cookieByName(webLogoutResp.Cookies(), name); cookie == nil || cookie.MaxAge != -1 {
			t.Fatalf("must-change web logout should clear %s cookie", name)
		}
	}
	revokedWebMeResp, revokedWebMeBody := authJSON(
		t,
		http.MethodGet,
		"/api/auth/me",
		nil,
		map[string]string{"Cookie": withCookies(loginResp)},
	)
	if revokedWebMeResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf(
			"logged-out must-change web me expected 401, got %d body=%s",
			revokedWebMeResp.StatusCode,
			revokedWebMeBody,
		)
	}

	deviceResp, deviceBody := authJSON(t, http.MethodPost, "/api/auth/device-login", map[string]string{
		"username":    username,
		"password":    tempPassword,
		"device_name": "must-change-device",
	}, nil)
	if deviceResp.StatusCode != http.StatusOK {
		t.Fatalf("device-login with temp password expected 200, got %d body=%s", deviceResp.StatusCode, deviceBody)
	}
	deviceResult := assertMustChange("device-login", deviceBody)
	credential, ok := deviceResult["device_credential"].(string)
	if !ok || credential == "" {
		t.Fatalf("device-login response missing credential: %s", deviceBody)
	}
	deviceID, ok := deviceResult["device_id"].(string)
	if !ok || deviceID == "" {
		t.Fatalf("device-login response missing device_id: %s", deviceBody)
	}
	deviceHeader := map[string]string{"Authorization": "Device " + credential}

	deviceProtectedResp, deviceProtectedBody := authJSON(
		t,
		http.MethodGet,
		"/api/auth/test-protected",
		nil,
		deviceHeader,
	)
	if deviceProtectedResp.StatusCode != http.StatusForbidden {
		t.Fatalf(
			"must-change device protected route expected 403, got %d body=%s",
			deviceProtectedResp.StatusCode,
			deviceProtectedBody,
		)
	}
	var deviceErr map[string]string
	if err := json.Unmarshal(deviceProtectedBody, &deviceErr); err != nil {
		t.Fatal(err)
	}
	if deviceErr["error"] != "password change required" {
		t.Fatalf("device protected error = %q; want password change required", deviceErr["error"])
	}

	deviceMeResp, deviceMeBody := authJSON(t, http.MethodGet, "/api/auth/me", nil, deviceHeader)
	if deviceMeResp.StatusCode != http.StatusOK {
		t.Fatalf("must-change device me expected 200, got %d body=%s", deviceMeResp.StatusCode, deviceMeBody)
	}

	deviceLogoutResp, deviceLogoutBody := authJSON(
		t,
		http.MethodPost,
		"/api/auth/device-logout",
		nil,
		deviceHeader,
	)
	if deviceLogoutResp.StatusCode != http.StatusNoContent {
		t.Fatalf(
			"must-change device logout expected 204, got %d body=%s",
			deviceLogoutResp.StatusCode,
			deviceLogoutBody,
		)
	}

	store := auth.NewStore(authV2Rdb)
	if _, err := store.Device(context.Background(), deviceID); !errors.Is(err, auth.ErrNotFound) {
		t.Fatalf("Device() after must-change logout error = %v; want ErrNotFound", err)
	}
	revokedMeResp, revokedMeBody := authJSON(t, http.MethodGet, "/api/auth/me", nil, deviceHeader)
	if revokedMeResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf(
			"logged-out must-change device me expected 401, got %d body=%s",
			revokedMeResp.StatusCode,
			revokedMeBody,
		)
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

func TestNewWithOptionsCookieTransport(t *testing.T) {
	tests := []struct {
		name         string
		publicURL    string
		cookieSecure bool
		wantPanic    bool
	}{
		{
			name:      "localhost insecure allowed case insensitive",
			publicURL: "http://LOCALHOST:8080",
		},
		{
			name:      "IPv4 loopback insecure allowed",
			publicURL: "http://127.0.0.1:8080",
		},
		{
			name:      "IPv6 loopback insecure allowed",
			publicURL: "http://[::1]:8080",
		},
		{
			name:         "public HTTPS secure allowed",
			publicURL:    "https://app.example.com",
			cookieSecure: true,
		},
		{
			name:      "public HTTP insecure rejected",
			publicURL: "http://app.example.com",
			wantPanic: true,
		},
		{
			name:         "public HTTP secure rejected",
			publicURL:    "http://app.example.com",
			cookieSecure: true,
			wantPanic:    true,
		},
		{
			name:      "public HTTPS insecure rejected",
			publicURL: "https://app.example.com",
			wantPanic: true,
		},
		{
			name:         "localhost HTTP secure rejected",
			publicURL:    "http://localhost:8080",
			cookieSecure: true,
			wantPanic:    true,
		},
		{
			name:         "unsupported scheme rejected",
			publicURL:    "ftp://app.example.com",
			cookieSecure: true,
			wantPanic:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			panicked := func() (panicked bool) {
				defer func() {
					panicked = recover() != nil
				}()
				NewWithOptions(ServerOptions{
					DataDir:      t.TempDir(),
					CookieSecure: tt.cookieSecure,
					PublicURL:    tt.publicURL,
				})
				return false
			}()
			if panicked != tt.wantPanic {
				t.Fatalf(
					"NewWithOptions(PublicURL=%q, CookieSecure=%v) panicked=%v; want %v",
					tt.publicURL,
					tt.cookieSecure,
					panicked,
					tt.wantPanic,
				)
			}
		})
	}
}

func TestProductionMuxOmitsAuthTestRoutes(t *testing.T) {
	srv := NewWithOptions(ServerOptions{
		DataDir:   t.TempDir(),
		PublicURL: "http://localhost:8080",
	})
	handler := srv.authMiddleware(srv.mux)

	for _, path := range []string{
		"/api/auth/test-protected",
		"/api/auth/test-actor",
	} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			resp := httptest.NewRecorder()
			handler.ServeHTTP(resp, req)
			if resp.Code != http.StatusNotFound && resp.Code != http.StatusMethodNotAllowed {
				t.Fatalf("production route %s returned %d; want 404 or 405", path, resp.Code)
			}
		})
	}
}

func TestAuthTestOnlyProbeRequiresIdentity(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	resp, body := authJSON(t, http.MethodGet, "/api/auth/test-protected", nil, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("test-only protected route expected 401, got %d body=%s", resp.StatusCode, body)
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
