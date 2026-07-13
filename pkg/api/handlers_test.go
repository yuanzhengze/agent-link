package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/team/agentlink/pkg/redis"
)

var testRdb *redis.Client
var ts *httptest.Server
var testDataDir string

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

	srv := New("", testDataDir, rdb, "test-password")
	ts = httptest.NewServer(srv.authMiddleware(srv.mux))

	code := m.Run()

	ts.Close()
	cleanupTestData()
	os.RemoveAll(testDataDir)
	rdb.Close()
	os.Exit(code)
}

func cleanupTestData() {
	ctx := context.Background()
	for _, pattern := range []string{"agentlink:device:*", "agentlink:api_key:*", "agentlink:inbox:*", "agentlink:task:*", "agentlink:tasks:*", "agentlink:project:*", "agentlink:projects"} {
		keys, _ := testRdb.Keys(ctx, pattern).Result()
		if len(keys) > 0 {
			testRdb.Del(ctx, keys...)
		}
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

func TestRegister(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		body := `{"device":"reg-test","sessions":["main","worker"],"register_password":"test-password"}`
		resp, err := http.Post(ts.URL+"/agents/register", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}

		var rr RegisterResponse
		json.NewDecoder(resp.Body).Decode(&rr)

		if rr.Device != "reg-test" {
			t.Errorf("expected device reg-test, got %s", rr.Device)
		}
		if len(rr.Sessions) != 2 {
			t.Errorf("expected 2 sessions, got %d", len(rr.Sessions))
		}
		if !strings.HasPrefix(rr.APIKey, "sk_live_") {
			t.Errorf("api key should start with sk_live_, got %s", rr.APIKey)
		}
		if len(rr.APIKey) != len("sk_live_")+64 {
			t.Errorf("expected api key length %d, got %d", len("sk_live_")+64, len(rr.APIKey))
		}
		if rr.RegisteredAt == "" {
			t.Error("registered_at should not be empty")
		}

		// Verify Redis hash
		exists, _ := testRdb.Exists(context.Background(), "agentlink:device:reg-test").Result()
		if exists != 1 {
			t.Error("agentlink:device:reg-test should exist in Redis")
		}

		// Verify API key hash matches
		storedHash, _ := testRdb.HGet(context.Background(), "agentlink:device:reg-test", "api_key_hash").Result()
		computedHash := sha256.Sum256([]byte(rr.APIKey))
		if storedHash != hex.EncodeToString(computedHash[:]) {
			t.Error("stored api_key_hash does not match computed hash")
		}

		// Verify reverse index
		revDevice, _ := testRdb.Get(context.Background(), "agentlink:api_key:"+storedHash).Result()
		if revDevice != "reg-test" {
			t.Errorf("reverse index should point to reg-test, got %s", revDevice)
		}
	})

	t.Run("wrong password", func(t *testing.T) {
		body := `{"device":"wp-test","sessions":["main"],"register_password":"wrong"}`
		resp, err := http.Post(ts.URL+"/agents/register", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("expected 401, got %d", resp.StatusCode)
		}
	})

	t.Run("invalid device name", func(t *testing.T) {
		body := `{"device":"INVALID","sessions":["main"],"register_password":"test-password"}`
		resp, err := http.Post(ts.URL+"/agents/register", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", resp.StatusCode)
		}
	})

	t.Run("empty sessions", func(t *testing.T) {
		body := `{"device":"es-test","sessions":[],"register_password":"test-password"}`
		resp, err := http.Post(ts.URL+"/agents/register", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", resp.StatusCode)
		}
	})

	t.Run("duplicate device reuse", func(t *testing.T) {
		body := `{"device":"reg-test","sessions":["main"],"register_password":"test-password"}`
		resp, err := http.Post(ts.URL+"/agents/register", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected 200 (reuse), got %d", resp.StatusCode)
		}

		var rr RegisterResponse
		json.NewDecoder(resp.Body).Decode(&rr)
		if rr.APIKey == "" {
			t.Error("expected api_key in response")
		}
	})

	t.Run("duplicate device wrong password", func(t *testing.T) {
		body := `{"device":"reg-test","sessions":["main"],"register_password":"wrong"}`
		resp, err := http.Post(ts.URL+"/agents/register", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("expected 401 for wrong password, got %d", resp.StatusCode)
		}
	})
}

func TestAuthMiddleware(t *testing.T) {
	// Setup: insert a test device directly in Redis
	ctx := context.Background()
	testDevice := "auth-device"
	testKey := "sk_live_" + strings.Repeat("a", 64)
	hash := sha256.Sum256([]byte(testKey))
	hashHex := hex.EncodeToString(hash[:])

	testRdb.HSet(ctx, "agentlink:device:"+testDevice,
		"sessions", `["main"]`,
		"api_key_hash", hashHex,
		"registered_at", "2026-01-01T00:00:00Z",
		"last_seen", "2026-01-01T00:00:00Z",
	)
	testRdb.Set(ctx, "agentlink:api_key:"+hashHex, testDevice, 0)

	t.Cleanup(func() {
		testRdb.Del(ctx, "agentlink:device:"+testDevice, "agentlink:api_key:"+hashHex)
	})

	t.Run("health is public", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/health")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected 200, got %d", resp.StatusCode)
		}
	})

	t.Run("register is public", func(t *testing.T) {
		// A request to register without auth should work (no Bearer needed)
		body := `{"device":"mid-public","sessions":["main"],"register_password":"test-password"}`
		resp, err := http.Post(ts.URL+"/agents/register", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected 200, got %d", resp.StatusCode)
		}
		// Clean up
		testRdb.Del(ctx, "agentlink:device:mid-public")
		h := sha256.Sum256([]byte("sk_live_"))
		testRdb.Del(ctx, "agentlink:api_key:"+hex.EncodeToString(h[:]))
	})

	t.Run("no token returns 401", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/messages/send")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("expected 401, got %d", resp.StatusCode)
		}
	})

	t.Run("empty bearer returns 401", func(t *testing.T) {
		req, _ := http.NewRequest("GET", ts.URL+"/nonexistent", nil)
		req.Header.Set("Authorization", "Bearer")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("expected 401, got %d", resp.StatusCode)
		}
	})

	t.Run("invalid token returns 401", func(t *testing.T) {
		req, _ := http.NewRequest("GET", ts.URL+"/nonexistent", nil)
		req.Header.Set("Authorization", "Bearer invalid")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("expected 401, got %d", resp.StatusCode)
		}
	})

	t.Run("valid token passes auth", func(t *testing.T) {
		req, _ := http.NewRequest("GET", ts.URL+"/nonexistent", nil)
		req.Header.Set("Authorization", "Bearer "+testKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		// 404 = auth passed, route just doesn't exist
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("expected 404 (auth passed), got %d", resp.StatusCode)
		}
	})
}

func TestGenerateAPIKey(t *testing.T) {
	raw, hash, err := generateAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(raw, "sk_live_") {
		t.Errorf("expected sk_live_ prefix, got %s", raw)
	}
	if len(raw) != len("sk_live_")+64 {
		t.Errorf("expected raw key length %d, got %d", len("sk_live_")+64, len(raw))
	}
	// Verify hash matches
	computed := sha256.Sum256([]byte(raw))
	if hash != hex.EncodeToString(computed[:]) {
		t.Error("hash does not match raw key")
	}
	// Verify uniqueness
	raw2, _, _ := generateAPIKey()
	if raw == raw2 {
		t.Error("consecutive keys should be unique")
	}
}

func TestMessages(t *testing.T) {
	ctx := context.Background()

	// Register a device for message tests
	regBody := `{"device":"msg-test","sessions":["main","worker"],"register_password":"test-password"}`
	resp, err := http.Post(ts.URL+"/agents/register", "application/json", strings.NewReader(regBody))
	if err != nil {
		t.Fatal(err)
	}
	var regResp RegisterResponse
	json.NewDecoder(resp.Body).Decode(&regResp)
	resp.Body.Close()
	apiKey := regResp.APIKey

	hash := sha256.Sum256([]byte(apiKey))
	hashHex := hex.EncodeToString(hash[:])

	t.Cleanup(func() {
		testRdb.Del(ctx, "agentlink:device:msg-test", "agentlink:api_key:"+hashHex, "agentlink:inbox:msg-test:main", "agentlink:inbox:msg-test:worker")
	})

	// --- POST /messages/send ---

	t.Run("send success", func(t *testing.T) {
		body := `{"to":"msg-test:worker","from_session":"main","content":"hello"}`
		req, _ := http.NewRequest("POST", ts.URL+"/messages/send", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}
		var sr SendResponse
		json.NewDecoder(resp.Body).Decode(&sr)
		if sr.ID == "" {
			t.Error("expected non-empty id")
		}
		if len(sr.ID) != 32 {
			t.Errorf("expected id length 32, got %d", len(sr.ID))
		}

		// Verify message in Redis
		data, err := testRdb.LIndex(ctx, "agentlink:inbox:msg-test:worker", 0).Result()
		if err != nil {
			t.Fatal("message not found in inbox")
		}
		var msg Message
		json.Unmarshal([]byte(data), &msg)
		if msg.ID != sr.ID {
			t.Errorf("id mismatch: %s vs %s", msg.ID, sr.ID)
		}
		if msg.Type != "msg" {
			t.Errorf("expected type=msg, got %s", msg.Type)
		}
		if msg.FromDevice != "msg-test" {
			t.Errorf("expected from_device=msg-test, got %s", msg.FromDevice)
		}
		if msg.FromSession != "main" {
			t.Errorf("expected from_session=main, got %s", msg.FromSession)
		}
		if msg.Content != "hello" {
			t.Errorf("expected content=hello, got %s", msg.Content)
		}
		if msg.Title != "hello" {
			t.Errorf("expected default title=hello, got %s", msg.Title)
		}
		if msg.CreatedAt == "" {
			t.Error("created_at should not be empty")
		}
	})

	t.Run("send with explicit title", func(t *testing.T) {
		body := `{"to":"msg-test:worker","from_session":"main","title":"紧急通知","content":"hello"}`
		req, _ := http.NewRequest("POST", ts.URL+"/messages/send", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}

		data, _ := testRdb.LIndex(ctx, "agentlink:inbox:msg-test:worker", 0).Result()
		var msg Message
		json.Unmarshal([]byte(data), &msg)
		if msg.Title != "紧急通知" {
			t.Errorf("expected title=紧急通知, got %s", msg.Title)
		}
	})

	t.Run("send default title truncates long content", func(t *testing.T) {
		longContent := strings.Repeat("a", 50)
		body := fmt.Sprintf(`{"to":"msg-test:worker","from_session":"main","content":"%s"}`, longContent)
		req, _ := http.NewRequest("POST", ts.URL+"/messages/send", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}

		data, _ := testRdb.LIndex(ctx, "agentlink:inbox:msg-test:worker", 0).Result()
		var msg Message
		json.Unmarshal([]byte(data), &msg)
		expected := strings.Repeat("a", 40) + "..."
		if msg.Title != expected {
			t.Errorf("expected truncated title %q, got %q", expected, msg.Title)
		}
	})

	t.Run("send does not persist msg record", func(t *testing.T) {
		body := `{"to":"msg-test:worker","from_session":"main","content":"no persist"}`
		req, _ := http.NewRequest("POST", ts.URL+"/messages/send", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var sr SendResponse
		json.NewDecoder(resp.Body).Decode(&sr)

		// No agentlink:msg:<id> key should exist (msg is fire-and-forget)
		exists, _ := testRdb.Exists(ctx, "agentlink:msg:"+sr.ID).Result()
		if exists != 0 {
			t.Errorf("expected no msg record for id %s, but key exists", sr.ID)
		}
	})

	t.Run("messages status route removed", func(t *testing.T) {
		req, _ := http.NewRequest("GET", ts.URL+"/messages/status?id=whatever", nil)
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("expected 404 for removed /messages/status route, got %d", resp.StatusCode)
		}
	})

	t.Run("send missing to", func(t *testing.T) {
		body := `{"from_session":"main","content":"hi"}`
		req, _ := http.NewRequest("POST", ts.URL+"/messages/send", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", resp.StatusCode)
		}
	})

	t.Run("send missing from_session", func(t *testing.T) {
		body := `{"to":"msg-test:worker","content":"hi"}`
		req, _ := http.NewRequest("POST", ts.URL+"/messages/send", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", resp.StatusCode)
		}
	})

	t.Run("send empty content", func(t *testing.T) {
		body := `{"to":"msg-test:worker","from_session":"main","content":""}`
		req, _ := http.NewRequest("POST", ts.URL+"/messages/send", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", resp.StatusCode)
		}
	})

	t.Run("send content too long", func(t *testing.T) {
		content := strings.Repeat("x", 3001)
		body := fmt.Sprintf(`{"to":"msg-test:worker","from_session":"main","content":%q}`, content)
		req, _ := http.NewRequest("POST", ts.URL+"/messages/send", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("expected 400 for content>3000, got %d", resp.StatusCode)
		}
	})

	t.Run("send content exactly 3000", func(t *testing.T) {
		content := strings.Repeat("x", 3000)
		body := fmt.Sprintf(`{"to":"msg-test:worker","from_session":"main","content":%q}`, content)
		req, _ := http.NewRequest("POST", ts.URL+"/messages/send", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected 200 for 3000-char content, got %d", resp.StatusCode)
		}
	})

	t.Run("send invalid target format", func(t *testing.T) {
		body := `{"to":"worker","from_session":"main","content":"hi"}`
		req, _ := http.NewRequest("POST", ts.URL+"/messages/send", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("expected 400 for no colon, got %d", resp.StatusCode)
		}
	})

	t.Run("send invalid target device name", func(t *testing.T) {
		body := `{"to":"BAD:worker","from_session":"main","content":"hi"}`
		req, _ := http.NewRequest("POST", ts.URL+"/messages/send", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("expected 400 for bad device name, got %d", resp.StatusCode)
		}
	})

	t.Run("send target device not found", func(t *testing.T) {
		body := `{"to":"nonexistent:main","from_session":"main","content":"hi"}`
		req, _ := http.NewRequest("POST", ts.URL+"/messages/send", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("expected 404 for unknown device, got %d", resp.StatusCode)
		}
	})

	t.Run("send target session not found", func(t *testing.T) {
		body := `{"to":"msg-test:reviewer","from_session":"main","content":"hi"}`
		req, _ := http.NewRequest("POST", ts.URL+"/messages/send", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("expected 404 for unknown session, got %d", resp.StatusCode)
		}
	})

	t.Run("send no auth", func(t *testing.T) {
		body := `{"to":"msg-test:worker","from_session":"main","content":"hi"}`
		resp, err := http.Post(ts.URL+"/messages/send", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("expected 401, got %d", resp.StatusCode)
		}
	})

	t.Run("send invalid auth", func(t *testing.T) {
		body := `{"to":"msg-test:worker","from_session":"main","content":"hi"}`
		req, _ := http.NewRequest("POST", ts.URL+"/messages/send", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer invalid-key")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("expected 401, got %d", resp.StatusCode)
		}
	})

	// --- GET /inbox/pull ---

	// Pre-load 5 messages for pull tests
	testRdb.Del(ctx, "agentlink:inbox:msg-test:worker")
	for i := 0; i < 5; i++ {
		msg := Message{
			ID:          fmt.Sprintf("msg-%d", i),
			Type:        "msg",
			FromDevice:  "msg-test",
			FromSession: "main",
			Content:     fmt.Sprintf("message %d", i),
			CreatedAt:   "2026-01-01T00:00:00Z",
		}
		data, _ := json.Marshal(msg)
		testRdb.LPush(ctx, "agentlink:inbox:msg-test:worker", data)
	}

	t.Run("pull success", func(t *testing.T) {
		req, _ := http.NewRequest("GET", ts.URL+"/inbox/pull?session=worker&limit=1", nil)
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}
		var pr PullResponse
		json.NewDecoder(resp.Body).Decode(&pr)
		if len(pr.Items) != 1 {
			t.Fatalf("expected 1 item, got %d", len(pr.Items))
		}
		if pr.Items[0].Content != "message 0" {
			t.Errorf("expected 'message 0', got %s", pr.Items[0].Content)
		}
	})

	t.Run("pull consumes messages", func(t *testing.T) {
		rem, _ := testRdb.LLen(ctx, "agentlink:inbox:msg-test:worker").Result()
		if rem != 4 {
			t.Errorf("expected 4 remaining after 1 pull, got %d", rem)
		}
	})

	t.Run("pull multiple with limit", func(t *testing.T) {
		req, _ := http.NewRequest("GET", ts.URL+"/inbox/pull?session=worker&limit=3", nil)
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}
		var pr PullResponse
		json.NewDecoder(resp.Body).Decode(&pr)
		if len(pr.Items) != 3 {
			t.Errorf("expected 3 items, got %d", len(pr.Items))
		}
		rem, _ := testRdb.LLen(ctx, "agentlink:inbox:msg-test:worker").Result()
		if rem != 1 {
			t.Errorf("expected 1 remaining, got %d", rem)
		}
	})

	t.Run("pull default limit", func(t *testing.T) {
		msg := Message{ID: "default-limit", Type: "msg", FromDevice: "msg-test", FromSession: "main", Content: "test", CreatedAt: "2026-01-01T00:00:00Z"}
		data, _ := json.Marshal(msg)
		testRdb.LPush(ctx, "agentlink:inbox:msg-test:worker", data)

		req, _ := http.NewRequest("GET", ts.URL+"/inbox/pull?session=worker", nil)
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		var pr PullResponse
		json.NewDecoder(resp.Body).Decode(&pr)
		if len(pr.Items) != 1 {
			t.Errorf("default limit should be 1, got %d", len(pr.Items))
		}
	})

	t.Run("pull limit zero defaults to 1", func(t *testing.T) {
		msg := Message{ID: "zero-limit", Type: "msg", FromDevice: "msg-test", FromSession: "main", Content: "test", CreatedAt: "2026-01-01T00:00:00Z"}
		data, _ := json.Marshal(msg)
		testRdb.LPush(ctx, "agentlink:inbox:msg-test:worker", data)

		req, _ := http.NewRequest("GET", ts.URL+"/inbox/pull?session=worker&limit=0", nil)
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}
		var pr PullResponse
		json.NewDecoder(resp.Body).Decode(&pr)
		if len(pr.Items) != 1 {
			t.Errorf("limit=0 should default to 1, got %d", len(pr.Items))
		}
	})

	t.Run("pull limit capped at 100", func(t *testing.T) {
		req, _ := http.NewRequest("GET", ts.URL+"/inbox/pull?session=worker&limit=200", nil)
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}
		var pr PullResponse
		json.NewDecoder(resp.Body).Decode(&pr)
		if pr.Items == nil {
			t.Error("expected non-nil items")
		}
	})

	t.Run("pull negative limit returns 400", func(t *testing.T) {
		req, _ := http.NewRequest("GET", ts.URL+"/inbox/pull?session=worker&limit=-1", nil)
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("expected 400 for negative limit, got %d", resp.StatusCode)
		}
	})

	t.Run("pull missing session", func(t *testing.T) {
		req, _ := http.NewRequest("GET", ts.URL+"/inbox/pull?limit=1", nil)
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", resp.StatusCode)
		}
	})

	t.Run("pull no auth", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/inbox/pull?session=worker&limit=1")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("expected 401, got %d", resp.StatusCode)
		}
	})

	t.Run("pull empty inbox", func(t *testing.T) {
		req, _ := http.NewRequest("GET", ts.URL+"/inbox/pull?session=main", nil)
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}
		var pr PullResponse
		json.NewDecoder(resp.Body).Decode(&pr)
		if len(pr.Items) != 0 {
			t.Errorf("expected 0 items, got %d", len(pr.Items))
		}
	})

	// --- Integration ---

	t.Run("send then pull end-to-end", func(t *testing.T) {
		body := `{"to":"msg-test:main","from_session":"worker","content":"request: check status"}`
		req, _ := http.NewRequest("POST", ts.URL+"/messages/send", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()

		req2, err := http.NewRequest("GET", ts.URL+"/inbox/pull?session=main&limit=1", nil)
		req2.Header.Set("Authorization", "Bearer "+apiKey)
		resp2, err := http.DefaultClient.Do(req2)
		if err != nil {
			t.Fatal(err)
		}
		defer resp2.Body.Close()

		var pr PullResponse
		json.NewDecoder(resp2.Body).Decode(&pr)
		if len(pr.Items) != 1 {
			t.Fatalf("expected 1 item, got %d", len(pr.Items))
		}
		if pr.Items[0].FromDevice != "msg-test" {
			t.Errorf("expected from_device=msg-test, got %s", pr.Items[0].FromDevice)
		}
		if pr.Items[0].FromSession != "worker" {
			t.Errorf("expected from_session=worker, got %s", pr.Items[0].FromSession)
		}
		if pr.Items[0].Content != "request: check status" {
			t.Errorf("content mismatch: %s", pr.Items[0].Content)
		}
	})
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
