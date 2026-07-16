package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// teamMembership is the CLI view of a team returned by GET /api/teams.
type teamMembership struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"`
}

type memberEntry struct {
	UserID   string `json:"user_id"`
	Username string `json:"username"`
	Role     string `json:"role"`
}

// teamOut is the destination for team command output; swappable in tests.
var teamOut io.Writer = os.Stdout

func fetchTeams(cfg *AgentConfig, creds *AgentCredentials) ([]teamMembership, error) {
	resp, err := APIDo(cfg, creds, http.MethodGet, "/api/teams", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Teams []teamMembership `json:"teams"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("cannot decode teams: %w", err)
	}
	return out.Teams, nil
}

// RunTeamList prints the caller's team memberships and marks the active team.
func RunTeamList() error {
	cfg, creds, err := LoadAuth()
	if err != nil {
		return err
	}
	teams, err := fetchTeams(cfg, creds)
	if err != nil {
		return err
	}
	if len(teams) == 0 {
		fmt.Fprintln(teamOut, "No teams yet. Create one with: agentlink team create <name>")
		return nil
	}
	for _, t := range teams {
		marker := " "
		if t.ID == cfg.CurrentTeam {
			marker = "*"
		}
		fmt.Fprintf(teamOut, "%s %s  %s  (%s)\n", marker, t.ID, t.Name, t.Role)
	}
	return nil
}

// RunTeamCreate creates a team, prints its one-time invite code, and selects it
// as the active team. The invite code is shown but never persisted.
func RunTeamCreate(name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("team name is required")
	}
	cfg, creds, err := LoadAuth()
	if err != nil {
		return err
	}
	resp, err := APIDo(cfg, creds, http.MethodPost, "/api/teams", map[string]string{"name": name})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var out struct {
		Team       teamMembership `json:"team"`
		InviteCode string         `json:"invite_code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("cannot decode team: %w", err)
	}
	if err := SetCurrentTeam(ConfigFilePath(), out.Team.ID); err != nil {
		return err
	}
	fmt.Fprintf(teamOut, "Created team %s (%s).\n", out.Team.Name, out.Team.ID)
	fmt.Fprintf(teamOut, "Invite code (shown once, share securely): %s\n", out.InviteCode)
	return nil
}

// RunTeamJoin joins a team with an invite code and selects it. The invite code
// is used for the request only and is never written to disk.
func RunTeamJoin(teamID, inviteCode string) error {
	if strings.TrimSpace(teamID) == "" || strings.TrimSpace(inviteCode) == "" {
		return errors.New("team id and invite code are required")
	}
	cfg, creds, err := LoadAuth()
	if err != nil {
		return err
	}
	resp, err := APIDo(cfg, creds, http.MethodPost, "/api/teams/join", map[string]string{
		"team_id":     teamID,
		"invite_code": inviteCode,
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var team teamMembership
	if err := json.NewDecoder(resp.Body).Decode(&team); err != nil {
		return fmt.Errorf("cannot decode team: %w", err)
	}
	if err := SetCurrentTeam(ConfigFilePath(), team.ID); err != nil {
		return err
	}
	fmt.Fprintf(teamOut, "Joined team %s (%s).\n", team.Name, team.ID)
	return nil
}

// RunTeamUse validates a membership against the server before persisting it as
// the active team, so a typo cannot select a team the user cannot access.
func RunTeamUse(teamID string) error {
	if strings.TrimSpace(teamID) == "" {
		return errors.New("team id is required")
	}
	cfg, creds, err := LoadAuth()
	if err != nil {
		return err
	}
	teams, err := fetchTeams(cfg, creds)
	if err != nil {
		return err
	}
	for _, t := range teams {
		if t.ID == teamID {
			if err := SetCurrentTeam(ConfigFilePath(), teamID); err != nil {
				return err
			}
			fmt.Fprintf(teamOut, "Now using team %s (%s).\n", t.Name, t.ID)
			return nil
		}
	}
	return fmt.Errorf("not a member of team %s", teamID)
}

// RunTeamMembers lists the members of the active team.
func RunTeamMembers() error {
	cfg, creds, err := LoadAuth()
	if err != nil {
		return err
	}
	path, err := TeamPath(cfg, "/members")
	if err != nil {
		return err
	}
	resp, err := APIDo(cfg, creds, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var out struct {
		Members []memberEntry `json:"members"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("cannot decode members: %w", err)
	}
	for _, m := range out.Members {
		fmt.Fprintf(teamOut, "%s  %s  (%s)\n", m.UserID, m.Username, m.Role)
	}
	return nil
}

// RunTeamLeave leaves the active team and clears current_team only after the
// server confirms the departure.
func RunTeamLeave() error {
	cfg, creds, err := LoadAuth()
	if err != nil {
		return err
	}
	path, err := TeamPath(cfg, "/leave")
	if err != nil {
		return err
	}
	resp, err := APIDo(cfg, creds, http.MethodPost, path, nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if err := SetCurrentTeam(ConfigFilePath(), ""); err != nil {
		return err
	}
	fmt.Fprintln(teamOut, "Left the team.")
	return nil
}
