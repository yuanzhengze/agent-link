package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/team/agentlink/pkg/redis"
)

type FixedClock struct{ T time.Time }

func (c FixedClock) Now() time.Time { return c.T }

func newAuthService(t *testing.T, clock Clock) (*Service, *Store, *redis.Client) {
	t.Helper()
	store, rdb := newAuthTestStore(t)
	return NewService(store, clock), store, rdb
}

func registerServiceUser(t *testing.T, svc *Service, username, password string) User {
	t.Helper()
	result, err := svc.Register(context.Background(), RegisterInput{
		Username: username,
		Password: password,
	})
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	return result.User
}

func loginWebSession(t *testing.T, svc *Service, username, password, ip string) WebLoginResult {
	t.Helper()
	result, err := svc.Login(context.Background(), LoginInput{
		Username: username,
		Password: password,
		IP:       ip,
	})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	return result
}

func loginDeviceSession(t *testing.T, svc *Service, username, password, ip, deviceName string) DeviceLoginResult {
	t.Helper()
	result, err := svc.DeviceLogin(context.Background(), DeviceLoginInput{
		Username:   username,
		Password:   password,
		IP:         ip,
		DeviceName: deviceName,
	})
	if err != nil {
		t.Fatalf("DeviceLogin() error = %v", err)
	}
	return result
}

func assertSecretShape(t *testing.T, secret, prefix string, rawBytes int) {
	t.Helper()
	if !strings.HasPrefix(secret, prefix) {
		t.Fatalf("secret %q missing prefix %q", secret, prefix)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(secret, prefix))
	if err != nil {
		t.Fatalf("decode secret %q: %v", secret, err)
	}
	if len(decoded) != rawBytes {
		t.Fatalf("secret %q encoded %d bytes; want %d", secret, len(decoded), rawBytes)
	}
}

func assertRedisStoresOnlyHash(t *testing.T, rdb *redis.Client, secret string) {
	t.Helper()
	want := secretHash(secret)
	ctx := context.Background()
	keys, err := rdb.Keys(ctx, "agentlink:v2:*").Result()
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		switch rdb.Type(ctx, key).Val() {
		case "hash":
			fields, err := rdb.HGetAll(ctx, key).Result()
			if err != nil {
				t.Fatal(err)
			}
			for field, value := range fields {
				if value == secret {
					t.Fatalf("redis key %q field %q stores plaintext secret", key, field)
				}
			}
		case "string":
			value, err := rdb.Get(ctx, key).Result()
			if err != nil {
				t.Fatal(err)
			}
			if value == secret {
				t.Fatalf("redis key %q stores plaintext secret", key)
			}
		}
	}
	if want == "" {
		t.Fatal("secret hash is empty")
	}
}

func TestServiceRegisterHashesPassword(t *testing.T) {
	clock := FixedClock{T: time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)}
	svc, store, rdb := newAuthService(t, clock)
	username := uniqueTestUsername(t)
	password := "correct horse battery staple"
	normalized, err := NormalizeUsername(username)
	if err != nil {
		t.Fatal(err)
	}
	cleanupAuthKeys(t, rdb, usernameKey(normalized))

	result, err := svc.Register(context.Background(), RegisterInput{
		Username: username,
		Password: password,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.User.Username != username || result.User.Status != "active" || result.User.MustChangePassword {
		t.Fatalf("Register() user = %+v; want active user without must-change", result.User)
	}
	assertSecretShape(t, result.SessionSecret, "ws_", 32)
	assertSecretShape(t, result.CSRFSecret, "csrf_", 32)
	assertRedisStoresOnlyHash(t, rdb, result.SessionSecret)
	assertRedisStoresOnlyHash(t, rdb, result.CSRFSecret)

	stored, err := store.UserByUsername(context.Background(), username)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := VerifyPassword(stored.PasswordPHC, password); err != nil || !ok {
		t.Fatalf("stored password verify = %v, %v; want true, nil", ok, err)
	}
	if stored.PasswordPHC == password {
		t.Fatal("password stored in plaintext")
	}

	_, _, err = svc.ResolveWebSession(context.Background(), result.SessionSecret)
	if err != nil {
		t.Fatalf("ResolveWebSession() after register error = %v", err)
	}
}

func TestServiceLoginReturnsGenericInvalidCredentials(t *testing.T) {
	clock := FixedClock{T: time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)}
	svc, _, rdb := newAuthService(t, clock)
	username := uniqueTestUsername(t)
	password := "correct horse battery staple"
	registerServiceUser(t, svc, username, password)
	ip := "203.0.113.10"
	clearAuthKeys(t, rdb, loginFailIPKey(ip))

	_, err := svc.Login(context.Background(), LoginInput{
		Username: uniqueTestUsername(t),
		Password: password,
		IP:       "203.0.113.10",
	})
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("Login() unknown user error = %v; want ErrInvalidCredentials", err)
	}

	_, err = svc.Login(context.Background(), LoginInput{
		Username: username,
		Password: "wrong password value",
		IP:       ip,
	})
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("Login() wrong password error = %v; want ErrInvalidCredentials", err)
	}
	_ = rdb
}

func TestServiceLoginRateLimitFiveFailures(t *testing.T) {
	clock := FixedClock{T: time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)}
	svc, _, rdb := newAuthService(t, clock)
	username := uniqueTestUsername(t)
	password := "correct horse battery staple"
	registerServiceUser(t, svc, username, password)
	ip := "203.0.113.55"
	normalized, err := NormalizeUsername(username)
	if err != nil {
		t.Fatal(err)
	}
	clearAuthKeys(t, rdb, loginFailUserKey(normalized), loginFailIPKey(ip))

	for i := 0; i < 4; i++ {
		_, err := svc.Login(context.Background(), LoginInput{
			Username: username,
			Password: "wrong password value",
			IP:       ip,
		})
		if !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("failure %d error = %v; want ErrInvalidCredentials", i+1, err)
		}
	}
	_, err = svc.Login(context.Background(), LoginInput{
		Username: username,
		Password: "wrong password value",
		IP:       ip,
	})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("5th Login() error = %v; want ErrRateLimited", err)
	}
	_, err = svc.Login(context.Background(), LoginInput{
		Username: username,
		Password: password,
		IP:       ip,
	})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("Login() after rate limit error = %v; want ErrRateLimited", err)
	}
	_ = rdb
}

func TestServiceChangePasswordRevokesAllSessions(t *testing.T) {
	clock := FixedClock{T: time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)}
	svc, _, _ := newAuthService(t, clock)
	username := uniqueTestUsername(t)
	oldPassword := "correct horse battery staple"
	user := registerServiceUser(t, svc, username, oldPassword)
	web := loginWebSession(t, svc, username, oldPassword, "203.0.113.1")
	device := loginDeviceSession(t, svc, username, oldPassword, "203.0.113.1", "workstation")

	newPassword := "new horse battery staple"
	if err := svc.ChangePassword(context.Background(), user.ID, oldPassword, newPassword); err != nil {
		t.Fatal(err)
	}

	if _, _, err := svc.ResolveWebSession(context.Background(), web.SessionSecret); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("ResolveWebSession() after change error = %v; want ErrSessionExpired", err)
	}
	if _, _, err := svc.ResolveDeviceSession(context.Background(), device.DeviceCredential); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("ResolveDeviceSession() after change error = %v; want ErrSessionExpired", err)
	}
	if _, err := svc.Login(context.Background(), LoginInput{
		Username: username,
		Password: oldPassword,
		IP:       "203.0.113.1",
	}); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("Login() with old password error = %v; want ErrInvalidCredentials", err)
	}
	loginWebSession(t, svc, username, newPassword, "203.0.113.1")
}

func TestServiceDeviceLoginCreatesOwnedDevice(t *testing.T) {
	clock := FixedClock{T: time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)}
	svc, store, rdb := newAuthService(t, clock)
	username := uniqueTestUsername(t)
	password := "correct horse battery staple"
	user := registerServiceUser(t, svc, username, password)

	result := loginDeviceSession(t, svc, username, password, "203.0.113.20", "kirby-pc")
	if !strings.HasPrefix(result.DeviceID, "dev_") {
		t.Fatalf("DeviceID = %q; want dev_ prefix", result.DeviceID)
	}
	assertSecretShape(t, result.DeviceCredential, "ds_", 32)
	assertRedisStoresOnlyHash(t, rdb, result.DeviceCredential)

	device, err := store.Device(context.Background(), result.DeviceID)
	if err != nil {
		t.Fatal(err)
	}
	if device.UserID != user.ID || device.Name != "kirby-pc" {
		t.Fatalf("Device() = %+v; want owner %s name kirby-pc", device, user.ID)
	}
	if device.SessionHash != secretHash(result.DeviceCredential) {
		t.Fatalf("device session_hash = %q; want %q", device.SessionHash, secretHash(result.DeviceCredential))
	}

	member, err := rdb.SIsMember(context.Background(), userDevicesKey(user.ID), result.DeviceID).Result()
	if err != nil {
		t.Fatal(err)
	}
	if !member {
		t.Fatal("device missing from user devices set")
	}

	resolvedUser, session, err := svc.ResolveDeviceSession(context.Background(), result.DeviceCredential)
	if err != nil {
		t.Fatal(err)
	}
	if resolvedUser.ID != user.ID || session.DeviceID != result.DeviceID {
		t.Fatalf("ResolveDeviceSession() = user %+v session %+v; want owned device", resolvedUser, session)
	}
}

func TestServiceResetPasswordRequiresChangeAndRevokesSessions(t *testing.T) {
	clock := FixedClock{T: time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)}
	svc, store, _ := newAuthService(t, clock)
	username := uniqueTestUsername(t)
	password := "correct horse battery staple"
	user := registerServiceUser(t, svc, username, password)
	web := loginWebSession(t, svc, username, password, "203.0.113.2")
	device := loginDeviceSession(t, svc, username, password, "203.0.113.2", "reset-device")

	temp, err := svc.ResetPassword(context.Background(), username)
	if err != nil {
		t.Fatal(err)
	}
	if len(temp) != 20 {
		t.Fatalf("ResetPassword() length = %d; want 20", len(temp))
	}

	stored, err := store.UserByID(context.Background(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.MustChangePassword {
		t.Fatal("ResetPassword() did not set must_change_password")
	}
	if ok, err := VerifyPassword(stored.PasswordPHC, temp); err != nil || !ok {
		t.Fatalf("verify reset password = %v, %v; want true, nil", ok, err)
	}
	if _, _, err := svc.ResolveWebSession(context.Background(), web.SessionSecret); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("web session still valid after reset: %v", err)
	}
	if _, _, err := svc.ResolveDeviceSession(context.Background(), device.DeviceCredential); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("device session still valid after reset: %v", err)
	}
}

func TestLoginFailureExistingWrongPassword(t *testing.T) {
	svc, _, rdb := newAuthService(t, FixedClock{T: time.Now().UTC()})
	username := uniqueTestUsername(t)
	registerServiceUser(t, svc, username, "correct horse battery staple")
	ip := "203.0.113.30"
	clearAuthKeys(t, rdb, loginFailIPKey(ip))
	_, err := svc.Login(context.Background(), LoginInput{
		Username: username,
		Password: "wrong password value",
		IP:       "203.0.113.30",
	})
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("Login() error = %v; want ErrInvalidCredentials", err)
	}
}

func TestLoginFailureInactiveUser(t *testing.T) {
	svc, store, rdb := newAuthService(t, FixedClock{T: time.Now().UTC()})
	username := uniqueTestUsername(t)
	password := "correct horse battery staple"
	user := registerServiceUser(t, svc, username, password)
	ip := "203.0.113.31"
	normalized, err := NormalizeUsername(username)
	if err != nil {
		t.Fatal(err)
	}
	clearAuthKeys(t, rdb, loginFailUserKey(normalized), loginFailIPKey(ip))
	if err := store.SetUserStatus(context.Background(), user.ID, "inactive"); err != nil {
		t.Fatal(err)
	}
	_, err = svc.Login(context.Background(), LoginInput{
		Username: username,
		Password: password,
		IP:       ip,
	})
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("Login() inactive error = %v; want ErrInvalidCredentials", err)
	}
}

func TestLoginFailureEmptyIP(t *testing.T) {
	svc, _, _ := newAuthService(t, FixedClock{T: time.Now().UTC()})
	username := uniqueTestUsername(t)
	registerServiceUser(t, svc, username, "correct horse battery staple")
	for _, ip := range []string{"", "   ", "\t"} {
		_, err := svc.Login(context.Background(), LoginInput{
			Username: username,
			Password: "correct horse battery staple",
			IP:       ip,
		})
		if err == nil {
			t.Fatalf("Login() with IP %q error = nil; want validation error", ip)
		}
	}
}

func TestLoginFailureIPRateLimitSpansUsernames(t *testing.T) {
	svc, _, rdb := newAuthService(t, FixedClock{T: time.Now().UTC()})
	ip := "203.0.113.77"
	clearAuthKeys(t, rdb, loginFailIPKey(ip))
	for i := 0; i < 5; i++ {
		username := uniqueTestUsername(t)
		registerServiceUser(t, svc, username, "correct horse battery staple")
		_, err := svc.Login(context.Background(), LoginInput{
			Username: username,
			Password: "wrong password value",
			IP:       ip,
		})
		if i < 4 && !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("failure %d error = %v; want ErrInvalidCredentials", i+1, err)
		}
		if i == 4 && !errors.Is(err, ErrRateLimited) {
			t.Fatalf("5th failure error = %v; want ErrRateLimited", err)
		}
	}
}

func TestLoginFailureSuccessClearsCounters(t *testing.T) {
	svc, store, rdb := newAuthService(t, FixedClock{T: time.Now().UTC()})
	username := uniqueTestUsername(t)
	password := "correct horse battery staple"
	registerServiceUser(t, svc, username, password)
	ip := "203.0.113.88"
	normalized, err := NormalizeUsername(username)
	if err != nil {
		t.Fatal(err)
	}
	clearAuthKeys(t, rdb, loginFailUserKey(normalized), loginFailIPKey(ip))

	for i := 0; i < 3; i++ {
		_, err := svc.Login(context.Background(), LoginInput{
			Username: username,
			Password: "wrong password value",
			IP:       ip,
		})
		if !errors.Is(err, ErrInvalidCredentials) {
			t.Fatal(err)
		}
	}
	loginWebSession(t, svc, username, password, ip)

	userCount, err := rdb.Get(context.Background(), loginFailUserKey(normalized)).Int64()
	if err != nil && !errors.Is(err, goredis.Nil) {
		t.Fatal(err)
	}
	if userCount != 0 {
		t.Fatalf("user counter = %d after success; want cleared", userCount)
	}
	ipCount, err := rdb.Get(context.Background(), loginFailIPKey(ip)).Int64()
	if err != nil && !errors.Is(err, goredis.Nil) {
		t.Fatal(err)
	}
	if ipCount != 0 {
		t.Fatalf("ip counter = %d after success; want cleared", ipCount)
	}
	_ = store
}

func TestLoginFailureCounterTTLAndWrongType(t *testing.T) {
	svc, _, rdb := newAuthService(t, FixedClock{T: time.Now().UTC()})
	username := uniqueTestUsername(t)
	registerServiceUser(t, svc, username, "correct horse battery staple")
	ip := "203.0.113.99"
	normalized, err := NormalizeUsername(username)
	if err != nil {
		t.Fatal(err)
	}
	userKey := loginFailUserKey(normalized)
	ipKey := loginFailIPKey(ip)
	clearAuthKeys(t, rdb, userKey, ipKey)

	_, err = svc.Login(context.Background(), LoginInput{
		Username: username,
		Password: "wrong password value",
		IP:       ip,
	})
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatal(err)
	}
	ttl, err := rdb.TTL(context.Background(), userKey).Result()
	if err != nil {
		t.Fatal(err)
	}
	if ttl < 14*time.Minute || ttl > 15*time.Minute {
		t.Fatalf("user counter TTL = %s; want ~15m", ttl)
	}

	if err := rdb.Set(context.Background(), userKey, "not-a-counter", 0).Err(); err != nil {
		t.Fatal(err)
	}
	_, err = svc.Login(context.Background(), LoginInput{
		Username: username,
		Password: "wrong password value",
		IP:       ip,
	})
	if err == nil {
		t.Fatal("Login() with wrong-type counter error = nil; want error")
	}
	value, err := rdb.Get(context.Background(), userKey).Result()
	if err != nil {
		t.Fatal(err)
	}
	if value != "not-a-counter" {
		t.Fatalf("wrong-type counter mutated to %q", value)
	}
}

func TestLoginFailureConcurrentFifthAttempt(t *testing.T) {
	svc, _, rdb := newAuthService(t, FixedClock{T: time.Now().UTC()})
	username := uniqueTestUsername(t)
	registerServiceUser(t, svc, username, "correct horse battery staple")
	ip := "203.0.113.44"
	normalized, err := NormalizeUsername(username)
	if err != nil {
		t.Fatal(err)
	}
	clearAuthKeys(t, rdb, loginFailUserKey(normalized), loginFailIPKey(ip))

	for i := 0; i < 3; i++ {
		_, _ = svc.Login(context.Background(), LoginInput{
			Username: username,
			Password: "wrong password value",
			IP:       ip,
		})
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			_, results[idx] = svc.Login(context.Background(), LoginInput{
				Username: username,
				Password: "wrong password value",
				IP:       ip,
			})
		}(i)
	}
	close(start)
	wg.Wait()

	var rateLimited int
	for _, err := range results {
		if errors.Is(err, ErrRateLimited) {
			rateLimited++
		}
	}
	if rateLimited == 0 {
		t.Fatalf("concurrent failures = %v; want at least one ErrRateLimited", results)
	}
}

func TestDeviceCredentialAtomicCreateFailureLeavesNoResidue(t *testing.T) {
	store, rdb := newAuthTestStore(t)
	user := createSessionTestUser(t, store, rdb, 1)
	deviceID, err := NewDeviceID()
	if err != nil {
		t.Fatal(err)
	}
	sessionSecret, err := NewSecret("ds_", 32)
	if err != nil {
		t.Fatal(err)
	}
	sessionHash := secretHash(sessionSecret)
	now := time.Now().UTC().Truncate(time.Millisecond)

	if err := rdb.Set(context.Background(), deviceSessionKey(sessionHash), "blocked", 0).Err(); err != nil {
		t.Fatal(err)
	}
	cleanupAuthKeys(t, rdb,
		deviceKey(deviceID),
		userDevicesKey(user.ID),
		deviceSessionKey(sessionHash),
		userDeviceSessionsKey(user.ID),
	)

	err = store.CreateDeviceCredential(context.Background(), Device{
		ID:          deviceID,
		UserID:      user.ID,
		Name:        "blocked-create",
		SessionHash: sessionHash,
		CreatedAt:   now,
		LastSeenAt:  now,
	}, sessionHash, DeviceSession{
		UserID:          user.ID,
		DeviceID:        deviceID,
		PasswordVersion: user.PasswordVersion,
		CreatedAt:       now,
		LastSeenAt:      now,
	})
	if err == nil {
		t.Fatal("CreateDeviceCredential() error = nil; want failure")
	}

	exists, err := rdb.Exists(context.Background(), deviceKey(deviceID)).Result()
	if err != nil {
		t.Fatal(err)
	}
	if exists != 0 {
		t.Fatal("device hash created despite failure")
	}
	member, err := rdb.SIsMember(context.Background(), userDevicesKey(user.ID), deviceID).Result()
	if err != nil {
		t.Fatal(err)
	}
	if member {
		t.Fatal("user devices set updated despite failure")
	}
}

func TestDeviceCredentialResolveRejectsMissingOrMismatchedDevice(t *testing.T) {
	clock := FixedClock{T: time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)}
	svc, store, rdb := newAuthService(t, clock)
	username := uniqueTestUsername(t)
	password := "correct horse battery staple"
	user := registerServiceUser(t, svc, username, password)
	result := loginDeviceSession(t, svc, username, password, "203.0.113.5", "resolve-test")

	if err := store.DeleteDevice(context.Background(), result.DeviceID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.ResolveDeviceSession(context.Background(), result.DeviceCredential); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("missing device resolve error = %v; want ErrSessionExpired", err)
	}
	assertSessionAndIndexGone(t, rdb, deviceSessionKey(secretHash(result.DeviceCredential)), userDeviceSessionsKey(user.ID), secretHash(result.DeviceCredential))

	other := loginDeviceSession(t, svc, username, password, "203.0.113.6", "other-device")
	if err := rdb.HSet(context.Background(), deviceKey(other.DeviceID), "user_id", "usr_foreign000000000").Err(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.ResolveDeviceSession(context.Background(), other.DeviceCredential); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("foreign owner resolve error = %v; want ErrSessionExpired", err)
	}

	stale := loginDeviceSession(t, svc, username, password, "203.0.113.7", "stale-hash")
	if err := rdb.HSet(context.Background(), deviceKey(stale.DeviceID), "session_hash", "deadbeef").Err(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.ResolveDeviceSession(context.Background(), stale.DeviceCredential); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("stale session_hash resolve error = %v; want ErrSessionExpired", err)
	}
}

func TestDeviceCredentialNaturalExpiryKeepsDeviceRecord(t *testing.T) {
	store, rdb := newAuthTestStore(t)
	user := createSessionTestUser(t, store, rdb, 1)
	deviceID, err := NewDeviceID()
	if err != nil {
		t.Fatal(err)
	}
	sessionSecret, err := NewSecret("ds_", 32)
	if err != nil {
		t.Fatal(err)
	}
	sessionHash := secretHash(sessionSecret)
	now := time.Now().UTC().Truncate(time.Millisecond)
	cleanupAuthKeys(t, rdb, deviceKey(deviceID), deviceSessionKey(sessionHash), userDevicesKey(user.ID))

	if err := store.CreateDeviceCredential(context.Background(), Device{
		ID: deviceID, UserID: user.ID, Name: "ttl-device", SessionHash: sessionHash,
		CreatedAt: now, LastSeenAt: now,
	}, sessionHash, DeviceSession{
		UserID: user.ID, DeviceID: deviceID, PasswordVersion: user.PasswordVersion,
		CreatedAt: now, LastSeenAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := rdb.Expire(context.Background(), deviceSessionKey(sessionHash), time.Second).Err(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)

	if _, err := store.ResolveDeviceSession(context.Background(), sessionHash, time.Now()); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("expired resolve error = %v; want ErrSessionExpired", err)
	}
	exists, err := rdb.Exists(context.Background(), deviceKey(deviceID)).Result()
	if err != nil {
		t.Fatal(err)
	}
	if exists == 0 {
		t.Fatal("device record deleted after natural session expiry")
	}
	member, err := rdb.SIsMember(context.Background(), userDevicesKey(user.ID), deviceID).Result()
	if err != nil {
		t.Fatal(err)
	}
	if !member {
		t.Fatal("device removed from user devices set after natural session expiry")
	}
}

func TestDeviceCredentialLogoutClearsEverything(t *testing.T) {
	clock := FixedClock{T: time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)}
	svc, store, rdb := newAuthService(t, clock)
	username := uniqueTestUsername(t)
	password := "correct horse battery staple"
	user := registerServiceUser(t, svc, username, password)
	result := loginDeviceSession(t, svc, username, password, "203.0.113.8", "logout-device")

	if err := svc.LogoutDevice(context.Background(), result.DeviceCredential); err != nil {
		t.Fatal(err)
	}
	if err := svc.LogoutDevice(context.Background(), result.DeviceCredential); err != nil {
		t.Fatalf("second LogoutDevice() error = %v; want nil", err)
	}
	if _, err := store.Device(context.Background(), result.DeviceID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Device() after logout error = %v; want ErrNotFound", err)
	}
	exists, err := rdb.Exists(context.Background(), deviceSessionKey(secretHash(result.DeviceCredential))).Result()
	if err != nil {
		t.Fatal(err)
	}
	if exists != 0 {
		t.Fatal("device session remains after logout")
	}
	member, err := rdb.SIsMember(context.Background(), userDevicesKey(user.ID), result.DeviceID).Result()
	if err != nil {
		t.Fatal(err)
	}
	if member {
		t.Fatal("device remains in user devices set after logout")
	}
}

func TestChangePasswordRejectsWrongCurrentPassword(t *testing.T) {
	svc, _, _ := newAuthService(t, FixedClock{T: time.Now().UTC()})
	username := uniqueTestUsername(t)
	password := "correct horse battery staple"
	user := registerServiceUser(t, svc, username, password)
	err := svc.ChangePassword(context.Background(), user.ID, "wrong password value", "new horse battery staple")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("ChangePassword() error = %v; want ErrInvalidCredentials", err)
	}
}

func TestResetPasswordAtomicFailureKeepsOldPassword(t *testing.T) {
	svc, store, rdb := newAuthService(t, FixedClock{T: time.Now().UTC()})
	username := uniqueTestUsername(t)
	password := "correct horse battery staple"
	user := registerServiceUser(t, svc, username, password)
	before, err := store.UserByID(context.Background(), user.ID)
	if err != nil {
		t.Fatal(err)
	}

	if err := rdb.Del(context.Background(), userKey(user.ID)).Err(); err != nil {
		t.Fatal(err)
	}
	temp, err := svc.ResetPassword(context.Background(), username)
	if err == nil {
		t.Fatalf("ResetPassword() = %q with missing user; want error", temp)
	}
	if temp != "" {
		t.Fatalf("ResetPassword() returned temp password %q on failure", temp)
	}

	restored, err := store.UserByID(context.Background(), user.ID)
	if err == nil && restored.PasswordPHC != before.PasswordPHC {
		t.Fatal("reset failure changed stored password")
	}
}

func secretHash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}
