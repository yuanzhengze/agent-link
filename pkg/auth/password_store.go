package auth

import (
	"context"
	"fmt"

	goredis "github.com/redis/go-redis/v9"
)

var updateUserPasswordAndRevokeSessionsScript = goredis.NewScript(`
local function key_type(key)
  return redis.call('TYPE', key)['ok']
end

local user_type = key_type(KEYS[1])
if user_type == 'none' then return 0 end
if user_type ~= 'hash' then return -1 end
local web_index_type = key_type(KEYS[2])
if web_index_type ~= 'none' and web_index_type ~= 'zset' then return -1 end
local device_index_type = key_type(KEYS[3])
if device_index_type ~= 'none' and device_index_type ~= 'zset' then return -1 end
local devices_type = key_type(KEYS[4])
if devices_type ~= 'none' and devices_type ~= 'set' then return -1 end
if ARGV[2] ~= '0' and ARGV[2] ~= '1' then return -1 end

local current_version = redis.call('HGET', KEYS[1], 'password_version')
if not current_version or not string.match(current_version, '^%d+$') then return -1 end

local web_sessions = {}
if web_index_type == 'zset' then
  web_sessions = redis.call('ZRANGE', KEYS[2], 0, -1)
end
local device_sessions = {}
if device_index_type == 'zset' then
  device_sessions = redis.call('ZRANGE', KEYS[3], 0, -1)
end
local devices = {}
if devices_type == 'set' then
  devices = redis.call('SMEMBERS', KEYS[4])
end
local device_hashes = {}

-- Validate every dynamically discovered key before the first write.
for _, hash in ipairs(web_sessions) do
  local session_key = ARGV[3] .. hash
  local session_type = key_type(session_key)
  if session_type ~= 'none' and session_type ~= 'hash' then return -1 end
  if session_type == 'hash' then
    local session_user = redis.call('HGET', session_key, 'user_id')
    if session_user and session_user ~= ARGV[7] then return -1 end
  end
end

for _, hash in ipairs(device_sessions) do
  local session_key = ARGV[4] .. hash
  local session_type = key_type(session_key)
  if session_type ~= 'none' and session_type ~= 'hash' then return -1 end
  if session_type == 'hash' then
    local session_user = redis.call('HGET', session_key, 'user_id')
    if session_user and session_user ~= ARGV[7] then return -1 end
  end

  local binding_key = ARGV[6] .. hash
  local binding_type = key_type(binding_key)
  if binding_type ~= 'none' and binding_type ~= 'hash' then return -1 end
  if binding_type == 'hash' then
    local binding_user = redis.call('HGET', binding_key, 'user_id')
    if not binding_user or binding_user ~= ARGV[7] then return -1 end
  end
end

for index, device_id in ipairs(devices) do
  local device_key = ARGV[5] .. device_id
  local device_type = key_type(device_key)
  if device_type ~= 'none' and device_type ~= 'hash' then return -1 end
  if device_type == 'hash' then
    local values = redis.call('HMGET', device_key, 'user_id', 'session_hash')
    if not values[1] or values[1] ~= ARGV[7] then return -1 end
    device_hashes[index] = values[2] or ''
    if values[2] then
      local binding_key = ARGV[6] .. values[2]
      local binding_type = key_type(binding_key)
      if binding_type ~= 'none' and binding_type ~= 'hash' then return -1 end
      if binding_type == 'hash' then
        local binding_values = redis.call('HMGET', binding_key, 'user_id', 'device_id')
        if not binding_values[1] or binding_values[1] ~= ARGV[7] or
           not binding_values[2] or binding_values[2] ~= device_id then
          return -1
        end
      end
    end
  else
    device_hashes[index] = ''
  end
end

redis.call('HINCRBY', KEYS[1], 'password_version', 1)
redis.call('HSET', KEYS[1], 'password_phc', ARGV[1], 'must_change_password', ARGV[2])

for _, hash in ipairs(web_sessions) do
  redis.call('DEL', ARGV[3] .. hash)
end
for _, hash in ipairs(device_sessions) do
  redis.call('DEL', ARGV[4] .. hash)
  redis.call('DEL', ARGV[6] .. hash)
end
for index, device_id in ipairs(devices) do
  local device_key = ARGV[5] .. device_id
  local hash = device_hashes[index]
  if hash ~= '' then redis.call('DEL', ARGV[6] .. hash) end
  redis.call('DEL', device_key)
end
redis.call('DEL', KEYS[2], KEYS[3], KEYS[4])
return 1
`)

func (s *Store) UpdateUserPasswordAndRevokeSessions(
	ctx context.Context,
	userID, passwordPHC string,
	mustChange bool,
) error {
	mustChangeValue := "0"
	if mustChange {
		mustChangeValue = "1"
	}
	updated, err := updateUserPasswordAndRevokeSessionsScript.Run(
		ctx,
		s.rdb,
		[]string{
			userKey(userID),
			userWebSessionsKey(userID),
			userDeviceSessionsKey(userID),
			userDevicesKey(userID),
		},
		passwordPHC,
		mustChangeValue,
		"agentlink:v2:web_session:",
		"agentlink:v2:device_session:",
		"agentlink:v2:device:",
		"agentlink:v2:device_credential:",
		userID,
	).Int64()
	if err != nil {
		return fmt.Errorf("update user password and revoke sessions: %w", err)
	}
	if updated == 0 {
		return ErrNotFound
	}
	if updated < 0 {
		return fmt.Errorf("update user password and revoke sessions: invalid Redis state")
	}
	return nil
}
