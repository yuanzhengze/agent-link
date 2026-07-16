package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	goredis "github.com/redis/go-redis/v9"
)

const (
	projectV2CreateAttempts = 5
	projectV2VerifyTimeout  = 2 * time.Second
)

var createTeamProjectV2Script = goredis.NewScript(`
local project_type = redis.call('TYPE', KEYS[1]).ok
local project_exists = redis.call('EXISTS', KEYS[1])
local index_type = redis.call('TYPE', KEYS[2]).ok
local index_exists = redis.call('EXISTS', KEYS[2])

if project_exists == 1 and project_type ~= 'hash' then
  return redis.error_reply('ERR project key must be none or hash')
end
if project_exists == 0 and project_type ~= 'none' then
  return redis.error_reply('ERR project key has inconsistent existence')
end
if index_exists == 1 and index_type ~= 'set' then
  return redis.error_reply('ERR team projects key must be none or set')
end
if index_exists == 0 and index_type ~= 'none' then
  return redis.error_reply('ERR team projects key has inconsistent existence')
end
if project_exists == 1 then
  local existing = redis.call('HMGET', KEYS[1],
    'id', 'team_id', 'name', 'created_at', 'creation_token')
  if existing[5] == ARGV[6] then
    -- Same creation token: this is our own attempt being replayed (e.g. a
    -- lost reply retried by the client). head_commit is mutable and may have
    -- already advanced via apply, so it is deliberately excluded from the
    -- identity check and never overwritten here.
    if existing[1] == ARGV[1]
      and existing[2] == ARGV[2]
      and existing[3] == ARGV[3]
      and existing[4] == ARGV[4] then
      redis.call('SADD', KEYS[2], ARGV[1])
      return 2
    end
    -- Same token but a different immutable identity means the stored record is
    -- corrupt. Never mutate it and never treat it as a reusable id collision.
    return 3
  end
  -- A different token means an independent project already owns this id.
  return 0
end

redis.call('HSET', KEYS[1],
  'id', ARGV[1],
  'team_id', ARGV[2],
  'name', ARGV[3],
  'created_at', ARGV[4],
  'head_commit', ARGV[5],
  'creation_token', ARGV[6])
redis.call('SADD', KEYS[2], ARGV[1])
return 1
`)

var verifyTeamProjectV2Script = goredis.NewScript(`
local project_type = redis.call('TYPE', KEYS[1]).ok
local index_type = redis.call('TYPE', KEYS[2]).ok

if project_type == 'none' then
  return 0
end
if project_type ~= 'hash' then
  return 4
end

local existing = redis.call('HMGET', KEYS[1],
  'id', 'team_id', 'name', 'created_at', 'head_commit', 'creation_token')
local existing_token = existing[6]
if not existing_token
  or string.len(existing_token) ~= 32
  or not string.match(existing_token, '^[0-9a-fA-F]+$') then
  return 4
end
if existing_token ~= ARGV[6] then
  return 2
end
if existing[1] ~= ARGV[1]
  or existing[2] ~= ARGV[2]
  or existing[3] ~= ARGV[3]
  or existing[4] ~= ARGV[4] then
  return 4
end
local head = existing[5]
if not head
  or not (string.len(head) == 40 or string.len(head) == 64)
  or not string.match(head, '^[0-9a-f]+$') then
  return 4
end
if index_type ~= 'set' then
  return 3
end
if redis.call('SISMEMBER', KEYS[2], ARGV[1]) ~= 1 then
  return 3
end
return 1
`)

type createProjectV2Request struct {
	Name string `json:"name"`
}

type listProjectsV2Response struct {
	Projects []TeamProject `json:"projects"`
}

type projectPersistenceVerification int

const (
	projectPersistenceUnknown projectPersistenceVerification = iota
	projectPersistenceAbsent
	projectPersistenceMatches
	projectPersistenceConflicts
	projectPersistenceBrokenIndex
	projectPersistenceCorrupt
)

type projectPersistenceSettlement int

const (
	projectPersistenceUnresolved projectPersistenceSettlement = iota
	projectPersistenceSettled
	projectPersistenceCollision
)

// projectPersistOutcome is the deterministic result of running the create
// script. Any transport/Redis error is reported separately and treated as
// uncertain, because a lost reply may hide an already-applied internal retry.
type projectPersistOutcome int

const (
	projectPersistUncertain projectPersistOutcome = iota
	projectPersistCreated
	projectPersistReplayed
	projectPersistCollided
	projectPersistCorrupt
)

func (s *Server) handleCreateProjectV2(w http.ResponseWriter, r *http.Request) {
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req createProjectV2Request
	if !decodeAuthJSON(w, r, &req) {
		return
	}
	name, err := validateProjectNameV2(req.Name)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	project, err := s.createTeamProjectV2(r.Context(), actor.TeamID, name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, project)
}

func (s *Server) handleListProjectsV2(w http.ResponseWriter, r *http.Request) {
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	projects, err := s.listTeamProjectsV2(r.Context(), actor.TeamID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, listProjectsV2Response{Projects: projects})
}

type TreeFileEntryV2 struct {
	Path   string       `json:"path"`
	Locked bool         `json:"locked"`
	Owner  *LockOwnerV2 `json:"owner,omitempty"`
}

type TreeResponseV2 struct {
	Files []TreeFileEntryV2 `json:"files"`
}

type SnapshotResponseV2 struct {
	HeadCommit string              `json:"head_commit"`
	Files      []SnapshotFileEntry `json:"files"`
}

// handleTreeV2 lists every file in a team project's work tree along with its
// structured lock status, for the GUI's file tree view.
func (s *Server) handleTreeV2(w http.ResponseWriter, r *http.Request) {
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	projectID := r.PathValue("project_id")
	if _, err := s.loadTeamProject(r.Context(), actor.TeamID, projectID); err != nil {
		if errors.Is(err, errProjectNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	paths, err := listProjectFiles(s.projectDirV2(actor.TeamID, projectID))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to walk project directory")
		return
	}

	files := make([]TreeFileEntryV2, 0, len(paths))
	for _, rel := range paths {
		owner, _, active := s.lockStatusV2(r, actor.TeamID, projectID, rel)
		entry := TreeFileEntryV2{Path: rel, Locked: active}
		if active {
			ownerCopy := owner
			entry.Owner = &ownerCopy
		}
		files = append(files, entry)
	}

	writeJSON(w, http.StatusOK, TreeResponseV2{Files: files})
}

// handleSnapshotV2 returns the full content of every file in a team project's
// work tree plus the current head_commit. It holds the same per-project mutex
// as apply, spanning both the head_commit read and every file read, so the
// returned bytes always correspond to the returned head_commit.
func (s *Server) handleSnapshotV2(w http.ResponseWriter, r *http.Request) {
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	projectID := r.PathValue("project_id")
	if _, err := s.loadTeamProject(r.Context(), actor.TeamID, projectID); err != nil {
		if errors.Is(err, errProjectNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	mu := s.projectLock(teamProjectMutexKey(actor.TeamID, projectID))
	mu.Lock()
	defer mu.Unlock()

	headCommit, err := s.rdb.HGet(r.Context(), projectV2Key(projectID), "head_commit").Result()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	dir := s.projectDirV2(actor.TeamID, projectID)
	paths, err := listProjectFiles(dir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to walk project directory")
		return
	}

	files := make([]SnapshotFileEntry, 0, len(paths))
	for _, rel := range paths {
		content, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to read file")
			return
		}
		files = append(files, SnapshotFileEntry{Path: rel, Content: string(content)})
	}

	writeJSON(w, http.StatusOK, SnapshotResponseV2{HeadCommit: headCommit, Files: files})
}

func validateProjectNameV2(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	count := utf8.RuneCountInString(name)
	if count < 1 || count > 64 {
		return "", errors.New("project name must contain 1 to 64 codepoints")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", errors.New("project name must not contain control characters")
		}
	}
	return name, nil
}

func (s *Server) createTeamProjectV2(ctx context.Context, teamID, name string) (TeamProject, error) {
	if !validProjectPathComponentV2(teamID) {
		return TeamProject{}, errors.New("invalid team path")
	}
	idGenerator := s.projectIDGenerator
	if idGenerator == nil {
		idGenerator = generateID
	}
	tokenGenerator := s.projectTokenGenerator
	if tokenGenerator == nil {
		tokenGenerator = generateProjectCreationTokenV2
	}
	initializer := s.projectGitInitializer
	if initializer == nil {
		initializer = initializeProjectGitV2
	}

	for attempt := 0; attempt < projectV2CreateAttempts; attempt++ {
		projectID := idGenerator()
		if !validProjectIDV2(projectID) {
			return TeamProject{}, fmt.Errorf("generate project id: %w", errProjectStoreInconsistent)
		}
		creationToken, err := tokenGenerator()
		if err != nil {
			return TeamProject{}, fmt.Errorf("generate project creation token: %w", err)
		}
		if !validProjectCreationTokenV2(creationToken) {
			return TeamProject{}, fmt.Errorf("generate project creation token: %w", errProjectStoreInconsistent)
		}

		dir := s.projectDirV2(teamID, projectID)
		if err := reserveProjectDirV2(dir); err != nil {
			if errors.Is(err, errProjectIDCollision) {
				continue
			}
			return TeamProject{}, err
		}

		headCommit, err := initializer(dir)
		if err != nil {
			removeReservedProjectDirV2(dir)
			return TeamProject{}, fmt.Errorf("initialize project git repository: %w", err)
		}
		if !validGitObjectID(headCommit) {
			removeReservedProjectDirV2(dir)
			return TeamProject{}, errors.New("initialize project git repository: invalid HEAD")
		}

		project := TeamProject{
			ID:         projectID,
			TeamID:     teamID,
			Name:       name,
			CreatedAt:  time.Now().UTC().Format(time.RFC3339Nano),
			HeadCommit: headCommit,
		}
		outcome, err := s.persistTeamProjectV2(ctx, project, creationToken)
		if err != nil {
			// Any transport/Redis error is uncertain: go-redis may have already
			// applied the write on a retry whose reply was lost, so we must never
			// blind-delete the work tree. Settle deterministically instead.
			settlement, settlementErr := s.settleTeamProjectPersistenceV2(project, creationToken)
			switch settlement {
			case projectPersistenceSettled:
				return s.reloadPersistedProjectV2(project)
			case projectPersistenceCollision:
				removeReservedProjectDirV2(dir)
				continue
			default: // projectPersistenceUnresolved: preserve the work tree.
				if settlementErr != nil {
					return TeamProject{}, settlementErr
				}
				return TeamProject{}, err
			}
		}

		switch outcome {
		case projectPersistCreated:
			return project, nil
		case projectPersistReplayed:
			// A same-token replay may have found an already-advanced head_commit;
			// reload the authoritative record instead of returning a stale head.
			return s.reloadPersistedProjectV2(project)
		case projectPersistCollided:
			removeReservedProjectDirV2(dir)
			continue
		default: // projectPersistCorrupt: same token, different immutable identity.
			return TeamProject{}, fmt.Errorf(
				"%w: project metadata mismatch on matching creation token",
				errProjectStoreInconsistent,
			)
		}
	}
	return TeamProject{}, errProjectIDExhausted
}

// reloadPersistedProjectV2 reads the authoritative project record after a
// confirmed create/replay so the caller returns the current head_commit rather
// than the value it attempted to write.
func (s *Server) reloadPersistedProjectV2(project TeamProject) (TeamProject, error) {
	ctx, cancel := context.WithTimeout(context.Background(), projectV2VerifyTimeout)
	defer cancel()
	return s.loadTeamProject(ctx, project.TeamID, project.ID)
}

func reserveProjectDirV2(dir string) error {
	workDir := filepath.Dir(filepath.Dir(dir))
	teamDir := filepath.Dir(dir)
	for _, parent := range []string{workDir, teamDir} {
		if err := ensureProjectParentDirV2(parent); err != nil {
			return fmt.Errorf("create project parent directory: %w", err)
		}
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		if errors.Is(err, os.ErrExist) {
			return errProjectIDCollision
		}
		return fmt.Errorf("reserve project directory: %w", err)
	}
	return nil
}

func validProjectPathComponentV2(value string) bool {
	return value != "" && value != "." && value != ".." && filepath.Base(value) == value
}

func ensureProjectParentDirV2(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("project parent is not a real directory")
	}
	return nil
}

func removeReservedProjectDirV2(dir string) {
	_ = os.RemoveAll(dir)
}

func initializeProjectGitV2(dir string) (string, error) {
	for _, command := range [][]string{
		{"init", "--quiet"},
		{"config", "--local", "user.name", "cowork"},
		{"config", "--local", "user.email", "cowork@localhost"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, command...)...).CombinedOutput(); err != nil {
			return "", fmt.Errorf("git %s: %w (%s)", command[0], err, strings.TrimSpace(string(out)))
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(seedIndexHTML), 0o644); err != nil {
		return "", fmt.Errorf("write seed index.html: %w", err)
	}
	for _, command := range [][]string{
		{"add", "-A"},
		{"commit", "--quiet", "-m", "initial commit"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, command...)...).CombinedOutput(); err != nil {
			return "", fmt.Errorf("git %s: %w (%s)", command[0], err, strings.TrimSpace(string(out)))
		}
	}
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	headCommit := strings.TrimSpace(string(out))
	if !validGitObjectID(headCommit) {
		return "", errors.New("git rev-parse HEAD returned an invalid object id")
	}
	return headCommit, nil
}

func (s *Server) persistTeamProjectV2(ctx context.Context, project TeamProject, creationToken string) (projectPersistOutcome, error) {
	result, err := createTeamProjectV2Script.Run(
		ctx,
		s.rdb,
		[]string{projectV2Key(project.ID), teamProjectsV2Key(project.TeamID)},
		project.ID,
		project.TeamID,
		project.Name,
		project.CreatedAt,
		project.HeadCommit,
		creationToken,
	).Int64()
	if err != nil {
		return projectPersistUncertain, fmt.Errorf("%w: persist project: %w", errProjectStoreInconsistent, err)
	}
	switch result {
	case 1:
		return projectPersistCreated, nil
	case 2:
		return projectPersistReplayed, nil
	case 3:
		return projectPersistCorrupt, nil
	case 0:
		return projectPersistCollided, nil
	default:
		return projectPersistUncertain, fmt.Errorf("%w: unexpected create result %d", errProjectStoreInconsistent, result)
	}
}

func (s *Server) settleTeamProjectPersistenceV2(
	project TeamProject,
	creationToken string,
) (projectPersistenceSettlement, error) {
	ctx, cancel := context.WithTimeout(context.Background(), projectV2VerifyTimeout)
	outcome, err := s.persistTeamProjectV2(ctx, project, creationToken)
	cancel()

	if err == nil {
		switch outcome {
		case projectPersistCreated, projectPersistReplayed:
			return projectPersistenceSettled, nil
		case projectPersistCollided:
			return projectPersistenceCollision, nil
		default: // projectPersistCorrupt: same token, different immutable identity.
			return projectPersistenceUnresolved, fmt.Errorf(
				"%w: project metadata mismatch on matching creation token",
				errProjectStoreInconsistent,
			)
		}
	}

	// The settlement re-run itself failed. Because a lost reply could still hide
	// a successful write, never delete here; fall back to a single read-only
	// atomic verification and only treat a different-token record as a collision.
	verification, verifyErr := s.verifyTeamProjectPersistenceV2(project, creationToken)
	if verifyErr != nil {
		return projectPersistenceUnresolved, verifyErr
	}
	switch verification {
	case projectPersistenceMatches:
		return projectPersistenceSettled, nil
	case projectPersistenceConflicts:
		return projectPersistenceCollision, nil
	case projectPersistenceAbsent:
		return projectPersistenceUnresolved, fmt.Errorf(
			"%w: project absent after uncertain persistence",
			errProjectStoreInconsistent,
		)
	case projectPersistenceBrokenIndex:
		return projectPersistenceUnresolved, fmt.Errorf(
			"%w: matching project has a missing or invalid team index",
			errProjectStoreInconsistent,
		)
	case projectPersistenceCorrupt:
		return projectPersistenceUnresolved, fmt.Errorf(
			"%w: project metadata is missing or corrupt",
			errProjectStoreInconsistent,
		)
	default:
		return projectPersistenceUnresolved, fmt.Errorf(
			"%w: unknown project persistence state",
			errProjectStoreInconsistent,
		)
	}
}

func (s *Server) verifyTeamProjectPersistenceV2(
	project TeamProject,
	creationToken string,
) (projectPersistenceVerification, error) {
	ctx, cancel := context.WithTimeout(context.Background(), projectV2VerifyTimeout)
	defer cancel()

	result, err := verifyTeamProjectV2Script.Run(
		ctx,
		s.rdb,
		[]string{projectV2Key(project.ID), teamProjectsV2Key(project.TeamID)},
		project.ID,
		project.TeamID,
		project.Name,
		project.CreatedAt,
		project.HeadCommit,
		creationToken,
	).Int64()
	if err != nil {
		return projectPersistenceUnknown, fmt.Errorf(
			"%w: verify project persistence: %w",
			errProjectStoreInconsistent,
			err,
		)
	}
	switch result {
	case 0:
		return projectPersistenceAbsent, nil
	case 1:
		return projectPersistenceMatches, nil
	case 2:
		return projectPersistenceConflicts, nil
	case 3:
		return projectPersistenceBrokenIndex, nil
	case 4:
		return projectPersistenceCorrupt, nil
	default:
		return projectPersistenceUnknown, fmt.Errorf(
			"%w: unexpected project verification result %d",
			errProjectStoreInconsistent,
			result,
		)
	}
}

func (s *Server) listTeamProjectsV2(ctx context.Context, teamID string) ([]TeamProject, error) {
	ids, err := s.rdb.SMembers(ctx, teamProjectsV2Key(teamID)).Result()
	if err != nil {
		return nil, fmt.Errorf("list team projects: %w: %v", errProjectStoreInconsistent, err)
	}

	projects := make([]TeamProject, 0, len(ids))
	for _, projectID := range ids {
		project, err := s.loadTeamProject(ctx, teamID, projectID)
		if errors.Is(err, errProjectNotFound) {
			if err := s.rdb.SRem(ctx, teamProjectsV2Key(teamID), projectID).Err(); err != nil {
				return nil, fmt.Errorf("remove stale team project: %w: %v", errProjectStoreInconsistent, err)
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		projects = append(projects, project)
	}

	sort.Slice(projects, func(i, j int) bool {
		left, _ := time.Parse(time.RFC3339Nano, projects[i].CreatedAt)
		right, _ := time.Parse(time.RFC3339Nano, projects[j].CreatedAt)
		if left.Equal(right) {
			return projects[i].ID < projects[j].ID
		}
		return left.Before(right)
	})
	return projects, nil
}
