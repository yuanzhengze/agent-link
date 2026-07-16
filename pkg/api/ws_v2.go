package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// wsWriteTimeout bounds how long a single broadcast write to one connection
// may block, so one slow/dead subscriber can never stall the hub.
const wsWriteTimeout = 5 * time.Second

// subscriptionKeyV2 identifies a team project's broadcast channel by an
// unambiguous (team, project) pair so a project id can never be confused
// across teams.
type subscriptionKeyV2 struct {
	TeamID    string
	ProjectID string
}

// connV2 wraps a subscriber connection with the channel it joined so
// unregister can remove it from the correct bucket.
type connV2 struct {
	ws  *websocket.Conn
	key subscriptionKeyV2
}

// HubV2 implements BroadcasterV2 using WebSocket connections. Clients
// subscribe via GET /api/teams/{team_id}/ws?project=<id> after cookie/device
// authentication and team/project authorization, and receive every EventV2
// broadcast for that (team, project).
type HubV2 struct {
	mu   sync.RWMutex
	subs map[subscriptionKeyV2]map[*connV2]struct{}
}

func NewHubV2() *HubV2 {
	return &HubV2{subs: make(map[subscriptionKeyV2]map[*connV2]struct{})}
}

func (h *HubV2) register(key subscriptionKeyV2, c *connV2) {
	h.mu.Lock()
	defer h.mu.Unlock()
	set, ok := h.subs[key]
	if !ok {
		set = make(map[*connV2]struct{})
		h.subs[key] = set
	}
	set[c] = struct{}{}
}

func (h *HubV2) unregister(c *connV2) {
	h.mu.Lock()
	defer h.mu.Unlock()
	set, ok := h.subs[c.key]
	if !ok {
		return
	}
	delete(set, c)
	if len(set) == 0 {
		delete(h.subs, c.key)
	}
}

// Broadcast JSON-encodes event and writes it to every connection subscribed to
// the (team, project) channel. Writes fan out one goroutine per connection,
// each bounded by wsWriteTimeout, so a slow/dead subscriber cannot delay the
// others. A write failure unregisters and closes that connection only.
func (h *HubV2) Broadcast(teamID, projectID string, event EventV2) {
	data, err := json.Marshal(event)
	if err != nil {
		return
	}
	key := subscriptionKeyV2{TeamID: teamID, ProjectID: projectID}

	h.mu.RLock()
	conns := make([]*connV2, 0, len(h.subs[key]))
	for c := range h.subs[key] {
		conns = append(conns, c)
	}
	h.mu.RUnlock()

	for _, c := range conns {
		go func(c *connV2) {
			ctx, cancel := context.WithTimeout(context.Background(), wsWriteTimeout)
			defer cancel()
			if err := c.ws.Write(ctx, websocket.MessageText, data); err != nil {
				h.unregister(c)
				c.ws.CloseNow()
			}
		}(c)
	}
}

// handleWSV2 upgrades an authenticated, authorized subscriber to a WebSocket.
// Authentication (cookie or device header) and team membership are enforced by
// requireIdentity + requireTeamRole before this runs; here we only validate
// that the requested project belongs to the actor's team, then accept.
func (s *Server) handleWSV2(w http.ResponseWriter, r *http.Request) {
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	projectID := r.URL.Query().Get("project")
	if projectID == "" {
		writeError(w, http.StatusBadRequest, "missing project parameter")
		return
	}
	if _, err := s.loadTeamProject(r.Context(), actor.TeamID, projectID); err != nil {
		if errors.Is(err, errProjectNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}

	key := subscriptionKeyV2{TeamID: actor.TeamID, ProjectID: projectID}
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
