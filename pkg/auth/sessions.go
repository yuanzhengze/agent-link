package auth

import (
	"context"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

const (
	WebSessionIdleTTL        = 12 * time.Hour
	WebSessionAbsoluteTTL    = 7 * 24 * time.Hour
	DeviceSessionIdleTTL     = 90 * 24 * time.Hour
	sessionUserKeyPrefix     = "agentlink:v2:user:"
	webSessionIndexSuffix    = ":web_sessions"
	deviceSessionIndexSuffix = ":device_sessions"
)

type WebSession struct {
	UserID            string
	CSRFHash          string
	PasswordVersion   int64
	CreatedAt         time.Time
	LastSeenAt        time.Time
	AbsoluteExpiresAt time.Time
}

type DeviceSession struct {
	UserID          string
	DeviceID        string
	PasswordVersion int64
	CreatedAt       time.Time
	LastSeenAt      time.Time
}

var createWebSessionScript = goredis.NewScript(`
local redis_time = redis.call('TIME')
local now_ms = (tonumber(redis_time[1]) * 1000) + math.floor(tonumber(redis_time[2]) / 1000)
local now_seconds = math.floor(now_ms / 1000)
redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', now_seconds)

local absolute_ms = tonumber(ARGV[7])
if not absolute_ms or absolute_ms <= now_ms then return 0 end
local expires_ms = now_ms + tonumber(ARGV[8])
if absolute_ms < expires_ms then expires_ms = absolute_ms end

redis.call('HSET', KEYS[1],
  'user_id', ARGV[1], 'csrf_hash', ARGV[2], 'password_version', ARGV[3],
  'created_at', ARGV[4], 'last_seen_at', ARGV[5],
  'absolute_expires_at', ARGV[6], 'absolute_expires_at_unix_ms', ARGV[7])
redis.call('PEXPIREAT', KEYS[1], expires_ms)
redis.call('ZADD', KEYS[2], math.floor(expires_ms / 1000), ARGV[9])
return 1
`)

var createDeviceSessionScript = goredis.NewScript(`
local redis_time = redis.call('TIME')
local now_ms = (tonumber(redis_time[1]) * 1000) + math.floor(tonumber(redis_time[2]) / 1000)
local now_seconds = math.floor(now_ms / 1000)
redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', now_seconds)

redis.call('HSET', KEYS[1],
  'user_id', ARGV[1], 'device_id', ARGV[2], 'password_version', ARGV[3],
  'created_at', ARGV[4], 'last_seen_at', ARGV[5])
redis.call('PEXPIRE', KEYS[1], ARGV[6])
redis.call('ZADD', KEYS[2], math.floor((now_ms + tonumber(ARGV[6])) / 1000), ARGV[7])
return 1
`)

var resolveWebSessionScript = goredis.NewScript(`
local user_id = redis.call('HGET', KEYS[1], 'user_id')
if not user_id then return {0} end

local index_key = ARGV[6] .. user_id .. ARGV[7]
redis.call('ZREMRANGEBYSCORE', index_key, '-inf', ARGV[1])

local values = redis.call('HMGET', KEYS[1],
  'csrf_hash', 'password_version', 'created_at', 'absolute_expires_at',
  'absolute_expires_at_unix_ms')
local absolute_ms = tonumber(values[5])
local now_ms = tonumber(ARGV[2])
if not absolute_ms or now_ms >= absolute_ms then
  redis.call('DEL', KEYS[1])
  redis.call('ZREM', index_key, ARGV[8])
  return {0}
end

local current_version = redis.call('HGET', ARGV[6] .. user_id, 'password_version')
if not current_version or current_version ~= values[2] then
  redis.call('DEL', KEYS[1])
  redis.call('ZREM', index_key, ARGV[8])
  return {0}
end

local redis_time = redis.call('TIME')
local redis_now_ms = (tonumber(redis_time[1]) * 1000) + math.floor(tonumber(redis_time[2]) / 1000)
if redis_now_ms >= absolute_ms then
  redis.call('DEL', KEYS[1])
  redis.call('ZREM', index_key, ARGV[8])
  return {0}
end
local expires_ms = now_ms + tonumber(ARGV[4])
if absolute_ms < expires_ms then expires_ms = absolute_ms end
if expires_ms <= redis_now_ms then
  redis.call('DEL', KEYS[1])
  redis.call('ZREM', index_key, ARGV[8])
  return {0}
end

redis.call('HSET', KEYS[1], 'last_seen_at', ARGV[3])
redis.call('PEXPIREAT', KEYS[1], expires_ms)
redis.call('ZADD', index_key, math.floor(expires_ms / 1000), ARGV[8])
return {1, user_id, values[1], values[2], values[3], ARGV[3], values[4]}
`)

var resolveDeviceSessionScript = goredis.NewScript(`
local user_id = redis.call('HGET', KEYS[1], 'user_id')
if not user_id then return {0} end

local index_key = ARGV[4] .. user_id .. ARGV[5]
redis.call('ZREMRANGEBYSCORE', index_key, '-inf', ARGV[1])

local values = redis.call('HMGET', KEYS[1],
  'device_id', 'password_version', 'created_at')
local current_version = redis.call('HGET', ARGV[4] .. user_id, 'password_version')
if not current_version or current_version ~= values[2] then
  redis.call('DEL', KEYS[1])
  redis.call('ZREM', index_key, ARGV[6])
  return {0}
end

local redis_time = redis.call('TIME')
local redis_now_ms = (tonumber(redis_time[1]) * 1000) + math.floor(tonumber(redis_time[2]) / 1000)
redis.call('HSET', KEYS[1], 'last_seen_at', ARGV[2])
redis.call('PEXPIRE', KEYS[1], ARGV[3])
redis.call('ZADD', index_key, math.floor((redis_now_ms + tonumber(ARGV[3])) / 1000), ARGV[6])
return {1, user_id, values[1], values[2], values[3], ARGV[2]}
`)

var revokeSessionScript = goredis.NewScript(`
local user_id = redis.call('HGET', KEYS[1], 'user_id')
if not user_id then
  redis.call('DEL', KEYS[1])
  return 0
end
local index_key = ARGV[1] .. user_id .. ARGV[2]
redis.call('DEL', KEYS[1])
redis.call('ZREM', index_key, ARGV[3])
return 1
`)

var revokeAllUserSessionsScript = goredis.NewScript(`
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', ARGV[1])
redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', ARGV[1])

local web_sessions = redis.call('ZRANGE', KEYS[1], 0, -1)
for _, hash in ipairs(web_sessions) do
  redis.call('DEL', ARGV[2] .. hash)
end
local device_sessions = redis.call('ZRANGE', KEYS[2], 0, -1)
for _, hash in ipairs(device_sessions) do
  redis.call('DEL', ARGV[3] .. hash)
end
redis.call('DEL', KEYS[1], KEYS[2])
return #web_sessions + #device_sessions
`)

func (s *Store) CreateWebSession(ctx context.Context, sessionHash string, ws WebSession) error {
	if ws.AbsoluteExpiresAt.After(ws.CreatedAt.Add(WebSessionAbsoluteTTL)) {
		return fmt.Errorf("web session absolute expiry exceeds %s", WebSessionAbsoluteTTL)
	}

	created, err := createWebSessionScript.Run(
		ctx,
		s.rdb,
		[]string{webSessionKey(sessionHash), userWebSessionsKey(ws.UserID)},
		ws.UserID,
		ws.CSRFHash,
		ws.PasswordVersion,
		formatRedisTime(ws.CreatedAt),
		formatRedisTime(ws.LastSeenAt),
		formatRedisTime(ws.AbsoluteExpiresAt),
		ws.AbsoluteExpiresAt.UnixMilli(),
		WebSessionIdleTTL.Milliseconds(),
		sessionHash,
	).Int64()
	if err != nil {
		return fmt.Errorf("create web session: %w", err)
	}
	if created == 0 {
		return ErrSessionExpired
	}
	return nil
}

func (s *Store) ResolveWebSession(ctx context.Context, sessionHash string, now time.Time) (WebSession, error) {
	result, err := resolveWebSessionScript.Run(
		ctx,
		s.rdb,
		[]string{webSessionKey(sessionHash)},
		now.Unix(),
		now.UnixMilli(),
		formatRedisTime(now),
		WebSessionIdleTTL.Milliseconds(),
		sessionHash,
		sessionUserKeyPrefix,
		webSessionIndexSuffix,
		sessionHash,
	).Slice()
	if err != nil {
		return WebSession{}, fmt.Errorf("resolve web session: %w", err)
	}
	if sessionReplyExpired(result) {
		return WebSession{}, ErrSessionExpired
	}
	if len(result) != 7 {
		return WebSession{}, fmt.Errorf("resolve web session: unexpected result length %d", len(result))
	}

	passwordVersion, err := parseRedisInt64(redisReplyString(result[3]), "password_version")
	if err != nil {
		return WebSession{}, err
	}
	createdAt, err := parseRedisTime(redisReplyString(result[4]), "created_at")
	if err != nil {
		return WebSession{}, err
	}
	lastSeenAt, err := parseRedisTime(redisReplyString(result[5]), "last_seen_at")
	if err != nil {
		return WebSession{}, err
	}
	absoluteExpiresAt, err := parseRedisTime(redisReplyString(result[6]), "absolute_expires_at")
	if err != nil {
		return WebSession{}, err
	}
	return WebSession{
		UserID:            redisReplyString(result[1]),
		CSRFHash:          redisReplyString(result[2]),
		PasswordVersion:   passwordVersion,
		CreatedAt:         createdAt,
		LastSeenAt:        lastSeenAt,
		AbsoluteExpiresAt: absoluteExpiresAt,
	}, nil
}

func (s *Store) RevokeWebSession(ctx context.Context, sessionHash string) error {
	if err := revokeSessionScript.Run(
		ctx,
		s.rdb,
		[]string{webSessionKey(sessionHash)},
		sessionUserKeyPrefix,
		webSessionIndexSuffix,
		sessionHash,
	).Err(); err != nil {
		return fmt.Errorf("revoke web session: %w", err)
	}
	return nil
}

func (s *Store) CreateDeviceSession(ctx context.Context, sessionHash string, ds DeviceSession) error {
	if err := createDeviceSessionScript.Run(
		ctx,
		s.rdb,
		[]string{deviceSessionKey(sessionHash), userDeviceSessionsKey(ds.UserID)},
		ds.UserID,
		ds.DeviceID,
		ds.PasswordVersion,
		formatRedisTime(ds.CreatedAt),
		formatRedisTime(ds.LastSeenAt),
		DeviceSessionIdleTTL.Milliseconds(),
		sessionHash,
	).Err(); err != nil {
		return fmt.Errorf("create device session: %w", err)
	}
	return nil
}

func (s *Store) ResolveDeviceSession(ctx context.Context, sessionHash string, now time.Time) (DeviceSession, error) {
	result, err := resolveDeviceSessionScript.Run(
		ctx,
		s.rdb,
		[]string{deviceSessionKey(sessionHash)},
		now.Unix(),
		formatRedisTime(now),
		DeviceSessionIdleTTL.Milliseconds(),
		sessionUserKeyPrefix,
		deviceSessionIndexSuffix,
		sessionHash,
	).Slice()
	if err != nil {
		return DeviceSession{}, fmt.Errorf("resolve device session: %w", err)
	}
	if sessionReplyExpired(result) {
		return DeviceSession{}, ErrSessionExpired
	}
	if len(result) != 6 {
		return DeviceSession{}, fmt.Errorf("resolve device session: unexpected result length %d", len(result))
	}

	passwordVersion, err := parseRedisInt64(redisReplyString(result[3]), "password_version")
	if err != nil {
		return DeviceSession{}, err
	}
	createdAt, err := parseRedisTime(redisReplyString(result[4]), "created_at")
	if err != nil {
		return DeviceSession{}, err
	}
	lastSeenAt, err := parseRedisTime(redisReplyString(result[5]), "last_seen_at")
	if err != nil {
		return DeviceSession{}, err
	}
	return DeviceSession{
		UserID:          redisReplyString(result[1]),
		DeviceID:        redisReplyString(result[2]),
		PasswordVersion: passwordVersion,
		CreatedAt:       createdAt,
		LastSeenAt:      lastSeenAt,
	}, nil
}

func (s *Store) RevokeDeviceSession(ctx context.Context, sessionHash string) error {
	if err := revokeSessionScript.Run(
		ctx,
		s.rdb,
		[]string{deviceSessionKey(sessionHash)},
		sessionUserKeyPrefix,
		deviceSessionIndexSuffix,
		sessionHash,
	).Err(); err != nil {
		return fmt.Errorf("revoke device session: %w", err)
	}
	return nil
}

func (s *Store) RevokeAllUserSessions(ctx context.Context, userID string) error {
	now := time.Now()
	if err := revokeAllUserSessionsScript.Run(
		ctx,
		s.rdb,
		[]string{userWebSessionsKey(userID), userDeviceSessionsKey(userID)},
		now.Unix(),
		"agentlink:v2:web_session:",
		"agentlink:v2:device_session:",
	).Err(); err != nil {
		return fmt.Errorf("revoke all user sessions: %w", err)
	}
	return nil
}

func sessionReplyExpired(result []interface{}) bool {
	return len(result) == 1 && redisReplyInt64(result[0]) == 0
}

func redisReplyString(value interface{}) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	return fmt.Sprint(value)
}

func redisReplyInt64(value interface{}) int64 {
	switch value := value.(type) {
	case int64:
		return value
	case int:
		return int64(value)
	default:
		return -1
	}
}
