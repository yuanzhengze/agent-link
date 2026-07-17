package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/team/agentlink/pkg/redis"
)

func TestPasswordUpdateConcurrentIncrementsVersion(t *testing.T) {
	store, rdb := newAuthTestStore(t)
	user := createSessionTestUser(t, store, rdb, 11)

	start := make(chan struct{})
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			<-start
			errs <- updatePasswordGate(
				context.Background(),
				store,
				user.ID,
				"concurrent-phc-"+string(rune('a'+i)),
				user.PasswordVersion+1,
				i == 1,
			)
		}(i)
	}
	close(start)

	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent password update %d error = %v", i+1, err)
		}
	}

	stored, err := store.UserByID(context.Background(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.PasswordVersion != user.PasswordVersion+2 {
		t.Fatalf(
			"password version after two concurrent updates = %d; want %d",
			stored.PasswordVersion,
			user.PasswordVersion+2,
		)
	}
}

func TestPasswordUpdateInvalidatesIntermediateVersionSession(t *testing.T) {
	store, rdb := newAuthTestStore(t)
	user := createSessionTestUser(t, store, rdb, 23)

	if err := updatePasswordGate(
		context.Background(),
		store,
		user.ID,
		"first-phc",
		user.PasswordVersion+1,
		false,
	); err != nil {
		t.Fatal(err)
	}

	intermediate, err := store.UserByID(context.Background(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	sessionHash := uniqueSessionHash(t, "intermediate_")
	now := time.Now().UTC().Truncate(time.Millisecond)
	cleanupAuthKeys(t, rdb, webSessionKey(sessionHash))
	if err := store.CreateWebSession(context.Background(), sessionHash, WebSession{
		UserID:            user.ID,
		CSRFHash:          "csrf-hash",
		PasswordVersion:   intermediate.PasswordVersion,
		CreatedAt:         now,
		LastSeenAt:        now,
		AbsoluteExpiresAt: now.Add(WebSessionAbsoluteTTL),
	}); err != nil {
		t.Fatal(err)
	}

	// Simulate a second updater that read the same stale version as the first.
	if err := updatePasswordGate(
		context.Background(),
		store,
		user.ID,
		"second-phc",
		user.PasswordVersion+1,
		true,
	); err != nil {
		t.Fatal(err)
	}

	stored, err := store.UserByID(context.Background(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.PasswordVersion != user.PasswordVersion+2 {
		t.Fatalf(
			"password version after two updates = %d; want %d",
			stored.PasswordVersion,
			user.PasswordVersion+2,
		)
	}
	if _, err := store.ResolveWebSession(context.Background(), sessionHash, time.Now()); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("intermediate-version session resolve error = %v; want ErrSessionExpired", err)
	}
}

func TestPasswordUpdateDeletesCredentialBindings(t *testing.T) {
	store, rdb := newAuthTestStore(t)
	user := createSessionTestUser(t, store, rdb, 7)
	fixture := newGateDeviceCredential(t, store, rdb, user)

	if err := updatePasswordGate(
		context.Background(),
		store,
		user.ID,
		"replacement-phc",
		user.PasswordVersion+1,
		false,
	); err != nil {
		t.Fatal(err)
	}

	assertGateKeyMissing(t, rdb, gateDeviceCredentialKey(fixture.sessionHash))
	assertGateKeyMissing(t, rdb, deviceKey(fixture.device.ID))
	assertGateKeyMissing(t, rdb, deviceSessionKey(fixture.sessionHash))
}

func TestServiceInitializesFixedDummyPHC(t *testing.T) {
	store, _ := newAuthTestStore(t)
	first := NewService(store, FixedClock{T: time.Now().UTC()})
	second := NewService(store, FixedClock{T: time.Now().UTC()})

	if first.dummyPHC == "" {
		t.Fatal("NewService() left dummyPHC empty; first unknown login would hash a random dummy")
	}
	if first.dummyPHC != second.dummyPHC {
		t.Fatal("NewService() dummy PHC is not deterministic")
	}
	ok, err := VerifyPassword(first.dummyPHC, dummyLoginPassword)
	if err != nil || !ok {
		t.Fatalf("fixed dummy PHC verification = %v, %v; want true, nil", ok, err)
	}
}

func TestLoginFailureUnknownUsersShareIPLimit(t *testing.T) {
	svc, _, rdb := newAuthService(t, FixedClock{T: time.Now().UTC()})
	ip := "unknown-" + uniqueTestUsername(t)
	clearAuthKeys(t, rdb, loginFailIPKey(ip))

	for i := 0; i < loginFailureMaxFailures; i++ {
		username := uniqueTestUsername(t)
		_, err := svc.Login(context.Background(), LoginInput{
			Username: username,
			Password: "wrong password value",
			IP:       ip,
		})
		if i < loginFailureMaxFailures-1 && !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("unknown login %d error = %v; want ErrInvalidCredentials", i+1, err)
		}
		if i == loginFailureMaxFailures-1 && !errors.Is(err, ErrRateLimited) {
			t.Fatalf("unknown login %d error = %v; want ErrRateLimited", i+1, err)
		}
	}
}

func TestLoginFailureInactiveAlwaysCounts(t *testing.T) {
	for _, test := range []struct {
		name     string
		password string
	}{
		{name: "wrong password", password: "wrong password value"},
		{name: "correct password", password: "correct horse battery staple"},
	} {
		t.Run(test.name, func(t *testing.T) {
			svc, store, rdb := newAuthService(t, FixedClock{T: time.Now().UTC()})
			username := uniqueTestUsername(t)
			user := registerServiceUser(t, svc, username, "correct horse battery staple")
			if err := store.SetUserStatus(context.Background(), user.ID, "inactive"); err != nil {
				t.Fatal(err)
			}
			ip := test.name + "-" + uniqueTestUsername(t)
			normalized, err := NormalizeUsername(username)
			if err != nil {
				t.Fatal(err)
			}
			clearAuthKeys(t, rdb, loginFailUserKey(normalized), loginFailIPKey(ip))

			_, err = svc.Login(context.Background(), LoginInput{
				Username: username,
				Password: test.password,
				IP:       ip,
			})
			if !errors.Is(err, ErrInvalidCredentials) {
				t.Fatalf("inactive Login() error = %v; want ErrInvalidCredentials", err)
			}
			userCount, ipCount, err := store.GetLoginFailureCounts(context.Background(), normalized, ip)
			if err != nil {
				t.Fatal(err)
			}
			if userCount != 1 || ipCount != 1 {
				t.Fatalf("inactive failure counters = user %d, IP %d; want 1, 1", userCount, ipCount)
			}
		})
	}
}

func TestLoginInfrastructureErrorDoesNotCount(t *testing.T) {
	svc, store, rdb := newAuthService(t, FixedClock{T: time.Now().UTC()})
	username := uniqueTestUsername(t)
	user := registerServiceUser(t, svc, username, "correct horse battery staple")
	ip := "infra-" + uniqueTestUsername(t)
	normalized, err := NormalizeUsername(username)
	if err != nil {
		t.Fatal(err)
	}
	clearAuthKeys(t, rdb, loginFailUserKey(normalized), loginFailIPKey(ip))
	if err := rdb.HSet(context.Background(), userKey(user.ID), "password_phc", "not-a-valid-phc").Err(); err != nil {
		t.Fatal(err)
	}

	_, err = svc.Login(context.Background(), LoginInput{
		Username: username,
		Password: "wrong password value",
		IP:       ip,
	})
	if err == nil || errors.Is(err, ErrInvalidCredentials) || errors.Is(err, ErrRateLimited) {
		t.Fatalf("Login() infrastructure error = %v; want non-authentication error", err)
	}
	userCount, ipCount, err := store.GetLoginFailureCounts(context.Background(), normalized, ip)
	if err != nil {
		t.Fatal(err)
	}
	if userCount != 0 || ipCount != 0 {
		t.Fatalf("infrastructure failure counters = user %d, IP %d; want 0, 0", userCount, ipCount)
	}
}

func TestLoginFailureLuaRejectsNonNumericWithoutPartialIncrement(t *testing.T) {
	store, rdb := newAuthTestStore(t)
	normalized := uniqueTestUsername(t)
	ip := "nonnumeric-" + uniqueTestUsername(t)
	userCounter := loginFailUserKey(normalized)
	ipCounter := loginFailIPKey(ip)
	clearAuthKeys(t, rdb, userCounter, ipCounter)
	cleanupAuthKeys(t, rdb, userCounter, ipCounter)
	if err := rdb.Set(context.Background(), userCounter, "4", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.Set(context.Background(), ipCounter, "not-a-number", 0).Err(); err != nil {
		t.Fatal(err)
	}

	if _, _, err := store.RecordLoginFailure(context.Background(), normalized, ip); err == nil {
		t.Fatal("RecordLoginFailure() error = nil; want nonnumeric-counter error")
	}
	userValue, err := rdb.Get(context.Background(), userCounter).Result()
	if err != nil {
		t.Fatal(err)
	}
	ipValue, err := rdb.Get(context.Background(), ipCounter).Result()
	if err != nil {
		t.Fatal(err)
	}
	if userValue != "4" || ipValue != "not-a-number" {
		t.Fatalf(
			"counter values after rejected increment = %q, %q; want %q, %q",
			userValue,
			ipValue,
			"4",
			"not-a-number",
		)
	}
}

func TestDeviceCredentialBindingPersistsWithoutTTL(t *testing.T) {
	store, rdb := newAuthTestStore(t)
	user := createSessionTestUser(t, store, rdb, 1)
	fixture := newGateDeviceCredential(t, store, rdb, user)
	bindingKey := gateDeviceCredentialKey(fixture.sessionHash)

	fields, err := rdb.HGetAll(context.Background(), bindingKey).Result()
	if err != nil {
		t.Fatal(err)
	}
	if fields["user_id"] != user.ID || fields["device_id"] != fixture.device.ID {
		t.Fatalf("credential binding = %#v; want user %q device %q", fields, user.ID, fixture.device.ID)
	}
	ttl, err := rdb.PTTL(context.Background(), bindingKey).Result()
	if err != nil {
		t.Fatal(err)
	}
	if ttl != -1 {
		t.Fatalf("credential binding PTTL = %s; want persistent (-1)", ttl)
	}
}

func TestDeviceCredentialLogoutAfterSessionExpiryDeletesInventory(t *testing.T) {
	store, rdb := newAuthTestStore(t)
	user := createSessionTestUser(t, store, rdb, 2)
	fixture := newGateDeviceCredential(t, store, rdb, user)

	// Redis naturally expires only the credential session. The persistent
	// binding and device inventory must still let logout find the device.
	if err := rdb.Del(context.Background(), deviceSessionKey(fixture.sessionHash)).Err(); err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeDeviceCredential(context.Background(), fixture.sessionHash); err != nil {
		t.Fatal(err)
	}

	assertGateCredentialGone(t, rdb, fixture)
}

func TestDeviceCredentialDeleteRevokesCredential(t *testing.T) {
	store, rdb := newAuthTestStore(t)
	user := createSessionTestUser(t, store, rdb, 3)
	fixture := newGateDeviceCredential(t, store, rdb, user)

	if err := store.DeleteDevice(context.Background(), fixture.device.ID); err != nil {
		t.Fatal(err)
	}
	assertGateCredentialGone(t, rdb, fixture)
}

func TestDeviceCredentialBindingMismatchRejectsWithoutDeletingForeignDevice(t *testing.T) {
	store, rdb := newAuthTestStore(t)
	firstUser := createSessionTestUser(t, store, rdb, 1)
	secondUser := createSessionTestUser(t, store, rdb, 1)
	first := newGateDeviceCredential(t, store, rdb, firstUser)
	foreign := newGateDeviceCredential(t, store, rdb, secondUser)

	if err := rdb.HSet(
		context.Background(),
		gateDeviceCredentialKey(first.sessionHash),
		"user_id", secondUser.ID,
		"device_id", foreign.device.ID,
	).Err(); err != nil {
		t.Fatal(err)
	}

	if _, err := store.ResolveDeviceSession(context.Background(), first.sessionHash, time.Now()); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("ResolveDeviceSession() binding mismatch error = %v; want ErrSessionExpired", err)
	}
	assertGateKeyMissing(t, rdb, deviceSessionKey(first.sessionHash))
	assertZMemberGone(t, rdb, userDeviceSessionsKey(firstUser.ID), first.sessionHash)
	assertGateKeyExists(t, rdb, deviceKey(first.device.ID))
	assertGateKeyExists(t, rdb, deviceKey(foreign.device.ID))
}

func TestDeviceCredentialCreateWrongTypeIsAtomic(t *testing.T) {
	store, rdb := newAuthTestStore(t)
	user := createSessionTestUser(t, store, rdb, 1)
	fixture := newUnstoredGateDevice(t, user)
	bindingKey := gateDeviceCredentialKey(fixture.sessionHash)
	cleanupGateDeviceKeys(t, rdb, fixture)
	if err := rdb.Set(context.Background(), bindingKey, "blocked-binding", 0).Err(); err != nil {
		t.Fatal(err)
	}

	err := store.CreateDeviceCredential(
		context.Background(),
		fixture.device,
		fixture.sessionHash,
		fixture.session,
	)
	if err == nil {
		t.Fatal("CreateDeviceCredential() error = nil; want wrong-type rejection")
	}
	value, err := rdb.Get(context.Background(), bindingKey).Result()
	if err != nil {
		t.Fatal(err)
	}
	if value != "blocked-binding" {
		t.Fatalf("binding value = %q; want unchanged", value)
	}
	assertGateKeyMissing(t, rdb, deviceKey(fixture.device.ID))
	assertGateKeyMissing(t, rdb, deviceSessionKey(fixture.sessionHash))
	assertGateKeyMissing(t, rdb, userDevicesKey(user.ID))
	assertGateKeyMissing(t, rdb, userDeviceSessionsKey(user.ID))
}

func TestDeviceCredentialLogoutWrongTypeIsAtomic(t *testing.T) {
	store, rdb := newAuthTestStore(t)
	user := createSessionTestUser(t, store, rdb, 1)
	fixture := newGateDeviceCredential(t, store, rdb, user)
	ensureGateBinding(t, rdb, fixture)

	if err := rdb.Del(context.Background(), userDevicesKey(user.ID)).Err(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.Set(context.Background(), userDevicesKey(user.ID), "blocked-set", 0).Err(); err != nil {
		t.Fatal(err)
	}

	if err := store.RevokeDeviceCredential(context.Background(), fixture.sessionHash); err == nil {
		t.Fatal("RevokeDeviceCredential() error = nil; want wrong-type rejection")
	}
	assertGateKeyExists(t, rdb, gateDeviceCredentialKey(fixture.sessionHash))
	assertGateKeyExists(t, rdb, deviceKey(fixture.device.ID))
	assertGateKeyExists(t, rdb, deviceSessionKey(fixture.sessionHash))
	if _, err := rdb.ZScore(
		context.Background(),
		userDeviceSessionsKey(user.ID),
		fixture.sessionHash,
	).Result(); err != nil {
		t.Fatalf("device session index was partially changed: %v", err)
	}
	value, err := rdb.Get(context.Background(), userDevicesKey(user.ID)).Result()
	if err != nil {
		t.Fatal(err)
	}
	if value != "blocked-set" {
		t.Fatalf("user devices value = %q; want unchanged", value)
	}
}

func TestDeviceCredentialDeleteWrongTypeIsAtomic(t *testing.T) {
	store, rdb := newAuthTestStore(t)
	user := createSessionTestUser(t, store, rdb, 1)
	fixture := newGateDeviceCredential(t, store, rdb, user)
	ensureGateBinding(t, rdb, fixture)

	if err := rdb.Del(context.Background(), userDevicesKey(user.ID)).Err(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.Set(context.Background(), userDevicesKey(user.ID), "blocked-set", 0).Err(); err != nil {
		t.Fatal(err)
	}

	if err := store.DeleteDevice(context.Background(), fixture.device.ID); err == nil {
		t.Fatal("DeleteDevice() error = nil; want wrong-type rejection")
	}
	assertGateKeyExists(t, rdb, gateDeviceCredentialKey(fixture.sessionHash))
	assertGateKeyExists(t, rdb, deviceKey(fixture.device.ID))
	assertGateKeyExists(t, rdb, deviceSessionKey(fixture.sessionHash))
	if _, err := rdb.ZScore(
		context.Background(),
		userDeviceSessionsKey(user.ID),
		fixture.sessionHash,
	).Result(); err != nil {
		t.Fatalf("device session index was partially changed: %v", err)
	}
}

func TestDeviceCredentialRejectsMismatchedParameters(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*gateDeviceFixture)
	}{
		{
			name: "user ID",
			mutate: func(f *gateDeviceFixture) {
				f.session.UserID = "usr_mismatched_user"
			},
		},
		{
			name: "device ID",
			mutate: func(f *gateDeviceFixture) {
				f.session.DeviceID = "dev_mismatched_device"
			},
		},
		{
			name: "session hash",
			mutate: func(f *gateDeviceFixture) {
				f.device.SessionHash = "mismatched-session-hash"
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, rdb := newAuthTestStore(t)
			user := createSessionTestUser(t, store, rdb, 1)
			fixture := newUnstoredGateDevice(t, user)
			test.mutate(&fixture)
			cleanupGateDeviceKeys(t, rdb, fixture)

			err := store.CreateDeviceCredential(
				context.Background(),
				fixture.device,
				fixture.sessionHash,
				fixture.session,
			)
			if err == nil {
				t.Fatal("CreateDeviceCredential() error = nil; want argument validation error")
			}
			assertGateKeyMissing(t, rdb, gateDeviceCredentialKey(fixture.sessionHash))
			assertGateKeyMissing(t, rdb, deviceKey(fixture.device.ID))
			assertGateKeyMissing(t, rdb, deviceSessionKey(fixture.sessionHash))
			assertGateKeyMissing(t, rdb, userDevicesKey(fixture.device.UserID))
			assertGateKeyMissing(t, rdb, userDeviceSessionsKey(fixture.device.UserID))
		})
	}
}

func TestResetPasswordWrongTypeIsAtomic(t *testing.T) {
	svc, store, rdb := newAuthService(t, FixedClock{T: time.Now().UTC()})
	username := uniqueTestUsername(t)
	password := "correct horse battery staple"
	user := registerServiceUser(t, svc, username, password)
	before, err := store.UserByID(context.Background(), user.ID)
	if err != nil {
		t.Fatal(err)
	}

	indexKey := userWebSessionsKey(user.ID)
	if err := rdb.Del(context.Background(), indexKey).Err(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.Set(context.Background(), indexKey, "blocked-index", 0).Err(); err != nil {
		t.Fatal(err)
	}

	temp, err := svc.ResetPassword(context.Background(), username)
	if err == nil {
		t.Fatalf("ResetPassword() = %q, nil; want atomic wrong-type error", temp)
	}
	if strings.Contains(err.Error(), "unexpected type") {
		t.Fatalf("ResetPassword() exposed Lua/Go return-shape mismatch: %v", err)
	}
	if temp != "" {
		t.Fatalf("ResetPassword() returned temp password %q on failure", temp)
	}

	after, err := store.UserByID(context.Background(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.PasswordPHC != before.PasswordPHC ||
		after.PasswordVersion != before.PasswordVersion ||
		after.MustChangePassword != before.MustChangePassword {
		t.Fatalf("user changed after failed reset: before %+v after %+v", before, after)
	}
	ok, err := VerifyPassword(after.PasswordPHC, password)
	if err != nil || !ok {
		t.Fatalf("old password after failed reset = %v, %v; want true, nil", ok, err)
	}
	value, err := rdb.Get(context.Background(), indexKey).Result()
	if err != nil {
		t.Fatal(err)
	}
	if value != "blocked-index" {
		t.Fatalf("web session index value = %q; want unchanged", value)
	}
}

// updatePasswordGate deliberately accepts a stale absolute version while
// recording RED against the old API. The production fix removes that argument
// and this adapter becomes a direct call to the increment-only API.
func updatePasswordGate(
	ctx context.Context,
	store *Store,
	userID, passwordPHC string,
	_ int64,
	mustChange bool,
) error {
	return store.UpdateUserPasswordAndRevokeSessions(
		ctx,
		userID,
		passwordPHC,
		mustChange,
	)
}

type gateDeviceFixture struct {
	user        User
	device      Device
	sessionHash string
	session     DeviceSession
}

func newUnstoredGateDevice(t *testing.T, user User) gateDeviceFixture {
	t.Helper()
	deviceID, err := NewDeviceID()
	if err != nil {
		t.Fatal(err)
	}
	credential, err := NewSecret("ds_", 32)
	if err != nil {
		t.Fatal(err)
	}
	sessionHash := SecretHash(credential)
	now := time.Now().UTC().Truncate(time.Millisecond)
	return gateDeviceFixture{
		user: user,
		device: Device{
			ID:          deviceID,
			UserID:      user.ID,
			Name:        "gate-device",
			SessionHash: sessionHash,
			CreatedAt:   now,
			LastSeenAt:  now,
		},
		sessionHash: sessionHash,
		session: DeviceSession{
			UserID:          user.ID,
			DeviceID:        deviceID,
			PasswordVersion: user.PasswordVersion,
			CreatedAt:       now,
			LastSeenAt:      now,
		},
	}
}

func newGateDeviceCredential(
	t *testing.T,
	store *Store,
	rdb *redis.Client,
	user User,
) gateDeviceFixture {
	t.Helper()
	fixture := newUnstoredGateDevice(t, user)
	cleanupGateDeviceKeys(t, rdb, fixture)
	if err := store.CreateDeviceCredential(
		context.Background(),
		fixture.device,
		fixture.sessionHash,
		fixture.session,
	); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func cleanupGateDeviceKeys(t *testing.T, rdb *redis.Client, fixture gateDeviceFixture) {
	t.Helper()
	cleanupAuthKeys(
		t,
		rdb,
		gateDeviceCredentialKey(fixture.sessionHash),
		deviceKey(fixture.device.ID),
		deviceSessionKey(fixture.sessionHash),
		userDevicesKey(fixture.device.UserID),
		userDeviceSessionsKey(fixture.device.UserID),
	)
}

func ensureGateBinding(t *testing.T, rdb *redis.Client, fixture gateDeviceFixture) {
	t.Helper()
	key := gateDeviceCredentialKey(fixture.sessionHash)
	keyType, err := rdb.Type(context.Background(), key).Result()
	if err != nil {
		t.Fatal(err)
	}
	if keyType == "none" {
		if err := rdb.HSet(
			context.Background(),
			key,
			"user_id", fixture.user.ID,
			"device_id", fixture.device.ID,
		).Err(); err != nil {
			t.Fatal(err)
		}
	}
}

func assertGateCredentialGone(t *testing.T, rdb *redis.Client, fixture gateDeviceFixture) {
	t.Helper()
	assertGateKeyMissing(t, rdb, gateDeviceCredentialKey(fixture.sessionHash))
	assertGateKeyMissing(t, rdb, deviceKey(fixture.device.ID))
	assertGateKeyMissing(t, rdb, deviceSessionKey(fixture.sessionHash))
	assertZMemberGone(t, rdb, userDeviceSessionsKey(fixture.user.ID), fixture.sessionHash)
	member, err := rdb.SIsMember(
		context.Background(),
		userDevicesKey(fixture.user.ID),
		fixture.device.ID,
	).Result()
	if err != nil {
		t.Fatal(err)
	}
	if member {
		t.Fatal("device remains in user devices set")
	}
}

func assertGateKeyExists(t *testing.T, rdb *redis.Client, key string) {
	t.Helper()
	exists, err := rdb.Exists(context.Background(), key).Result()
	if err != nil {
		t.Fatal(err)
	}
	if exists != 1 {
		t.Fatalf("Redis key %q does not exist", key)
	}
}

func assertGateKeyMissing(t *testing.T, rdb *redis.Client, key string) {
	t.Helper()
	exists, err := rdb.Exists(context.Background(), key).Result()
	if err != nil {
		t.Fatal(err)
	}
	if exists != 0 {
		t.Fatalf("Redis key %q still exists", key)
	}
}

func gateDeviceCredentialKey(sessionHash string) string {
	return "agentlink:v2:device_credential:" + sessionHash
}
