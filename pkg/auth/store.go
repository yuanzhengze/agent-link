package auth

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/team/agentlink/pkg/redis"
)

var (
	ErrNotFound           = errors.New("not found")
	ErrUsernameExists     = errors.New("username already exists")
	ErrTeamExists         = errors.New("team already exists")
	ErrAlreadyMember      = errors.New("already a team member")
	ErrInvalidInvite      = errors.New("invalid invite")
	ErrInvalidRole        = errors.New("invalid role")
	ErrStoreInconsistent  = errors.New("store inconsistent")
	ErrNotMember          = errors.New("not a team member")
	ErrForbidden          = errors.New("forbidden")
	ErrSessionExpired     = errors.New("session expired")
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrRateLimited        = errors.New("rate limited")
)

var createUserScript = goredis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 1 then return 0 end
redis.call('SET', KEYS[1], ARGV[1])
redis.call('HSET', KEYS[2],
  'id', ARGV[1], 'username', ARGV[2], 'username_normalized', ARGV[3],
  'password_phc', ARGV[4], 'password_version', ARGV[5],
  'status', 'active', 'must_change_password', '0', 'created_at', ARGV[6])
return 1
`)

var createTeamScript = goredis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 1 then return 0 end
local members_type = redis.call('TYPE', KEYS[2]).ok
if members_type ~= 'none' and members_type ~= 'hash' then
  return redis.error_reply('ERR team members key must be none or hash')
end
local user_teams_type = redis.call('TYPE', KEYS[3]).ok
if user_teams_type ~= 'none' and user_teams_type ~= 'set' then
  return redis.error_reply('ERR owner teams key must be none or set')
end
redis.call('HSET', KEYS[1],
  'id', ARGV[1], 'name', ARGV[2], 'owner_user_id', ARGV[3],
  'invite_hash', ARGV[4], 'invite_version', '1', 'created_at', ARGV[5])
redis.call('HSET', KEYS[2], ARGV[3], 'owner')
redis.call('SADD', KEYS[3], ARGV[1])
return 1
`)

type Store struct {
	rdb *redis.Client
}

func NewStore(rdb *redis.Client) *Store {
	return &Store{rdb: rdb}
}

func (s *Store) CreateUser(ctx context.Context, user User) error {
	normalized, err := NormalizeUsername(user.Username)
	if err != nil {
		return fmt.Errorf("normalize username: %w", err)
	}
	if user.UsernameNormalized != "" && user.UsernameNormalized != normalized {
		return errors.New("username_normalized does not match username")
	}

	created, err := createUserScript.Run(
		ctx,
		s.rdb,
		[]string{usernameKey(normalized), userKey(user.ID)},
		user.ID,
		user.Username,
		normalized,
		user.PasswordPHC,
		user.PasswordVersion,
		formatRedisTime(user.CreatedAt),
	).Int64()
	if err != nil {
		return fmt.Errorf("create user: %w", err)
	}
	if created == 0 {
		return ErrUsernameExists
	}
	return nil
}

func (s *Store) UserByUsername(ctx context.Context, username string) (User, error) {
	normalized, err := NormalizeUsername(username)
	if err != nil {
		return User{}, fmt.Errorf("normalize username: %w", err)
	}
	id, err := s.rdb.Get(ctx, usernameKey(normalized)).Result()
	if errors.Is(err, goredis.Nil) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("resolve username: %w", err)
	}

	fields, err := s.rdb.HGetAll(ctx, userKey(id)).Result()
	if err != nil {
		return User{}, fmt.Errorf("get user: %w", err)
	}
	if len(fields) == 0 {
		return User{}, ErrNotFound
	}

	return s.userFromFields(ctx, id, fields)
}

func (s *Store) UserByID(ctx context.Context, id string) (User, error) {
	fields, err := s.rdb.HGetAll(ctx, userKey(id)).Result()
	if err != nil {
		return User{}, fmt.Errorf("get user: %w", err)
	}
	if len(fields) == 0 {
		return User{}, ErrNotFound
	}
	return s.userFromFields(ctx, id, fields)
}

func (s *Store) userFromFields(_ context.Context, id string, fields map[string]string) (User, error) {
	passwordVersion, err := parseRedisInt64(fields["password_version"], "password_version")
	if err != nil {
		return User{}, err
	}
	mustChangePassword, err := parseRedisBool(fields["must_change_password"], "must_change_password")
	if err != nil {
		return User{}, err
	}
	createdAt, err := parseRedisTime(fields["created_at"], "created_at")
	if err != nil {
		return User{}, err
	}
	userID := fields["id"]
	if userID == "" {
		userID = id
	}
	return User{
		ID:                 userID,
		Username:           fields["username"],
		UsernameNormalized: fields["username_normalized"],
		PasswordPHC:        fields["password_phc"],
		PasswordVersion:    passwordVersion,
		Status:             fields["status"],
		MustChangePassword: mustChangePassword,
		CreatedAt:          createdAt,
	}, nil
}

func (s *Store) SetUserStatus(ctx context.Context, userID, status string) error {
	if err := s.rdb.HSet(ctx, userKey(userID), "status", status).Err(); err != nil {
		return fmt.Errorf("set user status: %w", err)
	}
	return nil
}

func (s *Store) CreateTeam(ctx context.Context, team Team, inviteHash string) error {
	created, err := createTeamScript.Run(
		ctx,
		s.rdb,
		[]string{teamKey(team.ID), teamMembersKey(team.ID), userTeamsKey(team.OwnerUserID)},
		team.ID,
		team.Name,
		team.OwnerUserID,
		inviteHash,
		formatRedisTime(team.CreatedAt),
	).Int64()
	if err != nil {
		return fmt.Errorf("create team: %w", err)
	}
	if created == 0 {
		return ErrTeamExists
	}
	return nil
}

func (s *Store) Team(ctx context.Context, id string) (Team, error) {
	fields, err := s.rdb.HGetAll(ctx, teamKey(id)).Result()
	if err != nil {
		return Team{}, fmt.Errorf("get team: %w", err)
	}
	if len(fields) == 0 {
		return Team{}, ErrNotFound
	}
	createdAt, err := parseRedisTime(fields["created_at"], "created_at")
	if err != nil {
		return Team{}, err
	}
	return Team{
		ID:          fields["id"],
		Name:        fields["name"],
		OwnerUserID: fields["owner_user_id"],
		CreatedAt:   createdAt,
	}, nil
}

func (s *Store) Role(ctx context.Context, teamID, userID string) (Role, error) {
	role, err := s.rdb.HGet(ctx, teamMembersKey(teamID), userID).Result()
	if errors.Is(err, goredis.Nil) {
		return "", ErrNotMember
	}
	if err != nil {
		return "", fmt.Errorf("get team role: %w", err)
	}
	return Role(role), nil
}

func formatRedisTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func parseRedisTime(value, field string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse %s: %w", field, err)
	}
	return parsed, nil
}

func parseRedisInt64(value, field string) (int64, error) {
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", field, err)
	}
	return parsed, nil
}

func parseRedisBool(value, field string) (bool, error) {
	switch value {
	case "0":
		return false, nil
	case "1":
		return true, nil
	default:
		return false, fmt.Errorf("parse %s: expected 0 or 1, got %q", field, value)
	}
}
