package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

var (
	errProjectNotFound          = errors.New("project not found")
	errProjectStoreInconsistent = errors.New("project store inconsistent")
	errProjectIDCollision       = errors.New("project id collision")
	errProjectIDExhausted       = errors.New("project id exhausted")
)

type TeamProject struct {
	ID         string `json:"id"`
	TeamID     string `json:"team_id"`
	Name       string `json:"name"`
	CreatedAt  string `json:"created_at"`
	HeadCommit string `json:"head_commit"`
}

func projectV2Key(projectID string) string {
	return "agentlink:v2:project:" + projectID
}

func teamProjectsV2Key(teamID string) string {
	return "agentlink:v2:team:" + teamID + ":projects"
}

func (s *Server) projectDirV2(teamID, projectID string) string {
	return filepath.Join(s.dataDir, "work", teamID, projectID)
}

func (s *Server) loadTeamProject(ctx context.Context, teamID, projectID string) (TeamProject, error) {
	fields, err := s.rdb.HGetAll(ctx, projectV2Key(projectID)).Result()
	if err != nil {
		return TeamProject{}, fmt.Errorf("load project %q: %w: %v", projectID, errProjectStoreInconsistent, err)
	}
	if len(fields) == 0 {
		return TeamProject{}, errProjectNotFound
	}

	storedTeamID := fields["team_id"]
	if storedTeamID == "" {
		return TeamProject{}, fmt.Errorf("load project %q: missing team_id: %w", projectID, errProjectStoreInconsistent)
	}
	if storedTeamID != teamID {
		return TeamProject{}, errProjectNotFound
	}

	project := TeamProject{
		ID:         fields["id"],
		TeamID:     storedTeamID,
		Name:       fields["name"],
		CreatedAt:  fields["created_at"],
		HeadCommit: fields["head_commit"],
	}
	if err := validateStoredTeamProject(project, projectID, fields["creation_token"]); err != nil {
		return TeamProject{}, fmt.Errorf("load project %q: %w: %v", projectID, errProjectStoreInconsistent, err)
	}
	return project, nil
}

func validateStoredTeamProject(project TeamProject, projectID, creationToken string) error {
	if project.ID != projectID || !validProjectIDV2(project.ID) {
		return errors.New("invalid id")
	}
	if !validProjectCreationTokenV2(creationToken) {
		return errors.New("invalid creation_token")
	}
	name, err := validateProjectNameV2(project.Name)
	if err != nil || name != project.Name {
		return errors.New("invalid name")
	}
	if _, err := time.Parse(time.RFC3339Nano, project.CreatedAt); err != nil {
		return errors.New("invalid created_at")
	}
	if !validGitObjectID(project.HeadCommit) {
		return errors.New("invalid head_commit")
	}
	return nil
}

func generateProjectCreationTokenV2() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate project creation token: %w", err)
	}
	return hex.EncodeToString(value), nil
}

func validProjectCreationTokenV2(token string) bool {
	if len(token) != 32 {
		return false
	}
	_, err := hex.DecodeString(token)
	return err == nil
}

func validProjectIDV2(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func validGitObjectID(id string) bool {
	if len(id) != 40 && len(id) != 64 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil && strings.ToLower(id) == id
}
