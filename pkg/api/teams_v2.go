package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/team/agentlink/pkg/auth"
)

type teamView struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	OwnerUserID string    `json:"owner_user_id"`
	CreatedAt   time.Time `json:"created_at"`
	Role        string    `json:"role"`
}

type memberView struct {
	UserID   string `json:"user_id"`
	Username string `json:"username"`
	Role     string `json:"role"`
}

type createTeamRequest struct {
	Name string `json:"name"`
}

type joinTeamRequest struct {
	TeamID     string `json:"team_id"`
	InviteCode string `json:"invite_code"`
}

type changeTeamRoleRequest struct {
	Role string `json:"role"`
}

type transferTeamOwnerRequest struct {
	UserID string `json:"user_id"`
}

type createTeamResponse struct {
	Team       teamView `json:"team"`
	InviteCode string   `json:"invite_code"`
}

type listTeamsResponse struct {
	Teams []teamView `json:"teams"`
}

type listTeamMembersResponse struct {
	Members []memberView `json:"members"`
}

type rotateInviteResponse struct {
	InviteCode string `json:"invite_code"`
}

func toTeamView(team auth.Team, role auth.Role) teamView {
	return teamView{
		ID:          team.ID,
		Name:        team.Name,
		OwnerUserID: team.OwnerUserID,
		CreatedAt:   team.CreatedAt,
		Role:        string(role),
	}
}

func toMemberView(member auth.Member) memberView {
	return memberView{
		UserID:   member.UserID,
		Username: member.Username,
		Role:     string(member.Role),
	}
}

func (s *Server) writeTeamServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, auth.ErrInvalidInvite):
		writeError(w, http.StatusBadRequest, "invalid team or invite code")
	case errors.Is(err, auth.ErrInvalidRole):
		writeError(w, http.StatusBadRequest, "invalid role")
	case errors.Is(err, auth.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, auth.ErrNotMember), errors.Is(err, auth.ErrForbidden):
		writeError(w, http.StatusForbidden, "forbidden")
	case errors.Is(err, auth.ErrAlreadyMember), errors.Is(err, auth.ErrUsernameExists):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, auth.ErrTeamIDExhausted), errors.Is(err, auth.ErrStoreInconsistent):
		writeError(w, http.StatusInternalServerError, "internal error")
	default:
		msg := err.Error()
		if strings.Contains(msg, "team name") || strings.Contains(msg, "characters") {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func (s *Server) handleCreateTeam(w http.ResponseWriter, r *http.Request) {
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req createTeamRequest
	if !decodeAuthJSON(w, r, &req) {
		return
	}

	result, err := s.authService.CreateTeam(r.Context(), actor.UserID, req.Name)
	if err != nil {
		s.writeTeamServiceError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, createTeamResponse{
		Team:       toTeamView(result.Team, auth.RoleOwner),
		InviteCode: result.InviteCode,
	})
}

func (s *Server) handleListTeams(w http.ResponseWriter, r *http.Request) {
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	teams, err := s.authService.ListTeams(r.Context(), actor.UserID)
	if err != nil {
		s.writeTeamServiceError(w, err)
		return
	}

	views := make([]teamView, 0, len(teams))
	for _, team := range teams {
		role, err := s.authService.TeamRole(r.Context(), team.ID, actor.UserID)
		if err != nil {
			s.writeTeamServiceError(w, err)
			return
		}
		views = append(views, toTeamView(team, role))
	}
	writeJSON(w, http.StatusOK, listTeamsResponse{Teams: views})
}

func (s *Server) handleJoinTeam(w http.ResponseWriter, r *http.Request) {
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req joinTeamRequest
	if !decodeAuthJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.TeamID) == "" || strings.TrimSpace(req.InviteCode) == "" {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	team, err := s.authService.JoinTeam(r.Context(), actor.UserID, req.TeamID, req.InviteCode)
	if err != nil {
		s.writeTeamServiceError(w, err)
		return
	}
	role, err := s.authService.TeamRole(r.Context(), team.ID, actor.UserID)
	if err != nil {
		s.writeTeamServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toTeamView(team, role))
}

func (s *Server) handleGetTeam(w http.ResponseWriter, r *http.Request) {
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	team, role, err := s.authService.GetTeamForMember(r.Context(), actor.TeamID, actor.UserID)
	if err != nil {
		s.writeTeamServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toTeamView(team, role))
}

func (s *Server) handleListTeamMembers(w http.ResponseWriter, r *http.Request) {
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	members, err := s.authService.ListMembers(r.Context(), actor)
	if err != nil {
		s.writeTeamServiceError(w, err)
		return
	}

	views := make([]memberView, 0, len(members))
	for _, member := range members {
		views = append(views, toMemberView(member))
	}
	writeJSON(w, http.StatusOK, listTeamMembersResponse{Members: views})
}

func (s *Server) handleRotateTeamInvite(w http.ResponseWriter, r *http.Request) {
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	inviteCode, err := s.authService.RotateInvite(r.Context(), actor)
	if err != nil {
		s.writeTeamServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rotateInviteResponse{InviteCode: inviteCode})
}

func (s *Server) handleChangeTeamRole(w http.ResponseWriter, r *http.Request) {
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	targetUserID := r.PathValue("user_id")
	if targetUserID == "" {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	var req changeTeamRoleRequest
	if !decodeAuthJSON(w, r, &req) {
		return
	}
	role := auth.Role(strings.TrimSpace(req.Role))
	if role != auth.RoleAdmin && role != auth.RoleMember {
		writeError(w, http.StatusBadRequest, "invalid role")
		return
	}

	if err := s.authService.ChangeRole(r.Context(), actor, targetUserID, role); err != nil {
		s.writeTeamServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRemoveTeamMember(w http.ResponseWriter, r *http.Request) {
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	targetUserID := r.PathValue("user_id")
	if targetUserID == "" {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if err := s.authService.RemoveMember(r.Context(), actor, targetUserID); err != nil {
		s.writeTeamServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleTransferTeamOwner(w http.ResponseWriter, r *http.Request) {
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req transferTeamOwnerRequest
	if !decodeAuthJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.UserID) == "" {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if err := s.authService.TransferOwner(r.Context(), actor, req.UserID); err != nil {
		s.writeTeamServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleLeaveTeam(w http.ResponseWriter, r *http.Request) {
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	if err := s.authService.LeaveTeam(r.Context(), actor); err != nil {
		s.writeTeamServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
