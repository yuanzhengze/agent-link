package auth

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/team/agentlink/pkg/redis"
)

func newTeamTestService(t *testing.T, clock Clock, opts ...func(*Service)) (*Service, *Store, *redis.Client) {
	t.Helper()
	svc, store, rdb := newAuthService(t, clock)
	for _, opt := range opts {
		opt(svc)
	}
	return svc, store, rdb
}

func withTeamIDGenerator(fn func() (string, error)) func(*Service) {
	return func(s *Service) {
		s.teamIDGenerator = fn
	}
}

func withInviteGenerator(fn func() (string, error)) func(*Service) {
	return func(s *Service) {
		s.inviteGenerator = fn
	}
}

func teamActor(user User, teamID string, role Role) Actor {
	return Actor{UserID: user.ID, Username: user.Username, TeamID: teamID, Role: role}
}

func createTestTeam(t *testing.T, svc *Service, owner User, name string) CreateTeamResult {
	t.Helper()
	result, err := svc.CreateTeam(context.Background(), owner.ID, name)
	if err != nil {
		t.Fatalf("CreateTeam() error = %v", err)
	}
	return result
}

func joinTestTeam(t *testing.T, svc *Service, user User, teamID, invite string) Team {
	t.Helper()
	team, err := svc.JoinTeam(context.Background(), user.ID, teamID, invite)
	if err != nil {
		t.Fatalf("JoinTeam() error = %v", err)
	}
	return team
}

func TestInviteRawNotStoredInRedis(t *testing.T) {
	clock := redisNowClock()
	svc, _, rdb := newTeamTestService(t, clock)
	owner := registerServiceUser(t, svc, uniqueTestUsername(t), "correct horse battery staple")
	result := createTestTeam(t, svc, owner, "Alpha Squad")
	assertRedisStoresOnlyHash(t, rdb, result.InviteCode)
	if !strings.HasPrefix(result.InviteCode, "inv_") {
		t.Fatalf("InviteCode = %q; want inv_ prefix", result.InviteCode)
	}
}

func TestRotateInviteJoinRaceOldCodeRejected(t *testing.T) {
	clock := redisNowClock()
	svc, _, _ := newTeamTestService(t, clock)
	owner := registerServiceUser(t, svc, uniqueTestUsername(t), "correct horse battery staple")
	joiner := registerServiceUser(t, svc, uniqueTestUsername(t), "correct horse battery staple")
	result := createTestTeam(t, svc, owner, "Race Team")
	oldInvite := result.InviteCode

	start := make(chan struct{})
	var rotateErr, joinErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, rotateErr = svc.RotateInvite(context.Background(), teamActor(owner, result.Team.ID, RoleOwner))
	}()
	go func() {
		defer wg.Done()
		<-start
		_, joinErr = svc.JoinTeam(context.Background(), joiner.ID, result.Team.ID, oldInvite)
	}()
	close(start)
	wg.Wait()

	if rotateErr != nil {
		t.Fatalf("RotateInvite() error = %v", rotateErr)
	}
	if joinErr != nil && !errors.Is(joinErr, ErrInvalidInvite) {
		t.Fatalf("JoinTeam() with stale invite error = %v; want ErrInvalidInvite or success before rotate", joinErr)
	}
	if joinErr != nil {
		role, err := svc.store.Role(context.Background(), result.Team.ID, joiner.ID)
		if !errors.Is(err, ErrNotMember) {
			t.Fatalf("joiner role after failed race join = %v, %v; want ErrNotMember", role, err)
		}
	}
	lateJoiner := registerServiceUser(t, svc, uniqueTestUsername(t), "correct horse battery staple")
	_, err := svc.JoinTeam(context.Background(), lateJoiner.ID, result.Team.ID, oldInvite)
	if !errors.Is(err, ErrInvalidInvite) {
		t.Fatalf("JoinTeam() with post-rotate old invite error = %v; want ErrInvalidInvite", err)
	}
}

func TestJoinTeamAlreadyMember(t *testing.T) {
	clock := redisNowClock()
	svc, _, _ := newTeamTestService(t, clock)
	owner := registerServiceUser(t, svc, uniqueTestUsername(t), "correct horse battery staple")
	member := registerServiceUser(t, svc, uniqueTestUsername(t), "correct horse battery staple")
	result := createTestTeam(t, svc, owner, "Dup Join")
	joinTestTeam(t, svc, member, result.Team.ID, result.InviteCode)
	_, err := svc.JoinTeam(context.Background(), member.ID, result.Team.ID, result.InviteCode)
	if !errors.Is(err, ErrAlreadyMember) {
		t.Fatalf("JoinTeam() duplicate error = %v; want ErrAlreadyMember", err)
	}
}

func TestSpoofedActorRoleCannotEscalate(t *testing.T) {
	clock := redisNowClock()
	svc, _, _ := newTeamTestService(t, clock)
	owner := registerServiceUser(t, svc, uniqueTestUsername(t), "correct horse battery staple")
	member := registerServiceUser(t, svc, uniqueTestUsername(t), "correct horse battery staple")
	admin := registerServiceUser(t, svc, uniqueTestUsername(t), "correct horse battery staple")
	result := createTestTeam(t, svc, owner, "Spoof Team")
	joinTestTeam(t, svc, member, result.Team.ID, result.InviteCode)
	joinTestTeam(t, svc, admin, result.Team.ID, result.InviteCode)

	if err := svc.ChangeRole(context.Background(), teamActor(member, result.Team.ID, RoleOwner), admin.ID, RoleAdmin); !errors.Is(err, ErrForbidden) {
		t.Fatalf("member spoofed owner ChangeRole error = %v; want ErrForbidden", err)
	}
	if err := svc.RemoveMember(context.Background(), teamActor(member, result.Team.ID, RoleOwner), admin.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("member spoofed owner RemoveMember error = %v; want ErrForbidden", err)
	}
	if _, err := svc.RotateInvite(context.Background(), teamActor(member, result.Team.ID, RoleAdmin)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("member spoofed admin RotateInvite error = %v; want ErrForbidden", err)
	}
}

func TestCreateTeamIDCollisionRetries(t *testing.T) {
	clock := redisNowClock()
	collisionID, err := NewTeamID()
	if err != nil {
		t.Fatal(err)
	}
	successID, err := NewTeamID()
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	svc, store, rdb := newTeamTestService(t, clock, withTeamIDGenerator(func() (string, error) {
		n := calls.Add(1)
		if n <= 4 {
			return collisionID, nil
		}
		return successID, nil
	}))
	owner := registerServiceUser(t, svc, uniqueTestUsername(t), "correct horse battery staple")
	cleanupAuthKeys(t, rdb, teamKey(collisionID), teamMembersKey(collisionID), userTeamsKey(owner.ID))

	if err := store.CreateTeam(context.Background(), Team{
		ID: collisionID, Name: "occupied", OwnerUserID: owner.ID, CreatedAt: clock.Now(),
	}, "occupied-hash"); err != nil {
		t.Fatal(err)
	}

	result, err := svc.CreateTeam(context.Background(), owner.ID, "Retry Team")
	if err != nil {
		t.Fatalf("CreateTeam() after collisions error = %v", err)
	}
	if result.Team.ID != successID {
		t.Fatalf("CreateTeam() ID = %q; want %q after retries", result.Team.ID, successID)
	}
	if calls.Load() != 5 {
		t.Fatalf("team ID generator calls = %d; want 5", calls.Load())
	}
	role, err := store.Role(context.Background(), successID, owner.ID)
	if err != nil || role != RoleOwner {
		t.Fatalf("owner role on success team = %q, %v; want owner", role, err)
	}
}

func TestCreateTeamIDCollisionExhaustedNoPartialIndex(t *testing.T) {
	clock := redisNowClock()
	collisionID, err := NewTeamID()
	if err != nil {
		t.Fatal(err)
	}
	svc, store, rdb := newTeamTestService(t, clock, withTeamIDGenerator(func() (string, error) {
		return collisionID, nil
	}))
	owner := registerServiceUser(t, svc, uniqueTestUsername(t), "correct horse battery staple")
	cleanupAuthKeys(t, rdb, teamKey(collisionID), teamMembersKey(collisionID), userTeamsKey(owner.ID))

	if err := store.CreateTeam(context.Background(), Team{
		ID: collisionID, Name: "occupied", OwnerUserID: owner.ID, CreatedAt: clock.Now(),
	}, "occupied-hash"); err != nil {
		t.Fatal(err)
	}

	_, err = svc.CreateTeam(context.Background(), owner.ID, "Exhausted Team")
	if err == nil {
		t.Fatal("CreateTeam() error = nil; want failure after 5 collisions")
	}
	if !errors.Is(err, ErrTeamIDExhausted) {
		t.Fatalf("CreateTeam() error = %v; want ErrTeamIDExhausted", err)
	}
	if errors.Is(err, ErrTeamExists) {
		t.Fatal("exhaustion error must not wrap ErrTeamExists")
	}

	teams, err := rdb.SMembers(context.Background(), userTeamsKey(owner.ID)).Result()
	if err != nil {
		t.Fatal(err)
	}
	for _, teamID := range teams {
		if teamID == collisionID {
			continue
		}
		t.Fatalf("partial team index contains unexpected team %q", teamID)
	}
	memberCount, err := rdb.HLen(context.Background(), teamMembersKey(collisionID)).Result()
	if err != nil {
		t.Fatal(err)
	}
	if memberCount != 1 {
		t.Fatalf("occupied team member count = %d; want 1 owner only", memberCount)
	}
}

func TestTransferOwnerOldOwnerBecomesAdmin(t *testing.T) {
	clock := redisNowClock()
	svc, store, _ := newTeamTestService(t, clock)
	owner := registerServiceUser(t, svc, uniqueTestUsername(t), "correct horse battery staple")
	successor := registerServiceUser(t, svc, uniqueTestUsername(t), "correct horse battery staple")
	result := createTestTeam(t, svc, owner, "Transfer Team")
	joinTestTeam(t, svc, successor, result.Team.ID, result.InviteCode)

	if err := svc.TransferOwner(context.Background(), teamActor(owner, result.Team.ID, RoleOwner), successor.ID); err != nil {
		t.Fatal(err)
	}
	team, err := store.Team(context.Background(), result.Team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if team.OwnerUserID != successor.ID {
		t.Fatalf("OwnerUserID = %q; want %q", team.OwnerUserID, successor.ID)
	}
	oldRole, err := store.Role(context.Background(), result.Team.ID, owner.ID)
	if err != nil || oldRole != RoleAdmin {
		t.Fatalf("old owner role = %q, %v; want admin", oldRole, err)
	}
	newRole, err := store.Role(context.Background(), result.Team.ID, successor.ID)
	if err != nil || newRole != RoleOwner {
		t.Fatalf("successor role = %q, %v; want owner", newRole, err)
	}
}

func TestConcurrentTransferOwnerSingleOwner(t *testing.T) {
	clock := redisNowClock()
	svc, store, _ := newTeamTestService(t, clock)
	owner := registerServiceUser(t, svc, uniqueTestUsername(t), "correct horse battery staple")
	alpha := registerServiceUser(t, svc, uniqueTestUsername(t), "correct horse battery staple")
	beta := registerServiceUser(t, svc, uniqueTestUsername(t), "correct horse battery staple")
	result := createTestTeam(t, svc, owner, "Concurrent Transfer")
	joinTestTeam(t, svc, alpha, result.Team.ID, result.InviteCode)
	joinTestTeam(t, svc, beta, result.Team.ID, result.InviteCode)

	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]error, 2)
	targets := []User{alpha, beta}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			results[idx] = svc.TransferOwner(context.Background(), teamActor(owner, result.Team.ID, RoleOwner), targets[idx].ID)
		}(i)
	}
	close(start)
	wg.Wait()

	successIndex := -1
	var successes int
	for i, err := range results {
		if err == nil {
			successes++
			successIndex = i
			continue
		}
		if !errors.Is(err, ErrForbidden) {
			t.Fatalf("failed TransferOwner error = %v; want ErrForbidden", err)
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent TransferOwner successes = %d; want 1 (%v)", successes, results)
	}

	team, err := store.Team(context.Background(), result.Team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if team.OwnerUserID != targets[successIndex].ID {
		t.Fatalf("OwnerUserID = %q; want successful target %q", team.OwnerUserID, targets[successIndex].ID)
	}
	oldOwnerRole, err := store.Role(context.Background(), result.Team.ID, owner.ID)
	if err != nil || oldOwnerRole != RoleAdmin {
		t.Fatalf("old owner role = %q, %v; want admin", oldOwnerRole, err)
	}

	members, err := rdbHGetAllRoles(t, store, result.Team.ID)
	if err != nil {
		t.Fatal(err)
	}
	var ownerCount int
	for _, role := range members {
		if role == RoleOwner {
			ownerCount++
		}
	}
	if ownerCount != 1 {
		t.Fatalf("owner count = %d; want exactly 1", ownerCount)
	}
}

func TestMemberUserTeamsIndexJoinRemoveLeave(t *testing.T) {
	clock := redisNowClock()
	svc, store, rdb := newTeamTestService(t, clock)
	owner := registerServiceUser(t, svc, uniqueTestUsername(t), "correct horse battery staple")
	member := registerServiceUser(t, svc, uniqueTestUsername(t), "correct horse battery staple")
	admin := registerServiceUser(t, svc, uniqueTestUsername(t), "correct horse battery staple")
	result := createTestTeam(t, svc, owner, "Index Team")
	joinTestTeam(t, svc, member, result.Team.ID, result.InviteCode)
	joinTestTeam(t, svc, admin, result.Team.ID, result.InviteCode)
	if err := svc.ChangeRole(context.Background(), teamActor(owner, result.Team.ID, RoleOwner), admin.ID, RoleAdmin); err != nil {
		t.Fatal(err)
	}

	assertUserTeamIndex(t, rdb, member.ID, result.Team.ID, true)
	assertUserTeamIndex(t, rdb, admin.ID, result.Team.ID, true)
	assertMemberIndex(t, rdb, result.Team.ID, member.ID, true)
	assertMemberIndex(t, rdb, result.Team.ID, admin.ID, true)

	if err := svc.RemoveMember(context.Background(), teamActor(owner, result.Team.ID, RoleOwner), member.ID); err != nil {
		t.Fatal(err)
	}
	assertUserTeamIndex(t, rdb, member.ID, result.Team.ID, false)
	assertMemberIndex(t, rdb, result.Team.ID, member.ID, false)

	if err := svc.LeaveTeam(context.Background(), teamActor(admin, result.Team.ID, RoleAdmin)); err != nil {
		t.Fatal(err)
	}
	assertUserTeamIndex(t, rdb, admin.ID, result.Team.ID, false)
	assertMemberIndex(t, rdb, result.Team.ID, admin.ID, false)

	if _, err := store.Role(context.Background(), result.Team.ID, member.ID); !errors.Is(err, ErrNotMember) {
		t.Fatalf("removed member role error = %v; want ErrNotMember", err)
	}
	if _, err := store.Role(context.Background(), result.Team.ID, admin.ID); !errors.Is(err, ErrNotMember) {
		t.Fatalf("left admin role error = %v; want ErrNotMember", err)
	}
}

func TestChangeRoleInvalidRole(t *testing.T) {
	clock := redisNowClock()
	svc, _, _ := newTeamTestService(t, clock)
	owner := registerServiceUser(t, svc, uniqueTestUsername(t), "correct horse battery staple")
	member := registerServiceUser(t, svc, uniqueTestUsername(t), "correct horse battery staple")
	result := createTestTeam(t, svc, owner, "Invalid Role")
	joinTestTeam(t, svc, member, result.Team.ID, result.InviteCode)

	if err := svc.ChangeRole(context.Background(), teamActor(owner, result.Team.ID, RoleOwner), member.ID, RoleOwner); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("ChangeRole() to owner error = %v; want ErrInvalidRole", err)
	}
	if err := svc.ChangeRole(context.Background(), teamActor(owner, result.Team.ID, RoleOwner), member.ID, Role("superuser")); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("ChangeRole() unknown role error = %v; want ErrInvalidRole", err)
	}
}

func TestOwnerCannotSelfRemoveOrDemote(t *testing.T) {
	clock := redisNowClock()
	svc, _, _ := newTeamTestService(t, clock)
	owner := registerServiceUser(t, svc, uniqueTestUsername(t), "correct horse battery staple")
	result := createTestTeam(t, svc, owner, "Owner Guard")

	if err := svc.RemoveMember(context.Background(), teamActor(owner, result.Team.ID, RoleOwner), owner.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("owner self RemoveMember error = %v; want ErrForbidden", err)
	}
	if err := svc.ChangeRole(context.Background(), teamActor(owner, result.Team.ID, RoleOwner), owner.ID, RoleAdmin); !errors.Is(err, ErrForbidden) {
		t.Fatalf("owner self demote error = %v; want ErrForbidden", err)
	}
	if err := svc.ChangeRole(context.Background(), teamActor(owner, result.Team.ID, RoleOwner), owner.ID, RoleMember); !errors.Is(err, ErrForbidden) {
		t.Fatalf("owner self demote to member error = %v; want ErrForbidden", err)
	}
}

func TestMissingTargetMutationsReturnErrNotMemberWithoutChanges(t *testing.T) {
	clock := redisNowClock()
	svc, _, rdb := newTeamTestService(t, clock)
	owner := registerServiceUser(t, svc, uniqueTestUsername(t), "correct horse battery staple")
	target := registerServiceUser(t, svc, uniqueTestUsername(t), "correct horse battery staple")
	result := createTestTeam(t, svc, owner, "Missing Target")
	actor := teamActor(owner, result.Team.ID, RoleOwner)

	tests := []struct {
		name string
		run  func() error
	}{
		{
			name: "change role",
			run: func() error {
				return svc.ChangeRole(context.Background(), actor, target.ID, RoleAdmin)
			},
		},
		{
			name: "remove member",
			run: func() error {
				return svc.RemoveMember(context.Background(), actor, target.ID)
			},
		},
		{
			name: "transfer owner",
			run: func() error {
				return svc.TransferOwner(context.Background(), actor, target.ID)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := snapshotTeamMutationState(t, rdb, result.Team.ID, owner.ID, target.ID)
			if err := tt.run(); !errors.Is(err, ErrNotMember) {
				t.Fatalf("operation error = %v; want ErrNotMember", err)
			}
			after := snapshotTeamMutationState(t, rdb, result.Team.ID, owner.ID, target.ID)
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("team state changed:\nbefore = %#v\nafter  = %#v", before, after)
			}
		})
	}
}

func TestListMembersCleansNonOwnerOrphanAndContinues(t *testing.T) {
	clock := redisNowClock()
	svc, _, rdb := newTeamTestService(t, clock)
	owner := registerServiceUser(t, svc, uniqueTestUsername(t), "correct horse battery staple")
	orphan := registerServiceUser(t, svc, uniqueTestUsername(t), "correct horse battery staple")
	survivor := registerServiceUser(t, svc, uniqueTestUsername(t), "correct horse battery staple")
	result := createTestTeam(t, svc, owner, "Orphan Cleanup")
	joinTestTeam(t, svc, orphan, result.Team.ID, result.InviteCode)
	joinTestTeam(t, svc, survivor, result.Team.ID, result.InviteCode)

	if err := rdb.Del(context.Background(), userKey(orphan.ID)).Err(); err != nil {
		t.Fatal(err)
	}
	members, err := svc.ListMembers(context.Background(), teamActor(owner, result.Team.ID, RoleOwner))
	if err != nil {
		t.Fatalf("ListMembers() error = %v; want orphan skipped", err)
	}
	gotIDs := make([]string, 0, len(members))
	for _, member := range members {
		gotIDs = append(gotIDs, member.UserID)
	}
	sort.Strings(gotIDs)
	wantIDs := []string{owner.ID, survivor.ID}
	sort.Strings(wantIDs)
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("ListMembers() IDs = %v; want %v", gotIDs, wantIDs)
	}
	assertMemberIndex(t, rdb, result.Team.ID, orphan.ID, false)
	assertUserTeamIndex(t, rdb, orphan.ID, result.Team.ID, false)
}

func TestListMembersMissingOwnerReturnsConsistencyErrorWithoutCleanup(t *testing.T) {
	clock := redisNowClock()
	svc, _, rdb := newTeamTestService(t, clock)
	owner := registerServiceUser(t, svc, uniqueTestUsername(t), "correct horse battery staple")
	member := registerServiceUser(t, svc, uniqueTestUsername(t), "correct horse battery staple")
	result := createTestTeam(t, svc, owner, "Missing Owner")
	joinTestTeam(t, svc, member, result.Team.ID, result.InviteCode)

	if err := rdb.Del(context.Background(), userKey(owner.ID)).Err(); err != nil {
		t.Fatal(err)
	}
	_, err := svc.ListMembers(context.Background(), teamActor(member, result.Team.ID, RoleMember))
	if err == nil || !errors.Is(err, ErrStoreInconsistent) || !strings.Contains(err.Error(), "inconsistent") {
		t.Fatalf("ListMembers() error = %v; want explicit internal consistency error", err)
	}
	assertMemberIndex(t, rdb, result.Team.ID, owner.ID, true)
	assertUserTeamIndex(t, rdb, owner.ID, result.Team.ID, true)
}

func TestListMembersUserReadErrorDoesNotCleanup(t *testing.T) {
	clock := redisNowClock()
	svc, _, rdb := newTeamTestService(t, clock)
	owner := registerServiceUser(t, svc, uniqueTestUsername(t), "correct horse battery staple")
	member := registerServiceUser(t, svc, uniqueTestUsername(t), "correct horse battery staple")
	result := createTestTeam(t, svc, owner, "User Read Error")
	joinTestTeam(t, svc, member, result.Team.ID, result.InviteCode)

	if err := rdb.Del(context.Background(), userKey(member.ID)).Err(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.Set(context.Background(), userKey(member.ID), "wrong-type", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ListMembers(context.Background(), teamActor(owner, result.Team.ID, RoleOwner)); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("ListMembers() error = %v; want Redis read error", err)
	}
	assertMemberIndex(t, rdb, result.Team.ID, member.ID, true)
	assertUserTeamIndex(t, rdb, member.ID, result.Team.ID, true)
}

func TestJoinTeamRequiresExistingActiveUser(t *testing.T) {
	clock := redisNowClock()
	svc, store, rdb := newTeamTestService(t, clock)
	owner := registerServiceUser(t, svc, uniqueTestUsername(t), "correct horse battery staple")
	inactive := registerServiceUser(t, svc, uniqueTestUsername(t), "correct horse battery staple")
	result := createTestTeam(t, svc, owner, "Active Joiners")
	if err := store.SetUserStatus(context.Background(), inactive.ID, "inactive"); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.JoinTeam(context.Background(), inactive.ID, result.Team.ID, result.InviteCode); !errors.Is(err, ErrForbidden) {
		t.Fatalf("inactive JoinTeam() error = %v; want ErrForbidden", err)
	}
	assertMemberIndex(t, rdb, result.Team.ID, inactive.ID, false)
	assertUserTeamIndex(t, rdb, inactive.ID, result.Team.ID, false)

	missingID, err := NewUserID()
	if err != nil {
		t.Fatal(err)
	}
	cleanupAuthKeys(t, rdb, userTeamsKey(missingID))
	if _, err := svc.JoinTeam(context.Background(), missingID, result.Team.ID, result.InviteCode); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing-user JoinTeam() error = %v; want ErrNotFound", err)
	}
	assertMemberIndex(t, rdb, result.Team.ID, missingID, false)
	assertUserTeamIndex(t, rdb, missingID, result.Team.ID, false)
}

func TestJoinTeamCorruptRedisTypeReturnsInternalError(t *testing.T) {
	clock := redisNowClock()
	svc, _, rdb := newTeamTestService(t, clock)
	owner := registerServiceUser(t, svc, uniqueTestUsername(t), "correct horse battery staple")
	joiner := registerServiceUser(t, svc, uniqueTestUsername(t), "correct horse battery staple")
	result := createTestTeam(t, svc, owner, "Corrupt Join")

	if err := rdb.Del(context.Background(), teamKey(result.Team.ID)).Err(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.Set(context.Background(), teamKey(result.Team.ID), "wrong-type", 0).Err(); err != nil {
		t.Fatal(err)
	}
	_, err := svc.JoinTeam(context.Background(), joiner.ID, result.Team.ID, result.InviteCode)
	if err == nil || errors.Is(err, ErrInvalidInvite) || !errors.Is(err, ErrStoreInconsistent) {
		t.Fatalf("JoinTeam() error = %v; want internal storage error, not ErrInvalidInvite", err)
	}
	assertMemberIndex(t, rdb, result.Team.ID, joiner.ID, false)
	assertUserTeamIndex(t, rdb, joiner.ID, result.Team.ID, false)
}

func TestCreateTeamRetriesOrphanMembersHashWithoutMutation(t *testing.T) {
	clock := redisNowClock()
	orphanID, err := NewTeamID()
	if err != nil {
		t.Fatal(err)
	}
	successID, err := NewTeamID()
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	svc, store, rdb := newTeamTestService(t, clock, withTeamIDGenerator(func() (string, error) {
		if calls.Add(1) == 1 {
			return orphanID, nil
		}
		return successID, nil
	}))
	owner := registerServiceUser(t, svc, uniqueTestUsername(t), "correct horse battery staple")
	staleOwnerID, err := NewUserID()
	if err != nil {
		t.Fatal(err)
	}
	staleMemberID, err := NewUserID()
	if err != nil {
		t.Fatal(err)
	}
	orphanMembers := map[string]string{
		staleOwnerID:  string(RoleOwner),
		staleMemberID: string(RoleMember),
	}
	cleanupAuthKeys(t, rdb,
		teamKey(orphanID),
		teamMembersKey(orphanID),
		teamKey(successID),
		teamMembersKey(successID),
		userTeamsKey(owner.ID),
	)
	if err := rdb.HSet(context.Background(), teamMembersKey(orphanID), orphanMembers).Err(); err != nil {
		t.Fatal(err)
	}

	result, err := svc.CreateTeam(context.Background(), owner.ID, "Orphan Retry")
	if err != nil {
		t.Fatalf("CreateTeam() error = %v", err)
	}
	if result.Team.ID != successID || calls.Load() != 2 {
		t.Fatalf("CreateTeam() ID/calls = %q/%d; want %q/2", result.Team.ID, calls.Load(), successID)
	}
	gotOrphanMembers, err := rdb.HGetAll(context.Background(), teamMembersKey(orphanID)).Result()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotOrphanMembers, orphanMembers) {
		t.Fatalf("orphan members changed from %v to %v", orphanMembers, gotOrphanMembers)
	}
	assertRedisKeyAbsent(t, rdb, teamKey(orphanID))
	assertUserTeamIndex(t, rdb, owner.ID, orphanID, false)
	assertUserTeamIndex(t, rdb, owner.ID, successID, true)
	role, err := store.Role(context.Background(), successID, owner.ID)
	if err != nil || role != RoleOwner {
		t.Fatalf("new team owner role = %q, %v; want owner", role, err)
	}
}

func TestCreateTeamFiveOrphanMembersCollisionsLeaveNoNewIndex(t *testing.T) {
	clock := redisNowClock()
	orphanIDs := make([]string, createTeamMaxAttempts)
	orphanSnapshots := make(map[string]map[string]string, createTeamMaxAttempts)
	for i := range orphanIDs {
		id, err := NewTeamID()
		if err != nil {
			t.Fatal(err)
		}
		staleMemberID, err := NewUserID()
		if err != nil {
			t.Fatal(err)
		}
		orphanIDs[i] = id
		orphanSnapshots[id] = map[string]string{staleMemberID: string(RoleMember)}
	}
	var calls atomic.Int32
	svc, _, rdb := newTeamTestService(t, clock, withTeamIDGenerator(func() (string, error) {
		index := int(calls.Add(1)) - 1
		if index >= len(orphanIDs) {
			return orphanIDs[len(orphanIDs)-1], nil
		}
		return orphanIDs[index], nil
	}))
	owner := registerServiceUser(t, svc, uniqueTestUsername(t), "correct horse battery staple")
	keys := []string{userTeamsKey(owner.ID)}
	for _, id := range orphanIDs {
		keys = append(keys, teamKey(id), teamMembersKey(id))
		if err := rdb.HSet(context.Background(), teamMembersKey(id), orphanSnapshots[id]).Err(); err != nil {
			t.Fatal(err)
		}
	}
	cleanupAuthKeys(t, rdb, keys...)

	if _, err := svc.CreateTeam(context.Background(), owner.ID, "All Orphans"); !errors.Is(err, ErrTeamIDExhausted) {
		t.Fatalf("CreateTeam() error = %v; want ErrTeamIDExhausted", err)
	}
	if calls.Load() != createTeamMaxAttempts {
		t.Fatalf("team ID generator calls = %d; want %d", calls.Load(), createTeamMaxAttempts)
	}
	ownerTeams, err := rdb.SMembers(context.Background(), userTeamsKey(owner.ID)).Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(ownerTeams) != 0 {
		t.Fatalf("owner team index = %v; want no new entries", ownerTeams)
	}
	for _, id := range orphanIDs {
		assertRedisKeyAbsent(t, rdb, teamKey(id))
		got, err := rdb.HGetAll(context.Background(), teamMembersKey(id)).Result()
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, orphanSnapshots[id]) {
			t.Fatalf("orphan members for %s changed from %v to %v", id, orphanSnapshots[id], got)
		}
	}
}

func TestRemoveOrphanMemberPreservesRecoveredUser(t *testing.T) {
	clock := redisNowClock()
	svc, store, rdb := newTeamTestService(t, clock)
	owner := registerServiceUser(t, svc, uniqueTestUsername(t), "correct horse battery staple")
	member := registerServiceUser(t, svc, uniqueTestUsername(t), "correct horse battery staple")
	result := createTestTeam(t, svc, owner, "Recovered Orphan")
	joinTestTeam(t, svc, member, result.Team.ID, result.InviteCode)
	userFields, err := rdb.HGetAll(context.Background(), userKey(member.ID)).Result()
	if err != nil {
		t.Fatal(err)
	}
	if err := rdb.Del(context.Background(), userKey(member.ID)).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UserByID(context.Background(), member.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UserByID() after delete error = %v; want ErrNotFound", err)
	}
	if err := rdb.HSet(context.Background(), userKey(member.ID), userFields).Err(); err != nil {
		t.Fatal(err)
	}

	err = store.RemoveOrphanTeamMember(context.Background(), result.Team.ID, member.ID)
	if !errors.Is(err, ErrOrphanUserRestored) || !strings.Contains(err.Error(), "restored") {
		t.Errorf("RemoveOrphanTeamMember() error = %v; want dedicated restored-user result", err)
	}
	assertMemberIndex(t, rdb, result.Team.ID, member.ID, true)
	assertUserTeamIndex(t, rdb, member.ID, result.Team.ID, true)
}

func TestListMembersRequiresTeamHashBeforeMembership(t *testing.T) {
	clock := redisNowClock()
	svc, _, rdb := newTeamTestService(t, clock)
	owner := registerServiceUser(t, svc, uniqueTestUsername(t), "correct horse battery staple")
	member := registerServiceUser(t, svc, uniqueTestUsername(t), "correct horse battery staple")
	result := createTestTeam(t, svc, owner, "Missing Team Hash")
	joinTestTeam(t, svc, member, result.Team.ID, result.InviteCode)
	if err := rdb.Del(context.Background(), teamKey(result.Team.ID)).Err(); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.ListMembers(context.Background(), teamActor(owner, result.Team.ID, RoleOwner)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ListMembers() error = %v; want ErrNotFound", err)
	}
	assertMemberIndex(t, rdb, result.Team.ID, owner.ID, true)
	assertMemberIndex(t, rdb, result.Team.ID, member.ID, true)
	assertUserTeamIndex(t, rdb, owner.ID, result.Team.ID, true)
	assertUserTeamIndex(t, rdb, member.ID, result.Team.ID, true)
}

func TestStoreJoinTeamRechecksUserActiveBeforeWriting(t *testing.T) {
	clock := redisNowClock()
	svc, store, rdb := newTeamTestService(t, clock)
	owner := registerServiceUser(t, svc, uniqueTestUsername(t), "correct horse battery staple")
	result := createTestTeam(t, svc, owner, "Join Recheck")

	t.Run("inactive", func(t *testing.T) {
		user := registerServiceUser(t, svc, uniqueTestUsername(t), "correct horse battery staple")
		if err := store.SetUserStatus(context.Background(), user.ID, "inactive"); err != nil {
			t.Fatal(err)
		}
		err := store.JoinTeam(context.Background(), result.Team.ID, user.ID, SecretHash(result.InviteCode))
		if !errors.Is(err, ErrForbidden) {
			t.Errorf("JoinTeam() inactive error = %v; want ErrForbidden", err)
		}
		assertMemberIndex(t, rdb, result.Team.ID, user.ID, false)
		assertUserTeamIndex(t, rdb, user.ID, result.Team.ID, false)
	})

	t.Run("missing", func(t *testing.T) {
		user := registerServiceUser(t, svc, uniqueTestUsername(t), "correct horse battery staple")
		if err := rdb.Del(context.Background(), userKey(user.ID)).Err(); err != nil {
			t.Fatal(err)
		}
		err := store.JoinTeam(context.Background(), result.Team.ID, user.ID, SecretHash(result.InviteCode))
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("JoinTeam() missing user error = %v; want ErrNotFound", err)
		}
		assertMemberIndex(t, rdb, result.Team.ID, user.ID, false)
		assertUserTeamIndex(t, rdb, user.ID, result.Team.ID, false)
	})
}

func TestListMembersCleansOrphanAdmin(t *testing.T) {
	clock := redisNowClock()
	svc, _, rdb := newTeamTestService(t, clock)
	owner := registerServiceUser(t, svc, uniqueTestUsername(t), "correct horse battery staple")
	admin := registerServiceUser(t, svc, uniqueTestUsername(t), "correct horse battery staple")
	result := createTestTeam(t, svc, owner, "Orphan Admin")
	joinTestTeam(t, svc, admin, result.Team.ID, result.InviteCode)
	if err := svc.ChangeRole(context.Background(), teamActor(owner, result.Team.ID, RoleOwner), admin.ID, RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if err := rdb.Del(context.Background(), userKey(admin.ID)).Err(); err != nil {
		t.Fatal(err)
	}

	members, err := svc.ListMembers(context.Background(), teamActor(owner, result.Team.ID, RoleOwner))
	if err != nil {
		t.Fatalf("ListMembers() error = %v", err)
	}
	if len(members) != 1 || members[0].UserID != owner.ID {
		t.Fatalf("ListMembers() = %+v; want only owner", members)
	}
	assertMemberIndex(t, rdb, result.Team.ID, admin.ID, false)
	assertUserTeamIndex(t, rdb, admin.ID, result.Team.ID, false)
}

func TestRemoveOrphanMemberFailsClosedWhenOwnerIDPointsToTarget(t *testing.T) {
	clock := redisNowClock()
	svc, store, rdb := newTeamTestService(t, clock)
	owner := registerServiceUser(t, svc, uniqueTestUsername(t), "correct horse battery staple")
	member := registerServiceUser(t, svc, uniqueTestUsername(t), "correct horse battery staple")
	result := createTestTeam(t, svc, owner, "Owner ID Guard")
	joinTestTeam(t, svc, member, result.Team.ID, result.InviteCode)
	if err := rdb.HSet(context.Background(), teamKey(result.Team.ID), "owner_user_id", member.ID).Err(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.Del(context.Background(), userKey(member.ID)).Err(); err != nil {
		t.Fatal(err)
	}

	if err := store.RemoveOrphanTeamMember(context.Background(), result.Team.ID, member.ID); !errors.Is(err, ErrStoreInconsistent) {
		t.Fatalf("RemoveOrphanTeamMember() error = %v; want ErrStoreInconsistent", err)
	}
	role, err := store.Role(context.Background(), result.Team.ID, member.ID)
	if err != nil || role != RoleMember {
		t.Fatalf("target role = %q, %v; want preserved member", role, err)
	}
	assertUserTeamIndex(t, rdb, member.ID, result.Team.ID, true)
}

type teamMutationState struct {
	team        map[string]string
	members     map[string]string
	ownerTeams  []string
	targetTeams []string
}

func snapshotTeamMutationState(t *testing.T, rdb *redis.Client, teamID, ownerID, targetID string) teamMutationState {
	t.Helper()
	ctx := context.Background()
	team, err := rdb.HGetAll(ctx, teamKey(teamID)).Result()
	if err != nil {
		t.Fatal(err)
	}
	members, err := rdb.HGetAll(ctx, teamMembersKey(teamID)).Result()
	if err != nil {
		t.Fatal(err)
	}
	ownerTeams, err := rdb.SMembers(ctx, userTeamsKey(ownerID)).Result()
	if err != nil {
		t.Fatal(err)
	}
	targetTeams, err := rdb.SMembers(ctx, userTeamsKey(targetID)).Result()
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(ownerTeams)
	sort.Strings(targetTeams)
	return teamMutationState{
		team:        team,
		members:     members,
		ownerTeams:  ownerTeams,
		targetTeams: targetTeams,
	}
}

func assertUserTeamIndex(t *testing.T, rdb *redis.Client, userID, teamID string, want bool) {
	t.Helper()
	member, err := rdb.SIsMember(context.Background(), userTeamsKey(userID), teamID).Result()
	if err != nil {
		t.Fatal(err)
	}
	if member != want {
		t.Fatalf("user %s team index for %s = %v; want %v", userID, teamID, member, want)
	}
}

func assertMemberIndex(t *testing.T, rdb *redis.Client, teamID, userID string, want bool) {
	t.Helper()
	exists, err := rdb.HExists(context.Background(), teamMembersKey(teamID), userID).Result()
	if err != nil {
		t.Fatal(err)
	}
	if exists != want {
		t.Fatalf("team %s member index for %s = %v; want %v", teamID, userID, exists, want)
	}
}

func rdbHGetAllRoles(t *testing.T, store *Store, teamID string) (map[string]Role, error) {
	t.Helper()
	members, err := store.ListTeamMembers(context.Background(), teamID)
	if err != nil {
		return nil, err
	}
	roles := make(map[string]Role, len(members))
	for _, member := range members {
		roles[member.UserID] = member.Role
	}
	return roles, nil
}
