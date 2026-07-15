package auth

import (
	"context"
	"errors"
	"fmt"

	goredis "github.com/redis/go-redis/v9"
)

var createDeviceCredentialScript = goredis.NewScript(`
local function valid_type(key, a, b)
  local t = redis.call('TYPE', key)['ok']
  if t == 'none' then return true end
  if a and t == a then return true end
  if b and t == b then return true end
  return false
end

if not valid_type(KEYS[1], nil, nil) then return {0} end
if not valid_type(KEYS[2], 'set', nil) then return {0} end
if not valid_type(KEYS[3], nil, nil) then return {0} end
if not valid_type(KEYS[4], 'zset', nil) then return {0} end
if redis.call('EXISTS', KEYS[1]) == 1 then return {0} end
if redis.call('EXISTS', KEYS[3]) == 1 then return {0} end

local redis_time = redis.call('TIME')
local redis_now_ms = (tonumber(redis_time[1]) * 1000) + math.floor(tonumber(redis_time[2]) / 1000)
redis.call('ZREMRANGEBYSCORE', KEYS[4], '-inf', redis_now_ms)

redis.call('HSET', KEYS[1],
  'id', ARGV[1], 'user_id', ARGV[2], 'name', ARGV[3],
  'session_hash', ARGV[4], 'created_at', ARGV[5], 'last_seen_at', ARGV[6])
redis.call('SADD', KEYS[2], ARGV[1])

redis.call('HSET', KEYS[3],
  'user_id', ARGV[2], 'device_id', ARGV[1], 'password_version', ARGV[7],
  'created_at', ARGV[8], 'last_seen_at', ARGV[9])
redis.call('PEXPIRE', KEYS[3], ARGV[10])
redis.call('ZADD', KEYS[4], redis_now_ms + tonumber(ARGV[10]), ARGV[11])
return 1
`)

var revokeDeviceCredentialScript = goredis.NewScript(`
local user_id = redis.call('HGET', KEYS[1], 'user_id')
local device_id = redis.call('HGET', KEYS[1], 'device_id')

if user_id then
  local index_key = ARGV[1] .. user_id .. ARGV[2]
  redis.call('ZREM', index_key, ARGV[3])
end
redis.call('DEL', KEYS[1])

if device_id then
  redis.call('DEL', ARGV[4] .. device_id)
  if user_id then
    redis.call('SREM', ARGV[1] .. user_id .. ':devices', device_id)
  end
end
return 1
`)

func (s *Store) Device(ctx context.Context, id string) (Device, error) {
	fields, err := s.rdb.HGetAll(ctx, deviceKey(id)).Result()
	if err != nil {
		return Device{}, fmt.Errorf("get device: %w", err)
	}
	if len(fields) == 0 {
		return Device{}, ErrNotFound
	}
	createdAt, err := parseRedisTime(fields["created_at"], "created_at")
	if err != nil {
		return Device{}, err
	}
	lastSeenAt, err := parseRedisTime(fields["last_seen_at"], "last_seen_at")
	if err != nil {
		return Device{}, err
	}
	return Device{
		ID:          fields["id"],
		UserID:      fields["user_id"],
		Name:        fields["name"],
		SessionHash: fields["session_hash"],
		CreatedAt:   createdAt,
		LastSeenAt:  lastSeenAt,
	}, nil
}

func (s *Store) DeleteDevice(ctx context.Context, id string) error {
	if err := s.rdb.Del(ctx, deviceKey(id)).Err(); err != nil {
		return fmt.Errorf("delete device: %w", err)
	}
	return nil
}

func (s *Store) CreateDeviceCredential(
	ctx context.Context,
	device Device,
	sessionHash string,
	ds DeviceSession,
) error {
	created, err := createDeviceCredentialScript.Run(
		ctx,
		s.rdb,
		[]string{
			deviceKey(device.ID),
			userDevicesKey(device.UserID),
			deviceSessionKey(sessionHash),
			userDeviceSessionsKey(device.UserID),
		},
		device.ID,
		device.UserID,
		device.Name,
		device.SessionHash,
		formatRedisTime(device.CreatedAt),
		formatRedisTime(device.LastSeenAt),
		ds.PasswordVersion,
		formatRedisTime(ds.CreatedAt),
		formatRedisTime(ds.LastSeenAt),
		DeviceSessionIdleTTL.Milliseconds(),
		sessionHash,
	).Int64()
	if err != nil {
		return fmt.Errorf("create device credential: %w", err)
	}
	if created == 0 {
		return errors.New("create device credential rejected")
	}
	return nil
}

func (s *Store) RevokeDeviceCredential(ctx context.Context, sessionHash string) error {
	if err := revokeDeviceCredentialScript.Run(
		ctx,
		s.rdb,
		[]string{deviceSessionKey(sessionHash)},
		sessionUserKeyPrefix,
		deviceSessionIndexSuffix,
		sessionHash,
		"agentlink:v2:device:",
	).Err(); err != nil {
		return fmt.Errorf("revoke device credential: %w", err)
	}
	return nil
}
