package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

const (
	loginFailureMaxFailures = 5
	loginFailureWindow      = 15 * time.Minute
)

var recordLoginFailureScript = goredis.NewScript(`
local t1 = redis.call('TYPE', KEYS[1])['ok']
local t2 = redis.call('TYPE', KEYS[2])['ok']
if t1 ~= 'none' and t1 ~= 'string' then return {-1, -1} end
if t2 ~= 'none' and t2 ~= 'string' then return {-1, -1} end

local user_count = redis.call('INCR', KEYS[1])
if user_count == 1 then
  redis.call('EXPIRE', KEYS[1], ARGV[1])
end
local ip_count = redis.call('INCR', KEYS[2])
if ip_count == 1 then
  redis.call('EXPIRE', KEYS[2], ARGV[1])
end
return {user_count, ip_count}
`)

func (s *Store) GetLoginFailureCounts(ctx context.Context, normalizedUsername, ip string) (int64, int64, error) {
	userCount, err := s.loginFailureCount(ctx, loginFailUserKey(normalizedUsername))
	if err != nil {
		return 0, 0, err
	}
	ipCount, err := s.loginFailureCount(ctx, loginFailIPKey(ip))
	if err != nil {
		return 0, 0, err
	}
	return userCount, ipCount, nil
}

func (s *Store) loginFailureCount(ctx context.Context, key string) (int64, error) {
	value, err := s.rdb.Get(ctx, key).Result()
	if errors.Is(err, goredis.Nil) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("get login failure counter: %w", err)
	}
	count, err := parseRedisInt64(value, "login_failure_count")
	if err != nil {
		return 0, err
	}
	return count, nil
}

func (s *Store) RecordLoginFailure(ctx context.Context, normalizedUsername, ip string) (int64, int64, error) {
	result, err := recordLoginFailureScript.Run(
		ctx,
		s.rdb,
		[]string{loginFailUserKey(normalizedUsername), loginFailIPKey(ip)},
		int64(loginFailureWindow.Seconds()),
	).Slice()
	if err != nil {
		return 0, 0, fmt.Errorf("record login failure: %w", err)
	}
	if len(result) != 2 {
		return 0, 0, fmt.Errorf("record login failure: unexpected result length %d", len(result))
	}
	userCount := redisReplyInt64(result[0])
	ipCount := redisReplyInt64(result[1])
	if userCount < 0 || ipCount < 0 {
		return 0, 0, errors.New("login failure counter has wrong type")
	}
	return userCount, ipCount, nil
}

func (s *Store) ClearLoginRateLimit(ctx context.Context, normalizedUsername, ip string) error {
	if err := s.rdb.Del(ctx, loginFailUserKey(normalizedUsername), loginFailIPKey(ip)).Err(); err != nil {
		return fmt.Errorf("clear login rate limit: %w", err)
	}
	return nil
}
