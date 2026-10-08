package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"time"
)

// deviceNameRE validates agent session and device names: 2-32 characters,
// lowercase letters/digits/hyphens/underscores, starting with a letter. Shared
// by the team-scoped agent, message, task, and actor-session paths.
var deviceNameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,31}$`)

// Message types — shared between server, net layer, and poller.
const (
	MsgTypeMsg  = "msg"
	MsgTypeTask = "task"
)

// Task-send Lua reject reasons — returned in the second element of the script
// result array and mapped to HTTP errors by the team task handlers.
const (
	rejectDup       = "dup"
	rejectBusy      = "busy"
	rejectSuspended = "suspended"
)

type HealthResponse struct {
	Ok    bool   `json:"ok"`
	Redis string `json:"redis"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	redisStatus := "connected"
	if err := s.rdb.Ping(r.Context()).Err(); err != nil {
		redisStatus = "disconnected"
	}

	ok := redisStatus == "connected"
	status := http.StatusOK
	if !ok {
		status = http.StatusServiceUnavailable
	}

	writeJSON(w, status, HealthResponse{
		Ok:    ok,
		Redis: redisStatus,
	})
}

// formatDuration renders a duration as a human-readable short string:
// <60s → "Xs", <60m → "Xm", else → "Xh".
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh", int(d.Hours()))
}

// generateID returns a random 128-bit hex id, used for project/message/task ids.
func generateID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// truncateTitle returns the first up to 40 runes of s, for default msg titles.
func truncateTitle(s string) string {
	r := []rune(s)
	if len(r) <= 40 {
		return s
	}
	return string(r[:40]) + "..."
}

// msgTitle returns the title when non-empty, otherwise truncates content.
func msgTitle(title, content string) string {
	if title != "" {
		return title
	}
	return truncateTitle(content)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
