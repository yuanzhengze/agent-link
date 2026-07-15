package auth

import (
	"context"
	"errors"
	"fmt"
	"sort"

	goredis "github.com/redis/go-redis/v9"
)

type TeamMembership struct {
	UserID string
	Role   Role
}

var joinTeamScript = goredis.NewScript(`
if redis.call('TYPE', KEYS[1]).ok ~= 'hash' then return -2 end
if redis.call('TYPE', KEYS[2]).ok ~= 'hash' then return -4 end
local user_teams_type = redis.call('TYPE', KEYS[3]).ok
if user_teams_type ~= 'set' and user_teams_type ~= 'none' then return -4 end
local invite_hash = redis.call('HGET', KEYS[1], 'invite_hash')
if invite_hash == false or invite_hash ~= ARGV[2] then return -1 end
if redis.call('HEXISTS', KEYS[2], ARGV[1]) == 1 then return 0 end
redis.call('HSET', KEYS[2], ARGV[1], ARGV[3])
redis.call('SADD', KEYS[3], ARGV[4])
return 1
`)

var rotateInviteScript = goredis.NewScript(`
if redis.call('TYPE', KEYS[1]).ok ~= 'hash' then return -2 end
if redis.call('TYPE', KEYS[2]).ok ~= 'hash' then return -4 end
local actor_role = redis.call('HGET', KEYS[2], ARGV[1])
if actor_role == false then return -3 end
if actor_role ~= 'owner' and actor_role ~= 'admin' then return -3 end
redis.call('HSET', KEYS[1], 'invite_hash', ARGV[2])
redis.call('HINCRBY', KEYS[1], 'invite_version', 1)
return 1
`)

var changeRoleScript = goredis.NewScript(`
if redis.call('TYPE', KEYS[1]).ok ~= 'hash' then return -2 end
if redis.call('TYPE', KEYS[2]).ok ~= 'hash' then return -4 end
local actor_role = redis.call('HGET', KEYS[2], ARGV[1])
if actor_role ~= 'owner' then return -3 end
if ARGV[1] == ARGV[2] then return -3 end
if redis.call('HEXISTS', KEYS[2], ARGV[2]) == 0 then return -5 end
if ARGV[3] ~= 'admin' and ARGV[3] ~= 'member' then return -6 end
redis.call('HSET', KEYS[2], ARGV[2], ARGV[3])
return 1
`)

var removeMemberScript = goredis.NewScript(`
if redis.call('TYPE', KEYS[1]).ok ~= 'hash' then return -2 end
if redis.call('TYPE', KEYS[2]).ok ~= 'hash' then return -4 end
local user_teams_type = redis.call('TYPE', KEYS[3]).ok
if user_teams_type ~= 'set' and user_teams_type ~= 'none' then return -4 end
local actor_role = redis.call('HGET', KEYS[2], ARGV[1])
if actor_role == false then return -3 end
if ARGV[1] == ARGV[2] then return -3 end
local target_role = redis.call('HGET', KEYS[2], ARGV[2])
if target_role == false then return -5 end
if actor_role == 'admin' then
  if target_role ~= 'member' then return -3 end
elseif actor_role == 'owner' then
  if target_role == 'owner' then return -3 end
else
  return -3
end
redis.call('HDEL', KEYS[2], ARGV[2])
redis.call('SREM', KEYS[3], ARGV[3])
return 1
`)

var transferOwnerScript = goredis.NewScript(`
if redis.call('TYPE', KEYS[1]).ok ~= 'hash' then return -2 end
if redis.call('TYPE', KEYS[2]).ok ~= 'hash' then return -4 end
local actor_role = redis.call('HGET', KEYS[2], ARGV[1])
if actor_role ~= 'owner' then return -3 end
if redis.call('HEXISTS', KEYS[2], ARGV[2]) == 0 then return -5 end
if redis.call('HGET', KEYS[2], ARGV[2]) == 'owner' then return -3 end
redis.call('HSET', KEYS[2], ARGV[1], 'admin')
redis.call('HSET', KEYS[2], ARGV[2], 'owner')
redis.call('HSET', KEYS[1], 'owner_user_id', ARGV[2])
return 1
`)

var leaveTeamScript = goredis.NewScript(`
if redis.call('TYPE', KEYS[1]).ok ~= 'hash' then return -2 end
if redis.call('TYPE', KEYS[2]).ok ~= 'hash' then return -4 end
local user_teams_type = redis.call('TYPE', KEYS[3]).ok
if user_teams_type ~= 'set' and user_teams_type ~= 'none' then return -4 end
local role = redis.call('HGET', KEYS[2], ARGV[1])
if role == false then return -3 end
if role == 'owner' then return -3 end
redis.call('HDEL', KEYS[2], ARGV[1])
redis.call('SREM', KEYS[3], ARGV[2])
return 1
`)

func (s *Store) TeamInviteHash(ctx context.Context, teamID string) (string, error) {
	keyType, err := s.rdb.Type(ctx, teamKey(teamID)).Result()
	if err != nil {
		return "", fmt.Errorf("type team: %w", err)
	}
	if keyType != "hash" {
		return "", ErrNotFound
	}
	hash, err := s.rdb.HGet(ctx, teamKey(teamID), "invite_hash").Result()
	if errors.Is(err, goredis.Nil) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("get team invite hash: %w", err)
	}
	if hash == "" {
		return "", ErrNotFound
	}
	return hash, nil
}

func (s *Store) JoinTeam(ctx context.Context, teamID, userID, inviteHash string) error {
	result, err := joinTeamScript.Run(
		ctx,
		s.rdb,
		[]string{teamKey(teamID), teamMembersKey(teamID), userTeamsKey(userID)},
		userID,
		inviteHash,
		string(RoleMember),
		teamID,
	).Int64()
	if err != nil {
		return fmt.Errorf("join team: %w", err)
	}
	switch result {
	case 1:
		return nil
	case 0:
		return ErrAlreadyMember
	case -1:
		return ErrInvalidInvite
	case -2:
		return ErrInvalidInvite
	case -4:
		return fmt.Errorf("join team: corrupt team storage")
	default:
		return fmt.Errorf("join team: unexpected result %d", result)
	}
}

func (s *Store) RotateInvite(ctx context.Context, teamID, actorUserID, newInviteHash string) error {
	result, err := rotateInviteScript.Run(
		ctx,
		s.rdb,
		[]string{teamKey(teamID), teamMembersKey(teamID)},
		actorUserID,
		newInviteHash,
	).Int64()
	if err != nil {
		return fmt.Errorf("rotate invite: %w", err)
	}
	switch result {
	case 1:
		return nil
	case -2:
		return ErrNotFound
	case -3:
		return ErrForbidden
	case -4:
		return fmt.Errorf("rotate invite: corrupt team storage")
	default:
		return fmt.Errorf("rotate invite: unexpected result %d", result)
	}
}

func (s *Store) ChangeMemberRole(ctx context.Context, teamID, actorUserID, targetUserID string, role Role) error {
	result, err := changeRoleScript.Run(
		ctx,
		s.rdb,
		[]string{teamKey(teamID), teamMembersKey(teamID)},
		actorUserID,
		targetUserID,
		string(role),
	).Int64()
	if err != nil {
		return fmt.Errorf("change member role: %w", err)
	}
	switch result {
	case 1:
		return nil
	case -2:
		return ErrNotFound
	case -3:
		return ErrForbidden
	case -5:
		return ErrNotMember
	case -6:
		return ErrInvalidRole
	case -4:
		return fmt.Errorf("change member role: corrupt team storage")
	default:
		return fmt.Errorf("change member role: unexpected result %d", result)
	}
}

func (s *Store) RemoveTeamMember(ctx context.Context, teamID, actorUserID, targetUserID string) error {
	result, err := removeMemberScript.Run(
		ctx,
		s.rdb,
		[]string{teamKey(teamID), teamMembersKey(teamID), userTeamsKey(targetUserID)},
		actorUserID,
		targetUserID,
		teamID,
	).Int64()
	if err != nil {
		return fmt.Errorf("remove team member: %w", err)
	}
	switch result {
	case 1:
		return nil
	case -2:
		return ErrNotFound
	case -3:
		return ErrForbidden
	case -5:
		return ErrNotMember
	case -4:
		return fmt.Errorf("remove team member: corrupt team storage")
	default:
		return fmt.Errorf("remove team member: unexpected result %d", result)
	}
}

func (s *Store) TransferTeamOwner(ctx context.Context, teamID, actorUserID, targetUserID string) error {
	result, err := transferOwnerScript.Run(
		ctx,
		s.rdb,
		[]string{teamKey(teamID), teamMembersKey(teamID)},
		actorUserID,
		targetUserID,
	).Int64()
	if err != nil {
		return fmt.Errorf("transfer team owner: %w", err)
	}
	switch result {
	case 1:
		return nil
	case -2:
		return ErrNotFound
	case -3:
		return ErrForbidden
	case -5:
		return ErrNotMember
	case -4:
		return fmt.Errorf("transfer team owner: corrupt team storage")
	default:
		return fmt.Errorf("transfer team owner: unexpected result %d", result)
	}
}

func (s *Store) LeaveTeam(ctx context.Context, teamID, userID string) error {
	result, err := leaveTeamScript.Run(
		ctx,
		s.rdb,
		[]string{teamKey(teamID), teamMembersKey(teamID), userTeamsKey(userID)},
		userID,
		teamID,
	).Int64()
	if err != nil {
		return fmt.Errorf("leave team: %w", err)
	}
	switch result {
	case 1:
		return nil
	case -2:
		return ErrNotFound
	case -3:
		return ErrForbidden
	case -4:
		return fmt.Errorf("leave team: corrupt team storage")
	default:
		return fmt.Errorf("leave team: unexpected result %d", result)
	}
}

func (s *Store) ListUserTeamIDs(ctx context.Context, userID string) ([]string, error) {
	keyType, err := s.rdb.Type(ctx, userTeamsKey(userID)).Result()
	if err != nil {
		return nil, fmt.Errorf("type user teams: %w", err)
	}
	if keyType == "none" {
		return nil, nil
	}
	if keyType != "set" {
		return nil, fmt.Errorf("user teams index has wrong type %q", keyType)
	}
	teamIDs, err := s.rdb.SMembers(ctx, userTeamsKey(userID)).Result()
	if err != nil {
		return nil, fmt.Errorf("list user teams: %w", err)
	}
	sort.Strings(teamIDs)
	return teamIDs, nil
}

func (s *Store) RemoveUserTeamIndex(ctx context.Context, userID, teamID string) error {
	if err := s.rdb.SRem(ctx, userTeamsKey(userID), teamID).Err(); err != nil {
		return fmt.Errorf("remove user team index: %w", err)
	}
	return nil
}

func (s *Store) ListTeamMembers(ctx context.Context, teamID string) ([]TeamMembership, error) {
	keyType, err := s.rdb.Type(ctx, teamMembersKey(teamID)).Result()
	if err != nil {
		return nil, fmt.Errorf("type team members: %w", err)
	}
	if keyType == "none" {
		return nil, ErrNotFound
	}
	if keyType != "hash" {
		return nil, fmt.Errorf("team members index has wrong type %q", keyType)
	}
	fields, err := s.rdb.HGetAll(ctx, teamMembersKey(teamID)).Result()
	if err != nil {
		return nil, fmt.Errorf("list team members: %w", err)
	}
	members := make([]TeamMembership, 0, len(fields))
	for userID, role := range fields {
		members = append(members, TeamMembership{UserID: userID, Role: Role(role)})
	}
	sort.Slice(members, func(i, j int) bool {
		return members[i].UserID < members[j].UserID
	})
	return members, nil
}
