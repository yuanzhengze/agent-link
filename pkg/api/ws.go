package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/coder/websocket"
	"github.com/team/agentlink/pkg/redis"
)

// wsWriteTimeout bounds how long a single broadcast write to one connection
// may take before it's considered dead and unregistered.
const wsWriteTimeout = 5 * time.Second

// Hub implements Broadcaster (defined in apply.go, Task 3) using WebSocket
// connections: clients subscribe to a project via GET /ws?project=<id> and
// receive every Event broadcast for that project as JSON.
type Hub struct {
	rdb  *redis.Client
	mu   sync.RWMutex
	subs map[string]map[*conn]struct{} // project -> set of conns
}

// conn wraps a *websocket.Conn with the project it's subscribed to, so
// unregister can find and remove it from the right subs bucket.
type conn struct {
	ws      *websocket.Conn
	project string
}

// NewHub constructs a Hub. rdb is used to validate the ?token= query param
// on GET /ws the same way authMiddleware validates Bearer keys, since
// browsers cannot set an Authorization header on a WebSocket handshake.
func NewHub(rdb *redis.Client) *Hub {
	return &Hub{
		rdb:  rdb,
		subs: make(map[string]map[*conn]struct{}),
	}
}

// register adds c to the project's subscriber set.
func (h *Hub) register(project string, c *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	set, ok := h.subs[project]
	if !ok {
		set = make(map[*conn]struct{})
		h.subs[project] = set
	}
	set[c] = struct{}{}
}

// unregister removes c from the project's subscriber set, cleaning up the
// bucket if it becomes empty.
func (h *Hub) unregister(c *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	set, ok := h.subs[c.project]
	if !ok {
		return
	}
	delete(set, c)
	if len(set) == 0 {
		delete(h.subs, c.project)
	}
}

// Broadcast JSON-encodes ev and writes it to every connection currently
// subscribed to project. A write failure on one connection unregisters and
// closes it but does not stop delivery to the others.
func (h *Hub) Broadcast(project string, ev Event) {
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}

	h.mu.RLock()
	conns := make([]*conn, 0, len(h.subs[project]))
	for c := range h.subs[project] {
		conns = append(conns, c)
	}
	h.mu.RUnlock()

	for _, c := range conns {
		ctx, cancel := context.WithTimeout(context.Background(), wsWriteTimeout)
		err := c.ws.Write(ctx, websocket.MessageText, data)
		cancel()
		if err != nil {
			h.unregister(c)
			c.ws.CloseNow()
		}
	}
}

// handleWS accepts a WebSocket handshake for GET /ws?project=<id>&token=<api_key>.
// Browsers cannot set an Authorization header on a WebSocket upgrade
// request, so auth here is a query-string token validated against the same
// agentlink:api_key:<sha256> lookup authMiddleware uses for Bearer keys.
// The 401 is written before websocket.Accept so a rejected dial surfaces a
// plain HTTP 401 handshake rather than an upgraded connection.
func (h *Hub) handleWS(w http.ResponseWriter, r *http.Request) {
	project := r.URL.Query().Get("project")
	if project == "" {
		writeError(w, http.StatusBadRequest, "missing project parameter")
		return
	}

	token := r.URL.Query().Get("token")
	if token == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	sum := sha256.Sum256([]byte(token))
	hashHex := hex.EncodeToString(sum[:])
	_, err := h.rdb.Get(r.Context(), "agentlink:api_key:"+hashHex).Result()
	if err == goredis.Nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}

	c := &conn{ws: ws, project: project}
	h.register(project, c)
	defer func() {
		h.unregister(c)
		ws.CloseNow()
	}()

	for {
		if _, _, err := ws.Read(r.Context()); err != nil {
			return
		}
	}
}
