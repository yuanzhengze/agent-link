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
  return 0
end

redis.call('HSET', KEYS[1],
  'id', ARGV[1],
  'team_id', ARGV[2],
  'name', ARGV[3],
  'created_at', ARGV[4],
  'head_commit', ARGV[5])
redis.call('SADD', KEYS[2], ARGV[1])
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
	initializer := s.projectGitInitializer
	if initializer == nil {
		initializer = initializeProjectGitV2
	}

	for attempt := 0; attempt < projectV2CreateAttempts; attempt++ {
		projectID := idGenerator()
		if !validProjectIDV2(projectID) {
			return TeamProject{}, fmt.Errorf("generate project id: %w", errProjectStoreInconsistent)
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
		err = s.persistTeamProjectV2(ctx, project)
		switch {
		case err == nil:
			return project, nil
		case errors.Is(err, errProjectIDCollision):
			removeReservedProjectDirV2(dir)
			continue
		case projectPersistenceMayBeUncertain(err):
			verification := s.verifyTeamProjectPersistenceV2(project)
			switch verification {
			case projectPersistenceMatches:
				return project, nil
			case projectPersistenceAbsent:
				removeReservedProjectDirV2(dir)
			case projectPersistenceUnknown, projectPersistenceConflicts:
				// The hash may refer to this work tree. Preserve the directory
				// rather than risk metadata pointing at a missing tree.
			}
			return TeamProject{}, err
		default:
			removeReservedProjectDirV2(dir)
			return TeamProject{}, err
		}
	}
	return TeamProject{}, errProjectIDExhausted
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

func (s *Server) persistTeamProjectV2(ctx context.Context, project TeamProject) error {
	result, err := createTeamProjectV2Script.Run(
		ctx,
		s.rdb,
		[]string{projectV2Key(project.ID), teamProjectsV2Key(project.TeamID)},
		project.ID,
		project.TeamID,
		project.Name,
		project.CreatedAt,
		project.HeadCommit,
	).Int64()
	if err != nil {
		return fmt.Errorf("%w: persist project: %w", errProjectStoreInconsistent, err)
	}
	switch result {
	case 1:
		return nil
	case 0:
		return errProjectIDCollision
	default:
		return fmt.Errorf("%w: unexpected create result %d", errProjectStoreInconsistent, result)
	}
}

func projectPersistenceMayBeUncertain(err error) bool {
	var redisErr goredis.Error
	return !errors.As(err, &redisErr)
}

func (s *Server) verifyTeamProjectPersistenceV2(project TeamProject) projectPersistenceVerification {
	ctx, cancel := context.WithTimeout(context.Background(), projectV2VerifyTimeout)
	defer cancel()

	key := projectV2Key(project.ID)
	keyType, err := s.rdb.Type(ctx, key).Result()
	if err != nil {
		return projectPersistenceUnknown
	}
	switch keyType {
	case "none":
		return projectPersistenceAbsent
	case "hash":
		fields, err := s.rdb.HGetAll(ctx, key).Result()
		if err != nil {
			return projectPersistenceUnknown
		}
		if fields["id"] == project.ID &&
			fields["team_id"] == project.TeamID &&
			fields["name"] == project.Name &&
			fields["created_at"] == project.CreatedAt &&
			fields["head_commit"] == project.HeadCommit {
			return projectPersistenceMatches
		}
		return projectPersistenceConflicts
	default:
		return projectPersistenceAbsent
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
