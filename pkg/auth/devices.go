package auth

import (
	"context"
	"errors"
	"fmt"

	goredis "github.com/redis/go-redis/v9"
)

var createDeviceCredentialScript = goredis.NewScript(`
local function key_type(key)
  return redis.call('TYPE', key)['ok']
end

if key_type(KEYS[1]) ~= 'none' then return 0 end
if key_type(KEYS[2]) ~= 'none' then return 0 end
local devices_type = key_type(KEYS[3])
if devices_type ~= 'none' and devices_type ~= 'set' then return 0 end
if key_type(KEYS[4]) ~= 'none' then return 0 end
local index_type = key_type(KEYS[5])
if index_type ~= 'none' and index_type ~= 'zset' then return 0 end
if not string.match(ARGV[7], '^%d+$') then return 0 end
local ttl_ms = tonumber(ARGV[10])
if not ttl_ms or ttl_ms <= 0 then return 0 end

local redis_time = redis.call('TIME')
local redis_now_ms = (tonumber(redis_time[1]) * 1000) + math.floor(tonumber(redis_time[2]) / 1000)
if index_type == 'zset' then
  redis.call('ZREMRANGEBYSCORE', KEYS[5], '-inf', redis_now_ms)
end

redis.call('HSET', KEYS[1], 'user_id', ARGV[2], 'device_id', ARGV[1])
redis.call('HSET', KEYS[2],
  'id', ARGV[1], 'user_id', ARGV[2], 'name', ARGV[3],
  'session_hash', ARGV[4], 'created_at', ARGV[5], 'last_seen_at', ARGV[6])
redis.call('SADD', KEYS[3], ARGV[1])

redis.call('HSET', KEYS[4],
  'user_id', ARGV[2], 'device_id', ARGV[1], 'password_version', ARGV[7],
  'created_at', ARGV[8], 'last_seen_at', ARGV[9])
redis.call('PEXPIRE', KEYS[4], ttl_ms)
redis.call('ZADD', KEYS[5], redis_now_ms + ttl_ms, ARGV[11])
return 1
`)

var revokeDeviceCredentialScript = goredis.NewScript(`
local function key_type(key)
  return redis.call('TYPE', key)['ok']
end

local binding_type = key_type(KEYS[1])
local session_type = key_type(KEYS[2])
if binding_type == 'none' and session_type == 'none' then return 0 end
if binding_type ~= 'none' and binding_type ~= 'hash' then
  return redis.error_reply('device credential binding has wrong type')
end
if session_type ~= 'none' and session_type ~= 'hash' then
  return redis.error_reply('device session has wrong type')
end

local binding_user = nil
local binding_device = nil
if binding_type == 'hash' then
  local values = redis.call('HMGET', KEYS[1], 'user_id', 'device_id')
  binding_user = values[1]
  binding_device = values[2]
  if not binding_user or not binding_device then
    return redis.error_reply('device credential binding is incomplete')
  end
end

local session_user = nil
local session_device = nil
if session_type == 'hash' then
  local values = redis.call('HMGET', KEYS[2], 'user_id', 'device_id')
  session_user = values[1]
  session_device = values[2]
  if not session_user or not session_device then
    return redis.error_reply('device session is incomplete')
  end
end

if binding_user and session_user and
   (binding_user ~= session_user or binding_device ~= session_device) then
  return redis.error_reply('device credential binding does not match session')
end

local user_id = binding_user or session_user
local device_id = binding_device or session_device
local device_key = ARGV[4] .. device_id
local device_type = key_type(device_key)
if device_type ~= 'none' and device_type ~= 'hash' then
  return redis.error_reply('device has wrong type')
end
if device_type == 'hash' then
  local values = redis.call('HMGET', device_key, 'user_id', 'session_hash')
  if not values[1] or values[1] ~= user_id or not values[2] or values[2] ~= ARGV[1] then
    return redis.error_reply('device does not match credential binding')
  end
end

local index_key = ARGV[2] .. user_id .. ARGV[3]
local devices_key = ARGV[2] .. user_id .. ':devices'
local index_type = key_type(index_key)
local devices_type = key_type(devices_key)
if index_type ~= 'none' and index_type ~= 'zset' then
  return redis.error_reply('device session index has wrong type')
end
if devices_type ~= 'none' and devices_type ~= 'set' then
  return redis.error_reply('user devices index has wrong type')
end

redis.call('DEL', KEYS[2])
if index_type == 'zset' then redis.call('ZREM', index_key, ARGV[1]) end
redis.call('DEL', device_key)
if devices_type == 'set' then redis.call('SREM', devices_key, device_id) end
redis.call('DEL', KEYS[1])
return 1
`)

var deleteDeviceCredentialScript = goredis.NewScript(`
local function key_type(key)
  return redis.call('TYPE', key)['ok']
end

local device_type = key_type(KEYS[1])
if device_type == 'none' then return 0 end
if device_type ~= 'hash' then return redis.error_reply('device has wrong type') end

local device_values = redis.call('HMGET', KEYS[1], 'user_id', 'session_hash')
local user_id = device_values[1]
local session_hash = device_values[2]
if not user_id or not session_hash then
  return redis.error_reply('device is incomplete')
end

local binding_key = ARGV[3] .. session_hash
local session_key = ARGV[2] .. session_hash
local index_key = ARGV[4] .. user_id .. ARGV[5]
local devices_key = ARGV[4] .. user_id .. ':devices'

local binding_type = key_type(binding_key)
local session_type = key_type(session_key)
local index_type = key_type(index_key)
local devices_type = key_type(devices_key)
if binding_type ~= 'none' and binding_type ~= 'hash' then
  return redis.error_reply('device credential binding has wrong type')
end
if session_type ~= 'none' and session_type ~= 'hash' then
  return redis.error_reply('device session has wrong type')
end
if index_type ~= 'none' and index_type ~= 'zset' then
  return redis.error_reply('device session index has wrong type')
end
if devices_type ~= 'none' and devices_type ~= 'set' then
  return redis.error_reply('user devices index has wrong type')
end

if binding_type == 'hash' then
  local values = redis.call('HMGET', binding_key, 'user_id', 'device_id')
  if not values[1] or values[1] ~= user_id or not values[2] or values[2] ~= ARGV[1] then
    return redis.error_reply('device credential binding does not match device')
  end
end
if session_type == 'hash' then
  local values = redis.call('HMGET', session_key, 'user_id', 'device_id')
  if not values[1] or values[1] ~= user_id or not values[2] or values[2] ~= ARGV[1] then
    return redis.error_reply('device session does not match device')
  end
end

redis.call('DEL', session_key)
if index_type == 'zset' then redis.call('ZREM', index_key, session_hash) end
redis.call('DEL', binding_key)
redis.call('DEL', KEYS[1])
if devices_type == 'set' then redis.call('SREM', devices_key, ARGV[1]) end
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
	if id == "" {
		return errors.New("device ID is required")
	}
	if _, err := deleteDeviceCredentialScript.Run(
		ctx,
		s.rdb,
		[]string{deviceKey(id)},
		id,
		"agentlink:v2:device_session:",
		"agentlink:v2:device_credential:",
		sessionUserKeyPrefix,
		deviceSessionIndexSuffix,
	).Int64(); err != nil {
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
	if device.UserID == "" || ds.UserID == "" || device.UserID != ds.UserID {
		return errors.New("device and session user IDs must match")
	}
	if device.ID == "" || ds.DeviceID == "" || device.ID != ds.DeviceID {
		return errors.New("device and session device IDs must match")
	}
	if sessionHash == "" || device.SessionHash == "" || device.SessionHash != sessionHash {
		return errors.New("device and credential session hashes must match")
	}
	created, err := createDeviceCredentialScript.Run(
		ctx,
		s.rdb,
		[]string{
			deviceCredentialKey(sessionHash),
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
	if sessionHash == "" {
		return nil
	}
	if _, err := revokeDeviceCredentialScript.Run(
		ctx,
		s.rdb,
		[]string{
			deviceCredentialKey(sessionHash),
			deviceSessionKey(sessionHash),
		},
		sessionHash,
		sessionUserKeyPrefix,
		deviceSessionIndexSuffix,
		"agentlink:v2:device:",
	).Int64(); err != nil {
		return fmt.Errorf("revoke device credential: %w", err)
	}
	return nil
}
