package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/team/agentlink/pkg/redis"
)

func TestStoreCreateUserEnforcesNormalizedUniqueness(t *testing.T) {
	store, rdb := newAuthTestStore(t)
	username := uniqueTestUsername(t)
	normalized, err := NormalizeUsername(username)
	if err != nil {
		t.Fatal(err)
	}

	first := newTestUser(t, strings.ToUpper(username), 1)
	second := newTestUser(t, strings.ToLower(username), 1)
	first.UsernameNormalized = normalized
	second.UsernameNormalized = normalized
	cleanupAuthKeys(t, rdb,
		userKey(first.ID),
		userKey(second.ID),
		usernameKey(normalized),
		userTeamsKey(first.ID),
		userTeamsKey(second.ID),
	)

	start := make(chan struct{})
	results := make(chan error, 2)
	for _, user := range []User{first, second} {
		user := user
		go func() {
			<-start
			results <- store.CreateUser(context.Background(), user)
		}()
	}
	close(start)

	var successes, conflicts int
	for range 2 {
		switch err := <-results; {
		case err == nil:
			successes++
		case errors.Is(err, ErrUsernameExists):
			conflicts++
		default:
			t.Fatalf("CreateUser() error = %v; want nil or ErrUsernameExists", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("CreateUser() results = %d successes, %d conflicts; want 1 and 1", successes, conflicts)
	}

	stored, err := store.UserByUsername(context.Background(), strings.ToUpper(username))
	if err != nil {
		t.Fatal(err)
	}
	if stored.ID != first.ID && stored.ID != second.ID {
		t.Fatalf("UserByUsername() ID = %q; want one of the competing user IDs", stored.ID)
	}
	if stored.UsernameNormalized != normalized {
		t.Fatalf("UserByUsername() normalized name = %q; want %q", stored.UsernameNormalized, normalized)
	}
	if stored.Status != "active" || stored.MustChangePassword {
		t.Fatalf("UserByUsername() defaults = status %q, must-change %v; want active, false", stored.Status, stored.MustChangePassword)
	}
}

func TestStoreCreateTeamAddsOwnerMembershipAtomically(t *testing.T) {
	store, rdb := newAuthTestStore(t)
	owner := newTestUser(t, uniqueTestUsername(t), 3)
	team := newTestTeam(t, owner.ID)
	cleanupAuthKeys(t, rdb,
		userKey(owner.ID),
		usernameKey(owner.UsernameNormalized),
		userTeamsKey(owner.ID),
		teamKey(team.ID),
		teamMembersKey(team.ID),
		teamProjectsKey(team.ID),
	)

	if err := store.CreateUser(context.Background(), owner); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateTeam(context.Background(), team, "invite-hash"); err != nil {
		t.Fatal(err)
	}

	stored, err := store.Team(context.Background(), team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ID != team.ID || stored.Name != team.Name || stored.OwnerUserID != owner.ID || !stored.CreatedAt.Equal(team.CreatedAt) {
		t.Fatalf("Team() = %+v; want %+v", stored, team)
	}
	role, err := store.Role(context.Background(), team.ID, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if role != RoleOwner {
		t.Fatalf("Role() = %q; want %q", role, RoleOwner)
	}
	member, err := rdb.SIsMember(context.Background(), userTeamsKey(owner.ID), team.ID).Result()
	if err != nil {
		t.Fatal(err)
	}
	if !member {
		t.Fatal("owner reverse team index is missing")
	}
	fields, err := rdb.HMGet(context.Background(), teamKey(team.ID), "invite_hash", "invite_version").Result()
	if err != nil {
		t.Fatal(err)
	}
	if fields[0] != "invite-hash" || fields[1] != "1" {
		t.Fatalf("team invite fields = %#v; want invite-hash and version 1", fields)
	}
}

func TestStoreRoleReturnsErrNotMember(t *testing.T) {
	store, rdb := newAuthTestStore(t)
	owner := newTestUser(t, uniqueTestUsername(t), 1)
	nonMember := newTestUser(t, uniqueTestUsername(t), 1)
	team := newTestTeam(t, owner.ID)
	cleanupAuthKeys(t, rdb,
		userKey(owner.ID),
		usernameKey(owner.UsernameNormalized),
		userTeamsKey(owner.ID),
		userKey(nonMember.ID),
		usernameKey(nonMember.UsernameNormalized),
		userTeamsKey(nonMember.ID),
		teamKey(team.ID),
		teamMembersKey(team.ID),
	)

	if err := store.CreateUser(context.Background(), owner); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateUser(context.Background(), nonMember); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateTeam(context.Background(), team, "invite-hash"); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Role(context.Background(), team.ID, nonMember.ID); !errors.Is(err, ErrNotMember) {
		t.Fatalf("Role() error = %v; want ErrNotMember", err)
	}
}

func TestStoreCreateTeamWrongIndexTypesLeaveNoPartialWrites(t *testing.T) {
	tests := []struct {
		name    string
		corrupt func(t *testing.T, rdb *redis.Client, team Team)
		assert  func(t *testing.T, rdb *redis.Client, team Team)
	}{
		{
			name: "members key",
			corrupt: func(t *testing.T, rdb *redis.Client, team Team) {
				t.Helper()
				if err := rdb.Set(context.Background(), teamMembersKey(team.ID), "blocked-members", 0).Err(); err != nil {
					t.Fatal(err)
				}
			},
			assert: func(t *testing.T, rdb *redis.Client, team Team) {
				t.Helper()
				assertRedisKeyAbsent(t, rdb, teamKey(team.ID))
				value, err := rdb.Get(context.Background(), teamMembersKey(team.ID)).Result()
				if err != nil {
					t.Fatal(err)
				}
				if value != "blocked-members" {
					t.Fatalf("members key = %q; want original wrong-type value", value)
				}
				assertRedisKeyAbsent(t, rdb, userTeamsKey(team.OwnerUserID))
			},
		},
		{
			name: "owner userTeams key",
			corrupt: func(t *testing.T, rdb *redis.Client, team Team) {
				t.Helper()
				if err := rdb.Set(context.Background(), userTeamsKey(team.OwnerUserID), "blocked-user-teams", 0).Err(); err != nil {
					t.Fatal(err)
				}
			},
			assert: func(t *testing.T, rdb *redis.Client, team Team) {
				t.Helper()
				assertRedisKeyAbsent(t, rdb, teamKey(team.ID))
				assertRedisKeyAbsent(t, rdb, teamMembersKey(team.ID))
				value, err := rdb.Get(context.Background(), userTeamsKey(team.OwnerUserID)).Result()
				if err != nil {
					t.Fatal(err)
				}
				if value != "blocked-user-teams" {
					t.Fatalf("userTeams key = %q; want original wrong-type value", value)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, rdb := newAuthTestStore(t)
			owner := newTestUser(t, uniqueTestUsername(t), 1)
			team := newTestTeam(t, owner.ID)
			cleanupAuthKeys(t, rdb,
				userKey(owner.ID),
				usernameKey(owner.UsernameNormalized),
				userTeamsKey(owner.ID),
				teamKey(team.ID),
				teamMembersKey(team.ID),
			)
			if err := store.CreateUser(context.Background(), owner); err != nil {
				t.Fatal(err)
			}
			tt.corrupt(t, rdb, team)

			err := store.CreateTeam(context.Background(), team, "invite-hash")
			if err == nil || errors.Is(err, ErrTeamExists) {
				t.Fatalf("CreateTeam() error = %v; want Redis type error", err)
			}
			tt.assert(t, rdb, team)
		})
	}
}

func TestStoreCreateTeamExistingKeyStillReturnsErrTeamExists(t *testing.T) {
	store, rdb := newAuthTestStore(t)
	owner := newTestUser(t, uniqueTestUsername(t), 1)
	team := newTestTeam(t, owner.ID)
	cleanupAuthKeys(t, rdb,
		userKey(owner.ID),
		usernameKey(owner.UsernameNormalized),
		userTeamsKey(owner.ID),
		teamKey(team.ID),
		teamMembersKey(team.ID),
	)
	if err := store.CreateUser(context.Background(), owner); err != nil {
		t.Fatal(err)
	}
	if err := rdb.Set(context.Background(), teamKey(team.ID), "occupied", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.Set(context.Background(), teamMembersKey(team.ID), "wrong-members-type", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.Set(context.Background(), userTeamsKey(owner.ID), "wrong-user-teams-type", 0).Err(); err != nil {
		t.Fatal(err)
	}

	if err := store.CreateTeam(context.Background(), team, "invite-hash"); !errors.Is(err, ErrTeamExists) {
		t.Fatalf("CreateTeam() error = %v; want ErrTeamExists", err)
	}
	if value := rdb.Get(context.Background(), teamKey(team.ID)).Val(); value != "occupied" {
		t.Fatalf("team key = %q; want occupied", value)
	}
	if value := rdb.Get(context.Background(), teamMembersKey(team.ID)).Val(); value != "wrong-members-type" {
		t.Fatalf("members key = %q; want unchanged", value)
	}
	if value := rdb.Get(context.Background(), userTeamsKey(owner.ID)).Val(); value != "wrong-user-teams-type" {
		t.Fatalf("userTeams key = %q; want unchanged", value)
	}
}

func assertRedisKeyAbsent(t *testing.T, rdb *redis.Client, key string) {
	t.Helper()
	exists, err := rdb.Exists(context.Background(), key).Result()
	if err != nil {
		t.Fatal(err)
	}
	if exists != 0 {
		t.Fatalf("Redis key %q exists; want absent", key)
	}
}

func newAuthTestStore(t *testing.T) (*Store, *redis.Client) {
	t.Helper()
	rdb, err := redis.NewClient("localhost:6379")
	if err != nil {
		t.Fatalf("connect to test Redis: %v", err)
	}
	t.Cleanup(func() {
		if err := rdb.Close(); err != nil {
			t.Errorf("close test Redis: %v", err)
		}
	})
	return NewStore(rdb), rdb
}

func clearAuthKeys(t *testing.T, rdb *redis.Client, keys ...string) {
	t.Helper()
	if err := rdb.Del(context.Background(), keys...).Err(); err != nil {
		t.Fatalf("clear test Redis keys: %v", err)
	}
}

func cleanupAuthKeys(t *testing.T, rdb *redis.Client, keys ...string) {
	t.Helper()
	t.Cleanup(func() {
		if err := rdb.Del(context.Background(), keys...).Err(); err != nil {
			t.Errorf("delete test Redis keys: %v", err)
		}
	})
}

func uniqueTestUsername(t *testing.T) string {
	t.Helper()
	username, err := NewSecret("u", 6)
	if err != nil {
		t.Fatal(err)
	}
	return strings.ToLower(username)
}

func newTestUser(t *testing.T, username string, passwordVersion int64) User {
	t.Helper()
	id, err := NewUserID()
	if err != nil {
		t.Fatal(err)
	}
	normalized, err := NormalizeUsername(username)
	if err != nil {
		t.Fatal(err)
	}
	return User{
		ID:                 id,
		Username:           username,
		UsernameNormalized: normalized,
		PasswordPHC:        "$argon2id$test",
		PasswordVersion:    passwordVersion,
		Status:             "ignored-on-create",
		MustChangePassword: true,
		CreatedAt:          time.Now().UTC().Truncate(time.Microsecond),
	}
}

func newTestTeam(t *testing.T, ownerID string) Team {
	t.Helper()
	id, err := NewTeamID()
	if err != nil {
		t.Fatal(err)
	}
	return Team{
		ID:          id,
		Name:        "Test Team",
		OwnerUserID: ownerID,
		CreatedAt:   time.Now().UTC().Truncate(time.Microsecond),
	}
}
