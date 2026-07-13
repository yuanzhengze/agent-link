package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"sync"

	"github.com/team/agentlink/pkg/redis"
)

type contextKey string

const (
	contextKeyDevice contextKey = "device"
)

type Server struct {
	rdb              *redis.Client
	registerPassword string
	dataDir          string
	mux              *http.ServeMux
	srv              *http.Server
	hub              Broadcaster // set by Task 4 (WebSocket Hub); nil-guarded until then
	projMu           sync.Map    // projectID -> *sync.Mutex, serializes writes+commits per project
	previewToken     string      // read-only token for unauthenticated preview pages to subscribe over /ws (Task 6)
}

// generatePreviewToken returns a random 32-byte hex-encoded token used to
// let unauthenticated preview pages subscribe to /ws without exposing a
// real (write-capable) api_key in page source.
func generatePreviewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func New(addr, dataDir string, rdb *redis.Client, registerPassword string) *Server {
	previewToken, err := generatePreviewToken()
	if err != nil {
		// crypto/rand failure is unrecoverable; a preview token is required
		// for the live-reload feature to work safely.
		panic(fmt.Sprintf("failed to generate preview token: %v", err))
	}

	s := &Server{
		rdb:              rdb,
		registerPassword: registerPassword,
		dataDir:          dataDir,
		mux:              http.NewServeMux(),
		previewToken:     previewToken,
	}

	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.mux.HandleFunc("POST /agents/register", s.handleRegister)
	s.mux.HandleFunc("POST /projects", s.handleCreateProject)
	s.mux.HandleFunc("GET /projects", s.handleListProjects)
	s.mux.HandleFunc("POST /messages/send", s.handleSend)
	s.mux.HandleFunc("GET /inbox/pull", s.handlePull)
	s.mux.HandleFunc("POST /tasks/send", s.handleSendTask)
	s.mux.HandleFunc("POST /tasks/result", s.handleTaskResult)
	s.mux.HandleFunc("POST /tasks/resume", s.handleTaskResume)
	s.mux.HandleFunc("POST /tasks/cancel", s.handleTaskCancel)
	s.mux.HandleFunc("POST /tasks/reopen", s.handleTaskReopen)
	s.mux.HandleFunc("GET /tasks/status", s.handleTaskStatus)
	s.mux.HandleFunc("GET /tasks/list", s.handleTaskList)
	s.mux.HandleFunc("POST /agents/heartbeat", s.handleHeartbeat)
	s.mux.HandleFunc("GET /agents/list", s.handleList)
	s.mux.HandleFunc("PATCH /agents/sessions", s.handlePatchSessions)
	s.mux.HandleFunc("DELETE /agents/sessions", s.handleDeleteSession)
	s.mux.HandleFunc("DELETE /agents/device", s.handleDeleteDevice)
	s.mux.HandleFunc("GET /whoami", s.handleWhoami)
	s.mux.HandleFunc("POST /locks/acquire", s.handleLockAcquire)
	s.mux.HandleFunc("POST /locks/release", s.handleLockRelease)
	s.mux.HandleFunc("GET /locks/list", s.handleLockList)
	s.mux.HandleFunc("POST /projects/{id}/apply", s.handleApply)
	s.mux.HandleFunc("GET /projects/{id}/tree", s.handleTree)
	s.mux.HandleFunc("GET /projects/{id}/snapshot", s.handleSnapshot)
	s.mux.HandleFunc("GET /preview/{id}/{path...}", s.handlePreview)

	hub := NewHub(rdb)
	hub.previewToken = s.previewToken
	s.hub = hub
	s.mux.HandleFunc("GET /ws", hub.handleWS)

	return s
}

func (s *Server) ListenAndServe(addr string) error {
	s.srv = &http.Server{
		Addr:    addr,
		Handler: s.authMiddleware(s.mux),
	}
	fmt.Printf("API server listening on %s\n", addr)
	return s.srv.ListenAndServe()
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.srv.Shutdown(ctx)
}
