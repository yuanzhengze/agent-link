package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/team/agentlink/pkg/auth"
)

// handlePreviewV2 serves a team project's static prototype files to
// authenticated team members only. It authenticates the Web Session cookie and
// verifies team membership + project ownership itself. HTML responses get a
// cookie-based live-reload script injected (no token in the URL); other files
// are served as-is with a derived Content-Type.
func (s *Server) handlePreviewV2(w http.ResponseWriter, r *http.Request) {
	teamID := r.PathValue("team_id")
	projectID := r.PathValue("project_id")
	rel := r.PathValue("path")
	if rel == "" || strings.HasSuffix(rel, "/") {
		rel += "index.html"
	}

	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	user, _, err := s.authService.ResolveWebSession(r.Context(), cookie.Value)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	if _, err := s.authService.TeamRole(r.Context(), teamID, user.ID); err != nil {
		switch {
		case errors.Is(err, auth.ErrNotMember):
			writeError(w, http.StatusForbidden, "forbidden")
		case errors.Is(err, auth.ErrNotFound):
			writeError(w, http.StatusNotFound, "not found")
		default:
			writeError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}

	if _, err := s.loadTeamProject(r.Context(), teamID, projectID); err != nil {
		// A member should not be able to distinguish "not in my team" from
		// "does not exist": both are 404.
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	// Reuse the hardened traversal/absolute-path/.git checks; any rejection
	// maps to 404 so a reader learns nothing about why a path is unreachable.
	fullPath, err := safeApplyPath(s.projectDirV2(teamID, projectID), rel)
	if err != nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	// Stat first so a directory (or any non-existent/unreadable path) maps to
	// 404 rather than leaking a 500 that reveals the target is a directory.
	info, err := os.Stat(fullPath)
	if err != nil || info.IsDir() {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	data, err := os.ReadFile(fullPath)
	if err != nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	// nosniff prevents the browser from MIME-sniffing a non-HTML asset into
	// executable HTML/JS. Note this does NOT neutralize stored XSS in genuine
	// .html prototypes: preview is served same-origin as the app (so a member's
	// injected <script> can read the readable al_csrf cookie and act as a
	// viewing member). Origin isolation is the real fix; see the team-auth
	// design's preview section. Threat is insider-only (a member attacking
	// teammates), matching v1's accepted model.
	w.Header().Set("X-Content-Type-Options", "nosniff")

	ext := filepath.Ext(fullPath)
	if strings.EqualFold(ext, ".html") || strings.EqualFold(ext, ".htm") {
		script, err := liveReloadScriptV2(teamID, projectID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(injectBeforeBodyClose(data, script))
		return
	}

	contentType := mime.TypeByExtension(ext)
	if contentType == "" {
		contentType = http.DetectContentType(data)
	}
	w.Header().Set("Content-Type", contentType)
	w.Write(data)
}

// liveReloadScriptV2 renders the live-reload <script> injected into team
// preview HTML. The WebSocket authenticates via the browser's HttpOnly session
// cookie (same-origin), so no credential appears in the URL. teamID/projectID
// are JSON-encoded into JS string literals to prevent script-tag breakout.
func liveReloadScriptV2(teamID, projectID string) (string, error) {
	teamJS, err := json.Marshal(teamID)
	if err != nil {
		return "", err
	}
	projectJS, err := json.Marshal(projectID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`<script>
(function(){var ws=new WebSocket((location.protocol==='https:'?'wss':'ws')+'://'+location.host+'/api/teams/'+encodeURIComponent(%s)+'/ws?project='+encodeURIComponent(%s));
ws.onmessage=function(e){try{var m=JSON.parse(e.data);if(m.type==='file_changed'&&m.project_id===%s)location.reload();}catch(_){}}})();
</script>
`, teamJS, projectJS, projectJS), nil
}

// injectBeforeBodyClose inserts script immediately before the last
// case-insensitive "</body>" tag in html, or appends it at the end of the
// document if no such tag is present.
func injectBeforeBodyClose(html []byte, script string) []byte {
	idx := bytes.LastIndex(bytes.ToLower(html), []byte("</body>"))
	if idx == -1 {
		out := make([]byte, 0, len(html)+len(script))
		out = append(out, html...)
		return append(out, []byte(script)...)
	}
	out := make([]byte, 0, len(html)+len(script))
	out = append(out, html[:idx]...)
	out = append(out, []byte(script)...)
	return append(out, html[idx:]...)
}
