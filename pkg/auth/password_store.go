package auth

import (
	"context"
	"fmt"

	goredis "github.com/redis/go-redis/v9"
)

var updateUserPasswordAndRevokeSessionsScript = goredis.NewScript(`
local function allowed_type(key, allowed_none, allowed_main)
  local t = redis.call('TYPE', key)['ok']
  if allowed_none and t == 'none' then return true end
  if allowed_main and t == allowed_main then return true end
  return false
end

if not allowed_type(KEYS[1], false, 'hash') then return {0} end
if not allowed_type(KEYS[2], true, 'zset') then return {0} end
if not allowed_type(KEYS[3], true, 'zset') then return {0} end
if not allowed_type(KEYS[4], true, 'set') then return {0} end

local redis_time = redis.call('TIME')
local redis_now_ms = (tonumber(redis_time[1]) * 1000) + math.floor(tonumber(redis_time[2]) / 1000)

local web_sessions = {}
if redis.call('EXISTS', KEYS[2]) == 1 then
  redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', redis_now_ms)
  web_sessions = redis.call('ZRANGE', KEYS[2], 0, -1)
end

local device_sessions = {}
if redis.call('EXISTS', KEYS[3]) == 1 then
  redis.call('ZREMRANGEBYSCORE', KEYS[3], '-inf', redis_now_ms)
  device_sessions = redis.call('ZRANGE', KEYS[3], 0, -1)
end

local devices = {}
if redis.call('EXISTS', KEYS[4]) == 1 then
  devices = redis.call('SMEMBERS', KEYS[4])
end

redis.call('HSET', KEYS[1],
  'password_phc', ARGV[1],
  'password_version', ARGV[2],
  'must_change_password', ARGV[3])

for _, hash in ipairs(web_sessions) do
  redis.call('DEL', 'agentlink:v2:web_session:' .. hash)
end
for _, hash in ipairs(device_sessions) do
  redis.call('DEL', 'agentlink:v2:device_session:' .. hash)
end
for _, device_id in ipairs(devices) do
  redis.call('DEL', 'agentlink:v2:device:' .. device_id)
end
redis.call('DEL', KEYS[2], KEYS[3], KEYS[4])
return 1
`)

func (s *Store) UpdateUserPasswordAndRevokeSessions(
	ctx context.Context,
	userID, passwordPHC string,
	passwordVersion int64,
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
		passwordVersion,
		mustChangeValue,
	).Int64()
	if err != nil {
		return fmt.Errorf("update user password and revoke sessions: %w", err)
	}
	if updated == 0 {
		return ErrNotFound
	}
	return nil
}
