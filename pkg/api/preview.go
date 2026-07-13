package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// handlePreview serves a project's static prototype files for
// unauthenticated browser preview (GET /preview/{id}/{path...}; exempted
// from authMiddleware by skipAuth). HTML responses have a live-reload
// <script> injected before </body> (or appended, if none) that subscribes
// to GET /ws using the server's read-only preview token and calls
// location.reload() when it observes a file_changed event for this
// project. Non-HTML files are served as-is with a derived Content-Type
// and no injection.
func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rel := r.PathValue("path")

	if rel == "" || strings.HasSuffix(rel, "/") {
		rel += "index.html"
	}

	exists, err := s.rdb.Exists(r.Context(), "agentlink:project:"+id).Result()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if exists == 0 {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	// Reuse the same hardened traversal/absolute-path/.git checks apply.go
	// uses, but map any rejection to 404 (not 400): an unauthenticated
	// preview reader should not learn anything about why a path is
	// unreachable, just that it doesn't exist.
	fullPath, err := safeApplyPath(s.projectDir(id), rel)
	if err != nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	data, err := os.ReadFile(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, "not found")
		} else {
			writeError(w, http.StatusInternalServerError, "failed to read file")
		}
		return
	}

	ext := filepath.Ext(fullPath)
	if strings.EqualFold(ext, ".html") || strings.EqualFold(ext, ".htm") {
		script, err := liveReloadScript(id, s.previewToken)
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

// liveReloadScript renders the <script> injected into preview HTML. id and
// token are JSON-encoded into JS string literals rather than naively
// string-concatenated, so a project id containing quotes or HTML cannot
// break out of the <script> tag. The socket subscribes for this project
// and reloads the page on any file_changed event matching it — the exact
// event shape and type apply.go's handleApply broadcasts.
func liveReloadScript(id, token string) (string, error) {
	idJS, err := json.Marshal(id)
	if err != nil {
		return "", err
	}
	tokenJS, err := json.Marshal(token)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`<script>
(function(){var ws=new WebSocket((location.protocol==='https:'?'wss':'ws')+'://'+location.host+'/ws?project='+%s+'&token='+%s);
ws.onmessage=function(e){try{var m=JSON.parse(e.data);if(m.type==='file_changed'&&m.project===%s)location.reload();}catch(_){}}})();
</script>
`, idJS, tokenJS, idJS), nil
}
