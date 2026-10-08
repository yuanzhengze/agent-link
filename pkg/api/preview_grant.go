package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
	goredis "github.com/redis/go-redis/v9"

	"github.com/team/agentlink/pkg/auth"
)

// previewGrantTTL is how long a read-only preview capability stays valid.
// The GUI mints a new one each time it loads the iframe, so this only has to
// cover a viewing session and the subresources fetched from that document.
const previewGrantTTL = 30 * time.Minute

type previewGrantRecord struct {
	UserID    string `json:"user_id"`
	TeamID    string `json:"team_id"`
	ProjectID string `json:"project_id"`
}

type previewGrantResponse struct {
	BootstrapURL string `json:"bootstrap_url"`
	Isolated     bool   `json:"isolated"`
	ExpiresIn    int    `json:"expires_in,omitempty"`
}

func previewGrantRedisKey(hash string) string {
	return "agentlink:v2:preview_grant:" + hash
}

func (s *Server) previewIsolated() bool {
	return s.previewOrigin != "" && s.previewOrigin != s.publicOrigin
}

func (s *Server) requestMatchesOrigin(r *http.Request, origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(r.Host, u.Host)
}

// handleIssuePreviewGrant mints the URL the GUI puts in the preview iframe.
// When preview is isolated, the URL is on the preview origin and carries a
// short-lived read-only grant. Otherwise it is the legacy same-origin path,
// which still authenticates with the session cookie.
func (s *Server) handleIssuePreviewGrant(w http.ResponseWriter, r *http.Request) {
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	teamID := r.PathValue("team_id")
	projectID := r.PathValue("project_id")
	if _, err := s.loadTeamProject(r.Context(), teamID, projectID); err != nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	if !s.previewIsolated() {
		writeJSON(w, http.StatusOK, previewGrantResponse{
			BootstrapURL: "/preview/" + url.PathEscape(teamID) + "/" + url.PathEscape(projectID) + "/",
			Isolated:     false,
		})
		return
	}

	grant, err := auth.NewSecret("pg_", 32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	record, err := json.Marshal(previewGrantRecord{
		UserID:    actor.UserID,
		TeamID:    teamID,
		ProjectID: projectID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := s.rdb.Set(r.Context(), previewGrantRedisKey(auth.SecretHash(grant)), record, previewGrantTTL).Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	writeJSON(w, http.StatusOK, previewGrantResponse{
		BootstrapURL: s.previewGrantURL(teamID, projectID, grant),
		Isolated:     true,
		ExpiresIn:    int(previewGrantTTL.Seconds()),
	})
}

func (s *Server) previewGrantURL(teamID, projectID, grant string) string {
	return strings.TrimRight(s.previewOrigin, "/") +
		"/preview/" + url.PathEscape(teamID) +
		"/" + url.PathEscape(projectID) +
		"/g/" + url.PathEscape(grant) + "/"
}

func (s *Server) handlePreviewGrant(w http.ResponseWriter, r *http.Request) {
	if !s.previewIsolated() || !s.requestMatchesOrigin(r, s.previewOrigin) {
		// Refuse to render member HTML on the application host, and refuse
		// grant URLs entirely when isolation is off.
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	teamID := r.PathValue("team_id")
	projectID := r.PathValue("project_id")
	grant := r.PathValue("grant")
	if _, err := s.authorizePreviewGrant(r, grant, teamID, projectID); err != nil {
		writePreviewGrantError(w, err)
		return
	}
	script, err := liveReloadScriptGrant(teamID, projectID, grant)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.writePreviewFile(w, teamID, projectID, r.PathValue("path"), script, true)
}

func (s *Server) handlePreviewGrantWS(w http.ResponseWriter, r *http.Request) {
	if !s.previewIsolated() || !s.requestMatchesOrigin(r, s.previewOrigin) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	teamID := r.PathValue("team_id")
	projectID := r.PathValue("project_id")
	if _, err := s.authorizePreviewGrant(r, r.PathValue("grant"), teamID, projectID); err != nil {
		writePreviewGrantError(w, err)
		return
	}

	// The preview iframe is sandboxed without allow-same-origin, so the browser
	// sends Origin: null. The unguessable grant already authorized this socket;
	// it is read-only and cannot be used as an application session.
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	key := subscriptionKeyV2{TeamID: teamID, ProjectID: projectID}
	c := &connV2{ws: ws, key: key}
	s.hubV2.register(key, c)
	defer func() {
		s.hubV2.unregister(c)
		ws.CloseNow()
	}()
	for {
		if _, _, err := ws.Read(r.Context()); err != nil {
			return
		}
	}
}

func (s *Server) authorizePreviewGrant(r *http.Request, grant, teamID, projectID string) (previewGrantRecord, error) {
	if grant == "" || len(grant) > 128 || !strings.HasPrefix(grant, "pg_") {
		return previewGrantRecord{}, errPreviewGrantUnauthorized
	}
	raw, err := s.rdb.Get(r.Context(), previewGrantRedisKey(auth.SecretHash(grant))).Result()
	if err != nil {
		if errors.Is(err, goredis.Nil) {
			return previewGrantRecord{}, errPreviewGrantUnauthorized
		}
		return previewGrantRecord{}, err
	}
	var rec previewGrantRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		return previewGrantRecord{}, errPreviewGrantUnauthorized
	}
	if rec.TeamID != teamID || rec.ProjectID != projectID || rec.UserID == "" {
		return previewGrantRecord{}, errPreviewGrantNotFound
	}
	if _, err := s.authService.TeamRole(r.Context(), teamID, rec.UserID); err != nil {
		if errors.Is(err, auth.ErrNotMember) || errors.Is(err, auth.ErrNotFound) {
			return previewGrantRecord{}, errPreviewGrantUnauthorized
		}
		return previewGrantRecord{}, err
	}
	if _, err := s.loadTeamProject(r.Context(), teamID, projectID); err != nil {
		if errors.Is(err, errProjectNotFound) {
			return previewGrantRecord{}, errPreviewGrantNotFound
		}
		return previewGrantRecord{}, err
	}
	return rec, nil
}

var (
	errPreviewGrantUnauthorized = errors.New("preview grant unauthorized")
	errPreviewGrantNotFound     = errors.New("preview grant not found")
)

func writePreviewGrantError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errPreviewGrantNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, errPreviewGrantUnauthorized):
		writeError(w, http.StatusUnauthorized, "unauthorized")
	default:
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

// liveReloadScriptGrant is the live-reload snippet for an isolated preview.
// It subscribes with the read-only grant. The grant is JSON-encoded so it
// cannot break out of the script tag.
func liveReloadScriptGrant(teamID, projectID, grant string) (string, error) {
	teamJS, err := json.Marshal(teamID)
	if err != nil {
		return "", err
	}
	projectJS, err := json.Marshal(projectID)
	if err != nil {
		return "", err
	}
	grantJS, err := json.Marshal(grant)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`<script>
(function(){var ws=new WebSocket((location.protocol==='https:'?'wss':'ws')+'://'+location.host+'/preview/'+encodeURIComponent(%s)+'/'+encodeURIComponent(%s)+'/g/'+encodeURIComponent(%s)+'/ws');
ws.onmessage=function(e){try{var m=JSON.parse(e.data);if(m.type==='file_changed'&&m.project_id===%s)location.reload();}catch(_){}}})();
</script>
`, teamJS, projectJS, grantJS, projectJS), nil
}
