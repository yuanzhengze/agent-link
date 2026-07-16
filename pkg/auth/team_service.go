package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type CreateTeamResult struct {
	Team       Team
	InviteCode string
}

type Member struct {
	UserID   string
	Username string
	Role     Role
}

const createTeamMaxAttempts = 5

func validateTeamName(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	count := utf8.RuneCountInString(trimmed)
	if count < 1 || count > 64 {
		return "", errors.New("team name must be 1-64 characters after trimming")
	}
	for _, r := range trimmed {
		if unicode.IsControl(r) {
			return "", errors.New("team name cannot contain control characters")
		}
	}
	return trimmed, nil
}

func validateActor(actor Actor) error {
	if strings.TrimSpace(actor.UserID) == "" || strings.TrimSpace(actor.TeamID) == "" {
		return ErrForbidden
	}
	return nil
}

func (s *Service) requireActorRole(ctx context.Context, actor Actor, allowed ...Role) (Role, error) {
	if err := validateActor(actor); err != nil {
		return "", err
	}
	role, err := s.store.Role(ctx, actor.TeamID, actor.UserID)
	if err != nil {
		return "", err
	}
	for _, allowedRole := range allowed {
		if role == allowedRole {
			return role, nil
		}
	}
	return role, ErrForbidden
}

func (s *Service) CreateTeam(ctx context.Context, userID, name string) (CreateTeamResult, error) {
	trimmed, err := validateTeamName(name)
	if err != nil {
		return CreateTeamResult{}, err
	}
	user, err := s.store.UserByID(ctx, userID)
	if err != nil {
		return CreateTeamResult{}, err
	}
	if user.Status != "active" {
		return CreateTeamResult{}, ErrForbidden
	}

	inviteCode, err := s.inviteGenerator()
	if err != nil {
		return CreateTeamResult{}, err
	}
	inviteHash := SecretHash(inviteCode)

	for attempt := 0; attempt < createTeamMaxAttempts; attempt++ {
		teamID, err := s.teamIDGenerator()
		if err != nil {
			return CreateTeamResult{}, err
		}
		team := Team{
			ID:          teamID,
			Name:        trimmed,
			OwnerUserID: userID,
			CreatedAt:   s.clock.Now().UTC().Truncate(time.Microsecond),
		}
		err = s.store.CreateTeam(ctx, team, inviteHash)
		if err == nil {
			return CreateTeamResult{Team: team, InviteCode: inviteCode}, nil
		}
		if errors.Is(err, ErrTeamExists) {
			continue
		}
		return CreateTeamResult{}, err
	}
	return CreateTeamResult{}, ErrTeamIDExhausted
}

func (s *Service) JoinTeam(ctx context.Context, userID, teamID, inviteCode string) (Team, error) {
	user, err := s.store.UserByID(ctx, userID)
	if err != nil {
		return Team{}, err
	}
	if user.Status != "active" {
		return Team{}, ErrForbidden
	}

	inviteHash, err := s.store.TeamInviteHash(ctx, teamID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Team{}, ErrInvalidInvite
		}
		return Team{}, err
	}
	providedHash := SecretHash(inviteCode)
	if subtle.ConstantTimeCompare([]byte(inviteHash), []byte(providedHash)) != 1 {
		return Team{}, ErrInvalidInvite
	}
	if err := s.store.JoinTeam(ctx, teamID, userID, inviteHash); err != nil {
		return Team{}, err
	}
	return s.store.Team(ctx, teamID)
}

func (s *Service) ListTeams(ctx context.Context, userID string) ([]Team, error) {
	teamIDs, err := s.store.ListUserTeamIDs(ctx, userID)
	if err != nil {
		return nil, err
	}
	teams := make([]Team, 0, len(teamIDs))
	for _, teamID := range teamIDs {
		team, err := s.store.Team(ctx, teamID)
		if errors.Is(err, ErrNotFound) {
			if removeErr := s.store.RemoveUserTeamIndex(ctx, userID, teamID); removeErr != nil {
				return nil, removeErr
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		teams = append(teams, team)
	}
	sort.Slice(teams, func(i, j int) bool {
		if teams[i].CreatedAt.Equal(teams[j].CreatedAt) {
			return teams[i].ID < teams[j].ID
		}
		return teams[i].CreatedAt.Before(teams[j].CreatedAt)
	})
	return teams, nil
}

func (s *Service) RotateInvite(ctx context.Context, actor Actor) (string, error) {
	if _, err := s.requireActorRole(ctx, actor, RoleOwner, RoleAdmin); err != nil {
		return "", err
	}
	inviteCode, err := s.inviteGenerator()
	if err != nil {
		return "", err
	}
	if err := s.store.RotateInvite(ctx, actor.TeamID, actor.UserID, SecretHash(inviteCode)); err != nil {
		return "", err
	}
	return inviteCode, nil
}

func (s *Service) ListMembers(ctx context.Context, actor Actor) ([]Member, error) {
	if err := validateActor(actor); err != nil {
		return nil, err
	}
	if _, err := s.store.Team(ctx, actor.TeamID); err != nil {
		return nil, err
	}
	if _, err := s.store.Role(ctx, actor.TeamID, actor.UserID); err != nil {
		return nil, err
	}
	memberships, err := s.store.ListTeamMembers(ctx, actor.TeamID)
	if err != nil {
		return nil, err
	}
	members := make([]Member, 0, len(memberships))
	for _, membership := range memberships {
		user, err := s.store.UserByID(ctx, membership.UserID)
		if errors.Is(err, ErrNotFound) {
			switch membership.Role {
			case RoleOwner:
				return nil, fmt.Errorf(
					"list team members: owner user %q is missing: %w",
					membership.UserID,
					ErrStoreInconsistent,
				)
			case RoleAdmin, RoleMember:
				pruneErr := s.store.RemoveOrphanTeamMember(ctx, actor.TeamID, membership.UserID)
				if pruneErr == nil {
					continue
				}
				if !errors.Is(pruneErr, ErrOrphanUserRestored) {
					return nil, pruneErr
				}
				user, err = s.store.UserByID(ctx, membership.UserID)
				if err != nil {
					return nil, fmt.Errorf(
						"list team members: restored user %q cannot be loaded (%v): %w",
						membership.UserID,
						err,
						ErrStoreInconsistent,
					)
				}
			default:
				return nil, fmt.Errorf(
					"list team members: user %q has invalid role %q: %w",
					membership.UserID,
					membership.Role,
					ErrStoreInconsistent,
				)
			}
		}
		if err != nil {
			return nil, err
		}
		members = append(members, Member{
			UserID:   membership.UserID,
			Username: user.Username,
			Role:     membership.Role,
		})
	}
	sort.Slice(members, func(i, j int) bool {
		return members[i].UserID < members[j].UserID
	})
	return members, nil
}

func (s *Service) ChangeRole(ctx context.Context, actor Actor, targetUserID string, role Role) error {
	if role != RoleAdmin && role != RoleMember {
		return ErrInvalidRole
	}
	if _, err := s.requireActorRole(ctx, actor, RoleOwner); err != nil {
		return err
	}
	if actor.UserID == targetUserID {
		return ErrForbidden
	}
	return s.store.ChangeMemberRole(ctx, actor.TeamID, actor.UserID, targetUserID, role)
}

func (s *Service) RemoveMember(ctx context.Context, actor Actor, targetUserID string) error {
	if err := validateActor(actor); err != nil {
		return err
	}
	if actor.UserID == targetUserID {
		return ErrForbidden
	}
	actorRole, err := s.store.Role(ctx, actor.TeamID, actor.UserID)
	if err != nil {
		return err
	}
	switch actorRole {
	case RoleOwner, RoleAdmin:
	default:
		return ErrForbidden
	}
	return s.store.RemoveTeamMember(ctx, actor.TeamID, actor.UserID, targetUserID)
}

func (s *Service) TransferOwner(ctx context.Context, actor Actor, targetUserID string) error {
	if _, err := s.requireActorRole(ctx, actor, RoleOwner); err != nil {
		return err
	}
	if actor.UserID == targetUserID {
		return ErrForbidden
	}
	return s.store.TransferTeamOwner(ctx, actor.TeamID, actor.UserID, targetUserID)
}

func (s *Service) LeaveTeam(ctx context.Context, actor Actor) error {
	if err := validateActor(actor); err != nil {
		return err
	}
	role, err := s.store.Role(ctx, actor.TeamID, actor.UserID)
	if err != nil {
		return err
	}
	if role == RoleOwner {
		return ErrForbidden
	}
	return s.store.LeaveTeam(ctx, actor.TeamID, actor.UserID)
}
