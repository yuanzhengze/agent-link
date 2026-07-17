package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/team/agentlink/pkg/redis"
)

func TestWebSessionResolveAndRevoke(t *testing.T) {
	store, rdb := newAuthTestStore(t)
	user := createSessionTestUser(t, store, rdb, 4)
	hash := uniqueSessionHash(t, "web_")
	cleanupAuthKeys(t, rdb, webSessionKey(hash))

	createdAt := time.Now().UTC().Truncate(time.Millisecond)
	session := WebSession{
		UserID:            user.ID,
		CSRFHash:          "csrf-hash",
		PasswordVersion:   user.PasswordVersion,
		CreatedAt:         createdAt,
		LastSeenAt:        createdAt,
		AbsoluteExpiresAt: createdAt.Add(WebSessionAbsoluteTTL),
	}
	beforeCreate := time.Now()
	if err := store.CreateWebSession(context.Background(), hash, session); err != nil {
		t.Fatal(err)
	}
	afterCreate := time.Now()
	assertTTLNear(t, rdb, webSessionKey(hash), WebSessionIdleTTL)
	assertZScoreBetween(
		t,
		rdb,
		userWebSessionsKey(user.ID),
		hash,
		beforeCreate.Add(WebSessionIdleTTL),
		afterCreate.Add(WebSessionIdleTTL),
	)

	resolveNow := time.Now().UTC().Truncate(time.Millisecond)
	beforeResolve := time.Now()
	resolved, err := store.ResolveWebSession(context.Background(), hash, resolveNow)
	afterResolve := time.Now()
	if err != nil {
		t.Fatal(err)
	}
	if resolved.UserID != session.UserID ||
		resolved.CSRFHash != session.CSRFHash ||
		resolved.PasswordVersion != session.PasswordVersion ||
		!resolved.CreatedAt.Equal(session.CreatedAt) ||
		!resolved.LastSeenAt.Equal(resolveNow) ||
		!resolved.AbsoluteExpiresAt.Equal(session.AbsoluteExpiresAt) {
		t.Fatalf("ResolveWebSession() = %+v; want session with last seen %s", resolved, resolveNow)
	}
	assertTTLNear(t, rdb, webSessionKey(hash), WebSessionIdleTTL)
	assertZScoreBetween(
		t,
		rdb,
		userWebSessionsKey(user.ID),
		hash,
		beforeResolve.Add(WebSessionIdleTTL),
		afterResolve.Add(WebSessionIdleTTL),
	)

	if err := store.RevokeWebSession(context.Background(), hash); err != nil {
		t.Fatal(err)
	}
	assertSessionAndIndexGone(t, rdb, webSessionKey(hash), userWebSessionsKey(user.ID), hash)
	if err := store.RevokeWebSession(context.Background(), hash); err != nil {
		t.Fatalf("second RevokeWebSession() error = %v; want nil", err)
	}
	if _, err := store.ResolveWebSession(context.Background(), hash, time.Now()); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("ResolveWebSession() after revoke error = %v; want ErrSessionExpired", err)
	}
}

func TestWebSessionResolveUsesRedisTimeForExpiry(t *testing.T) {
	store, rdb := newAuthTestStore(t)
	user := createSessionTestUser(t, store, rdb, 1)
	hash := uniqueSessionHash(t, "web_")
	cleanupAuthKeys(t, rdb, webSessionKey(hash))

	createdAt := time.Now().UTC().Truncate(time.Millisecond)
	if err := store.CreateWebSession(context.Background(), hash, WebSession{
		UserID:            user.ID,
		CSRFHash:          "csrf-hash",
		PasswordVersion:   user.PasswordVersion,
		CreatedAt:         createdAt,
		LastSeenAt:        createdAt,
		AbsoluteExpiresAt: createdAt.Add(WebSessionAbsoluteTTL),
	}); err != nil {
		t.Fatal(err)
	}

	futureLastSeen := time.Now().UTC().Add(time.Hour).Truncate(time.Millisecond)
	resolved, err := store.ResolveWebSession(context.Background(), hash, futureLastSeen)
	if err != nil {
		t.Fatal(err)
	}
	if !resolved.LastSeenAt.Equal(futureLastSeen) {
		t.Fatalf("LastSeenAt = %s; want caller-provided %s", resolved.LastSeenAt, futureLastSeen)
	}
	ttl, err := rdb.PTTL(context.Background(), webSessionKey(hash)).Result()
	if err != nil {
		t.Fatal(err)
	}
	if ttl < WebSessionIdleTTL-5*time.Second || ttl > WebSessionIdleTTL {
		t.Fatalf("web session TTL after +1h LastSeenAt = %s; want Redis-based TTL within 5s of %s", ttl, WebSessionIdleTTL)
	}
}

func TestSessionIndexScoresUseUnixMilliseconds(t *testing.T) {
	t.Run("web create and resolve", func(t *testing.T) {
		store, rdb := newAuthTestStore(t)
		user := createSessionTestUser(t, store, rdb, 1)
		hash := uniqueSessionHash(t, "web_")
		cleanupAuthKeys(t, rdb, webSessionKey(hash))

		createdAt := time.Now().UTC().Truncate(time.Millisecond)
		if err := store.CreateWebSession(context.Background(), hash, WebSession{
			UserID:            user.ID,
			CSRFHash:          "csrf-hash",
			PasswordVersion:   user.PasswordVersion,
			CreatedAt:         createdAt,
			LastSeenAt:        createdAt,
			AbsoluteExpiresAt: createdAt.Add(WebSessionAbsoluteTTL),
		}); err != nil {
			t.Fatal(err)
		}
		assertSessionIndexScoreMatchesExpiry(
			t,
			rdb,
			webSessionKey(hash),
			userWebSessionsKey(user.ID),
			hash,
		)

		if _, err := store.ResolveWebSession(context.Background(), hash, time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		assertSessionIndexScoreMatchesExpiry(
			t,
			rdb,
			webSessionKey(hash),
			userWebSessionsKey(user.ID),
			hash,
		)
	})

	t.Run("device create and resolve", func(t *testing.T) {
		store, rdb := newAuthTestStore(t)
		user := createSessionTestUser(t, store, rdb, 1)
		hash := uniqueSessionHash(t, "device_")
		cleanupAuthKeys(t, rdb, deviceSessionKey(hash))

		createdAt := time.Now().UTC().Truncate(time.Millisecond)
		createDeviceSessionForTest(t, store, rdb, user, hash, "device-id", createdAt)
		assertSessionIndexScoreMatchesExpiry(
			t,
			rdb,
			deviceSessionKey(hash),
			userDeviceSessionsKey(user.ID),
			hash,
		)

		if _, err := store.ResolveDeviceSession(context.Background(), hash, time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		assertSessionIndexScoreMatchesExpiry(
			t,
			rdb,
			deviceSessionKey(hash),
			userDeviceSessionsKey(user.ID),
			hash,
		)
	})
}

func TestDeviceSessionResolveAndRevoke(t *testing.T) {
	store, rdb := newAuthTestStore(t)
	user := createSessionTestUser(t, store, rdb, 2)
	hash := uniqueSessionHash(t, "device_")
	cleanupAuthKeys(t, rdb, deviceSessionKey(hash))

	createdAt := time.Now().UTC().Truncate(time.Millisecond)
	session := DeviceSession{
		UserID:          user.ID,
		DeviceID:        "dev-" + uniqueSessionHash(t, ""),
		PasswordVersion: user.PasswordVersion,
		CreatedAt:       createdAt,
		LastSeenAt:      createdAt,
	}
	beforeCreate := time.Now()
	createDeviceSessionForTest(t, store, rdb, user, hash, session.DeviceID, createdAt)
	afterCreate := time.Now()
	assertTTLNear(t, rdb, deviceSessionKey(hash), DeviceSessionIdleTTL)
	assertZScoreBetween(
		t,
		rdb,
		userDeviceSessionsKey(user.ID),
		hash,
		beforeCreate.Add(DeviceSessionIdleTTL),
		afterCreate.Add(DeviceSessionIdleTTL),
	)

	resolveNow := time.Now().UTC().Truncate(time.Millisecond)
	beforeResolve := time.Now()
	resolved, err := store.ResolveDeviceSession(context.Background(), hash, resolveNow)
	afterResolve := time.Now()
	if err != nil {
		t.Fatal(err)
	}
	if resolved.UserID != session.UserID ||
		resolved.DeviceID != session.DeviceID ||
		resolved.PasswordVersion != session.PasswordVersion ||
		!resolved.CreatedAt.Equal(session.CreatedAt) ||
		!resolved.LastSeenAt.Equal(resolveNow) {
		t.Fatalf("ResolveDeviceSession() = %+v; want session with last seen %s", resolved, resolveNow)
	}
	assertTTLNear(t, rdb, deviceSessionKey(hash), DeviceSessionIdleTTL)
	assertZScoreBetween(
		t,
		rdb,
		userDeviceSessionsKey(user.ID),
		hash,
		beforeResolve.Add(DeviceSessionIdleTTL),
		afterResolve.Add(DeviceSessionIdleTTL),
	)

	if err := store.RevokeDeviceSession(context.Background(), hash); err != nil {
		t.Fatal(err)
	}
	assertSessionAndIndexGone(t, rdb, deviceSessionKey(hash), userDeviceSessionsKey(user.ID), hash)
	if err := store.RevokeDeviceSession(context.Background(), hash); err != nil {
		t.Fatalf("second RevokeDeviceSession() error = %v; want nil", err)
	}
	if _, err := store.ResolveDeviceSession(context.Background(), hash, time.Now()); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("ResolveDeviceSession() after revoke error = %v; want ErrSessionExpired", err)
	}
}

func TestPasswordVersionInvalidatesSessions(t *testing.T) {
	store, rdb := newAuthTestStore(t)
	user := createSessionTestUser(t, store, rdb, 7)
	webHash := uniqueSessionHash(t, "web_")
	deviceHash := uniqueSessionHash(t, "device_")
	cleanupAuthKeys(t, rdb, webSessionKey(webHash), deviceSessionKey(deviceHash))

	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := store.CreateWebSession(context.Background(), webHash, WebSession{
		UserID:            user.ID,
		CSRFHash:          "csrf-hash",
		PasswordVersion:   user.PasswordVersion,
		CreatedAt:         now,
		LastSeenAt:        now,
		AbsoluteExpiresAt: now.Add(WebSessionAbsoluteTTL),
	}); err != nil {
		t.Fatal(err)
	}
	createDeviceSessionForTest(t, store, rdb, user, deviceHash, "device-id", now)
	if err := rdb.HSet(context.Background(), userKey(user.ID), "password_version", user.PasswordVersion+1).Err(); err != nil {
		t.Fatal(err)
	}

	if _, err := store.ResolveWebSession(context.Background(), webHash, time.Now()); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("ResolveWebSession() error = %v; want ErrSessionExpired", err)
	}
	assertSessionAndIndexGone(t, rdb, webSessionKey(webHash), userWebSessionsKey(user.ID), webHash)
	if _, err := store.ResolveDeviceSession(context.Background(), deviceHash, time.Now()); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("ResolveDeviceSession() error = %v; want ErrSessionExpired", err)
	}
	assertSessionAndIndexGone(t, rdb, deviceSessionKey(deviceHash), userDeviceSessionsKey(user.ID), deviceHash)
}

func TestWebSessionHonorsAbsoluteExpiry(t *testing.T) {
	store, rdb := newAuthTestStore(t)
	user := createSessionTestUser(t, store, rdb, 1)
	hash := uniqueSessionHash(t, "web_")
	cleanupAuthKeys(t, rdb, webSessionKey(hash))

	createdAt := time.Now().UTC().Truncate(time.Millisecond)
	absoluteExpiry := createdAt.Add(2 * time.Hour)
	if err := store.CreateWebSession(context.Background(), hash, WebSession{
		UserID:            user.ID,
		CSRFHash:          "csrf-hash",
		PasswordVersion:   user.PasswordVersion,
		CreatedAt:         createdAt,
		LastSeenAt:        createdAt,
		AbsoluteExpiresAt: absoluteExpiry,
	}); err != nil {
		t.Fatal(err)
	}
	ttl, err := rdb.PTTL(context.Background(), webSessionKey(hash)).Result()
	if err != nil {
		t.Fatal(err)
	}
	if ttl <= 119*time.Minute || ttl > 2*time.Hour {
		t.Fatalf("web session TTL = %s; want approximately 2h absolute remainder", ttl)
	}
	score, err := rdb.ZScore(context.Background(), userWebSessionsKey(user.ID), hash).Result()
	if err != nil {
		t.Fatal(err)
	}
	if int64(score) != absoluteExpiry.UnixMilli() {
		t.Fatalf("web session index score = %d; want absolute expiry %d", int64(score), absoluteExpiry.UnixMilli())
	}

	resolved, err := store.ResolveWebSession(context.Background(), hash, absoluteExpiry)
	if err != nil {
		t.Fatalf("ResolveWebSession() with future LastSeenAt error = %v; want nil", err)
	}
	if !resolved.LastSeenAt.Equal(absoluteExpiry) {
		t.Fatalf("LastSeenAt = %s; want caller-provided %s", resolved.LastSeenAt, absoluteExpiry)
	}
	ttl, err = rdb.PTTL(context.Background(), webSessionKey(hash)).Result()
	if err != nil {
		t.Fatal(err)
	}
	if ttl <= 119*time.Minute || ttl > 2*time.Hour {
		t.Fatalf("web session TTL after future LastSeenAt = %s; want unchanged absolute cap near 2h", ttl)
	}

	if err := rdb.HSet(
		context.Background(),
		webSessionKey(hash),
		"absolute_expires_at_unix_ms",
		time.Now().Add(-time.Millisecond).UnixMilli(),
	).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveWebSession(context.Background(), hash, createdAt); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("ResolveWebSession() with Redis-expired absolute time error = %v; want ErrSessionExpired", err)
	}
	assertSessionAndIndexGone(t, rdb, webSessionKey(hash), userWebSessionsKey(user.ID), hash)
}

func TestWebSessionRejectsAbsoluteExpiryBeyondLimit(t *testing.T) {
	store, rdb := newAuthTestStore(t)
	user := createSessionTestUser(t, store, rdb, 1)
	hash := uniqueSessionHash(t, "web_")
	cleanupAuthKeys(t, rdb, webSessionKey(hash))

	createdAt := time.Now().UTC().Truncate(time.Millisecond)
	err := store.CreateWebSession(context.Background(), hash, WebSession{
		UserID:            user.ID,
		CSRFHash:          "csrf-hash",
		PasswordVersion:   user.PasswordVersion,
		CreatedAt:         createdAt,
		LastSeenAt:        createdAt,
		AbsoluteExpiresAt: createdAt.Add(WebSessionAbsoluteTTL + time.Millisecond),
	})
	if err == nil {
		t.Fatal("CreateWebSession() error = nil; want absolute-expiry validation error")
	}
	assertSessionAndIndexGone(t, rdb, webSessionKey(hash), userWebSessionsKey(user.ID), hash)
}

func TestSessionIndexesPruneExpiredMembers(t *testing.T) {
	store, rdb := newAuthTestStore(t)
	user := createSessionTestUser(t, store, rdb, 1)
	webHash := uniqueSessionHash(t, "web_")
	deviceHash := uniqueSessionHash(t, "device_")
	cleanupAuthKeys(t, rdb, webSessionKey(webHash), deviceSessionKey(deviceHash))

	ctx := context.Background()
	staleWeb := uniqueSessionHash(t, "stale_web_")
	staleDevice := uniqueSessionHash(t, "stale_device_")
	staleScore := float64(time.Now().Add(-time.Minute).UnixMilli())
	if err := rdb.ZAdd(ctx, userWebSessionsKey(user.ID), goredis.Z{Score: staleScore, Member: staleWeb}).Err(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.ZAdd(ctx, userDeviceSessionsKey(user.ID), goredis.Z{Score: staleScore, Member: staleDevice}).Err(); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := store.CreateWebSession(ctx, webHash, WebSession{
		UserID:            user.ID,
		CSRFHash:          "csrf-hash",
		PasswordVersion:   user.PasswordVersion,
		CreatedAt:         now,
		LastSeenAt:        now,
		AbsoluteExpiresAt: now.Add(WebSessionAbsoluteTTL),
	}); err != nil {
		t.Fatal(err)
	}
	createDeviceSessionForTest(t, store, rdb, user, deviceHash, "device-id", now)
	assertZMemberGone(t, rdb, userWebSessionsKey(user.ID), staleWeb)
	assertZMemberGone(t, rdb, userDeviceSessionsKey(user.ID), staleDevice)

	staleWeb = uniqueSessionHash(t, "stale_web_")
	staleDevice = uniqueSessionHash(t, "stale_device_")
	if err := rdb.ZAdd(ctx, userWebSessionsKey(user.ID), goredis.Z{Score: staleScore, Member: staleWeb}).Err(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.ZAdd(ctx, userDeviceSessionsKey(user.ID), goredis.Z{Score: staleScore, Member: staleDevice}).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveWebSession(ctx, webHash, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveDeviceSession(ctx, deviceHash, time.Now()); err != nil {
		t.Fatal(err)
	}
	assertZMemberGone(t, rdb, userWebSessionsKey(user.ID), staleWeb)
	assertZMemberGone(t, rdb, userDeviceSessionsKey(user.ID), staleDevice)
}

func TestRevokeAllUserSessions(t *testing.T) {
	store, rdb := newAuthTestStore(t)
	user := createSessionTestUser(t, store, rdb, 1)
	webHashes := []string{uniqueSessionHash(t, "web_"), uniqueSessionHash(t, "web_")}
	deviceHashes := []string{uniqueSessionHash(t, "device_"), uniqueSessionHash(t, "device_")}
	keys := make([]string, 0, len(webHashes)+len(deviceHashes))
	for _, hash := range webHashes {
		keys = append(keys, webSessionKey(hash))
	}
	for _, hash := range deviceHashes {
		keys = append(keys, deviceSessionKey(hash))
	}
	cleanupAuthKeys(t, rdb, keys...)

	now := time.Now().UTC().Truncate(time.Millisecond)
	for _, hash := range webHashes {
		if err := store.CreateWebSession(context.Background(), hash, WebSession{
			UserID:            user.ID,
			CSRFHash:          "csrf-hash",
			PasswordVersion:   user.PasswordVersion,
			CreatedAt:         now,
			LastSeenAt:        now,
			AbsoluteExpiresAt: now.Add(WebSessionAbsoluteTTL),
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, hash := range deviceHashes {
		if err := store.CreateDeviceSession(context.Background(), hash, DeviceSession{
			UserID:          user.ID,
			DeviceID:        "device-id",
			PasswordVersion: user.PasswordVersion,
			CreatedAt:       now,
			LastSeenAt:      now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, hash := range webHashes {
		assertSessionIndexScoreMatchesExpiry(
			t,
			rdb,
			webSessionKey(hash),
			userWebSessionsKey(user.ID),
			hash,
		)
	}
	for _, hash := range deviceHashes {
		assertSessionIndexScoreMatchesExpiry(
			t,
			rdb,
			deviceSessionKey(hash),
			userDeviceSessionsKey(user.ID),
			hash,
		)
	}

	staleScore := float64(time.Now().Add(-time.Minute).UnixMilli())
	if err := rdb.ZAdd(context.Background(), userWebSessionsKey(user.ID), goredis.Z{Score: staleScore, Member: "expired-web"}).Err(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.ZAdd(context.Background(), userDeviceSessionsKey(user.ID), goredis.Z{Score: staleScore, Member: "expired-device"}).Err(); err != nil {
		t.Fatal(err)
	}

	if err := store.RevokeAllUserSessions(context.Background(), user.ID); err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		exists, err := rdb.Exists(context.Background(), key).Result()
		if err != nil {
			t.Fatal(err)
		}
		if exists != 0 {
			t.Fatalf("session key %q remains after RevokeAllUserSessions()", key)
		}
	}
	for _, indexKey := range []string{userWebSessionsKey(user.ID), userDeviceSessionsKey(user.ID)} {
		count, err := rdb.ZCard(context.Background(), indexKey).Result()
		if err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("session index %q has %d members after revoke-all", indexKey, count)
		}
	}
}

func assertSessionIndexScoreMatchesExpiry(
	t *testing.T,
	rdb *redis.Client,
	sessionKey, indexKey, hash string,
) {
	t.Helper()
	before := time.Now()
	ttl, err := rdb.PTTL(context.Background(), sessionKey).Result()
	after := time.Now()
	if err != nil {
		t.Fatal(err)
	}
	if ttl <= 0 {
		t.Fatalf("PTTL(%q) = %s; want a live session", sessionKey, ttl)
	}
	score, err := rdb.ZScore(context.Background(), indexKey, hash).Result()
	if err != nil {
		t.Fatal(err)
	}

	got := int64(score)
	const tolerance = 2 * time.Second
	earliest := before.Add(ttl).Add(-tolerance).UnixMilli()
	latest := after.Add(ttl).Add(tolerance).UnixMilli()
	if got < earliest || got > latest {
		t.Errorf(
			"ZScore(%q, %q) = %d; want Unix-millisecond expiry between %d and %d",
			indexKey,
			hash,
			got,
			earliest,
			latest,
		)
	}
}

func createSessionTestUser(t *testing.T, store *Store, rdb *redis.Client, passwordVersion int64) User {
	t.Helper()
	user := newTestUser(t, uniqueTestUsername(t), passwordVersion)
	cleanupAuthKeys(t, rdb,
		userKey(user.ID),
		usernameKey(user.UsernameNormalized),
		userTeamsKey(user.ID),
		userWebSessionsKey(user.ID),
		userDeviceSessionsKey(user.ID),
	)
	if err := store.CreateUser(context.Background(), user); err != nil {
		t.Fatal(err)
	}
	return user
}

func createDeviceSessionForTest(
	t *testing.T,
	store *Store,
	rdb *redis.Client,
	user User,
	hash string,
	deviceID string,
	createdAt time.Time,
) {
	t.Helper()
	if deviceID == "" {
		var err error
		deviceID, err = NewDeviceID()
		if err != nil {
			t.Fatal(err)
		}
	}
	cleanupAuthKeys(t, rdb, deviceKey(deviceID))
	if err := store.CreateDeviceCredential(context.Background(), Device{
		ID:          deviceID,
		UserID:      user.ID,
		Name:        "test-device",
		SessionHash: hash,
		CreatedAt:   createdAt,
		LastSeenAt:  createdAt,
	}, hash, DeviceSession{
		UserID:          user.ID,
		DeviceID:        deviceID,
		PasswordVersion: user.PasswordVersion,
		CreatedAt:       createdAt,
		LastSeenAt:      createdAt,
	}); err != nil {
		t.Fatal(err)
	}
}

func uniqueSessionHash(t *testing.T, prefix string) string {
	t.Helper()
	hash, err := NewSecret(prefix, 12)
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

func assertTTLNear(t *testing.T, rdb *redis.Client, key string, want time.Duration) {
	t.Helper()
	ttl, err := rdb.PTTL(context.Background(), key).Result()
	if err != nil {
		t.Fatal(err)
	}
	if ttl < want-5*time.Second || ttl > want {
		t.Fatalf("PTTL(%q) = %s; want within 5s of %s", key, ttl, want)
	}
}

func assertZScoreBetween(
	t *testing.T,
	rdb *redis.Client,
	key, member string,
	earliest, latest time.Time,
) {
	t.Helper()
	score, err := rdb.ZScore(context.Background(), key, member).Result()
	if err != nil {
		t.Fatal(err)
	}
	got := int64(score)
	if got < earliest.UnixMilli()-2000 || got > latest.UnixMilli()+2000 {
		t.Fatalf(
			"ZScore(%q, %q) = %d; want Unix-millisecond score between %d and %d",
			key,
			member,
			got,
			earliest.UnixMilli(),
			latest.UnixMilli(),
		)
	}
}

func assertSessionAndIndexGone(t *testing.T, rdb *redis.Client, sessionKey, indexKey, hash string) {
	t.Helper()
	exists, err := rdb.Exists(context.Background(), sessionKey).Result()
	if err != nil {
		t.Fatal(err)
	}
	if exists != 0 {
		t.Fatalf("session key %q still exists", sessionKey)
	}
	assertZMemberGone(t, rdb, indexKey, hash)
}

func assertZMemberGone(t *testing.T, rdb *redis.Client, key, member string) {
	t.Helper()
	if _, err := rdb.ZScore(context.Background(), key, member).Result(); !errors.Is(err, goredis.Nil) {
		t.Fatalf("ZScore(%q, %q) error = %v; want redis.Nil", key, member, err)
	}
}
