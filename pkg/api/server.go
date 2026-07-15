package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/team/agentlink/pkg/auth"
	"github.com/team/agentlink/pkg/redis"
	"github.com/team/agentlink/web"
)

type legacyContextKey string

const (
	contextKeyDevice legacyContextKey = "device"
)

type ServerOptions struct {
	Addr             string
	DataDir          string
	Redis            *redis.Client
	CookieSecure     bool
	PublicURL        string
	RegisterPassword string
}

type Server struct {
	rdb              *redis.Client
	registerPassword string
	dataDir          string
	mux              *http.ServeMux
	srv              *http.Server
	hub              Broadcaster
	projMu           sync.Map
	previewToken     string
	authService      *auth.Service
	cookieSecure     bool
	publicOrigin     string
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

func parsePublicOrigin(raw string, cookieSecure bool) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid public URL: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("invalid public URL: missing scheme or host")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("invalid public URL: unsupported scheme %q", u.Scheme)
	}
	if u.Path != "" && u.Path != "/" {
		return "", fmt.Errorf("invalid public URL: path not allowed")
	}

	hostname := u.Hostname()
	ip := net.ParseIP(hostname)
	isLoopback := strings.EqualFold(hostname, "localhost") || ip != nil && ip.IsLoopback()
	if cookieSecure && u.Scheme != "https" {
		return "", fmt.Errorf("invalid cookie transport: secure cookies require HTTPS")
	}
	if !isLoopback && (u.Scheme != "https" || !cookieSecure) {
		return "", fmt.Errorf("invalid cookie transport: non-loopback URLs require HTTPS and secure cookies")
	}

	return u.Scheme + "://" + u.Host, nil
}

func NewWithOptions(opts ServerOptions) *Server {
	publicOrigin, err := parsePublicOrigin(opts.PublicURL, opts.CookieSecure)
	if err != nil {
		panic(err)
	}

	previewToken, err := generatePreviewToken()
	if err != nil {
		panic(fmt.Sprintf("failed to generate preview token: %v", err))
	}

	store := auth.NewStore(opts.Redis)
	authService := auth.NewService(store, realClock{})

	s := &Server{
		rdb:              opts.Redis,
		registerPassword: opts.RegisterPassword,
		dataDir:          opts.DataDir,
		mux:              http.NewServeMux(),
		previewToken:     previewToken,
		authService:      authService,
		cookieSecure:     opts.CookieSecure,
		publicOrigin:     publicOrigin,
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

	s.mux.HandleFunc("POST /api/auth/register", s.handleAuthRegister)
	s.mux.HandleFunc("POST /api/auth/login", s.handleAuthLogin)
	s.mux.Handle("POST /api/auth/logout", s.requireIdentity(http.HandlerFunc(s.handleAuthLogout)))
	s.mux.Handle("GET /api/auth/me", s.requireIdentity(http.HandlerFunc(s.handleAuthMe)))
	s.mux.Handle("POST /api/auth/change-password", s.requireIdentity(http.HandlerFunc(s.handleAuthChangePassword)))
	s.mux.HandleFunc("POST /api/auth/device-login", s.handleDeviceLogin)
	s.mux.HandleFunc("POST /api/auth/device-logout", s.handleDeviceLogout)

	hub := NewHub(opts.Redis)
	hub.previewToken = s.previewToken
	s.hub = hub
	s.mux.HandleFunc("GET /ws", hub.handleWS)

	s.mux.Handle("GET /", http.FileServer(http.FS(web.FS)))

	return s
}

func New(addr, dataDir string, rdb *redis.Client, registerPassword string) *Server {
	publicURL := "http://localhost:8080"
	if addr != "" && strings.HasPrefix(addr, ":") {
		publicURL = "http://localhost" + addr
	}
	return NewWithOptions(ServerOptions{
		Addr:             addr,
		DataDir:          dataDir,
		Redis:            rdb,
		CookieSecure:     false,
		PublicURL:        publicURL,
		RegisterPassword: registerPassword,
	})
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

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
