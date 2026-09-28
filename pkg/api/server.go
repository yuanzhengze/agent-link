package api

import (
	"context"
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

type ServerOptions struct {
	Addr             string
	DataDir          string
	Redis            *redis.Client
	CookieSecure     bool
	PublicURL        string
	PreviewPublicURL string
}

type Server struct {
	rdb                   *redis.Client
	dataDir               string
	mux                   *http.ServeMux
	srv                   *http.Server
	hubV2                 *HubV2
	projMu                sync.Map
	authService           *auth.Service
	cookieSecure          bool
	publicOrigin          string
	previewOrigin         string
	projectIDGenerator    func() string
	projectTokenGenerator func() (string, error)
	projectGitInitializer func(string) (string, error)
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

	isLoopback := isLoopbackHost(u.Hostname())
	if cookieSecure && u.Scheme != "https" {
		return "", fmt.Errorf("invalid cookie transport: secure cookies require HTTPS")
	}
	if !isLoopback && (u.Scheme != "https" || !cookieSecure) {
		return "", fmt.Errorf("invalid cookie transport: non-loopback URLs require HTTPS and secure cookies")
	}

	return u.Scheme + "://" + u.Host, nil
}

func isLoopbackHost(hostname string) bool {
	host := strings.TrimSuffix(strings.ToLower(hostname), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(hostname)
	return ip != nil && ip.IsLoopback()
}

func NewWithOptions(opts ServerOptions) *Server {
	publicOrigin, err := parsePublicOrigin(opts.PublicURL, opts.CookieSecure)
	if err != nil {
		panic(err)
	}
	previewOrigin := ""
	if strings.TrimSpace(opts.PreviewPublicURL) != "" {
		previewOrigin, err = parsePublicOrigin(opts.PreviewPublicURL, opts.CookieSecure)
		if err != nil {
			panic(fmt.Errorf("invalid preview URL: %w", err))
		}
		if previewOrigin == publicOrigin {
			panic("invalid preview URL: PREVIEW_PUBLIC_URL must be a different origin from PUBLIC_URL (set PREVIEW_PUBLIC_URL=off to keep same-origin preview)")
		}
	}

	store := auth.NewStore(opts.Redis)
	authService := auth.NewService(store, realClock{})

	s := &Server{
		rdb:                   opts.Redis,
		dataDir:               opts.DataDir,
		mux:                   http.NewServeMux(),
		authService:           authService,
		cookieSecure:          opts.CookieSecure,
		publicOrigin:          publicOrigin,
		previewOrigin:         previewOrigin,
		projectIDGenerator:    generateID,
		projectTokenGenerator: generateProjectCreationTokenV2,
		projectGitInitializer: initializeProjectGitV2,
	}

	s.mux.HandleFunc("GET /health", s.handleHealth)

	s.mux.HandleFunc("POST /api/auth/register", s.handleAuthRegister)
	s.mux.HandleFunc("POST /api/auth/login", s.handleAuthLogin)
	s.mux.Handle("POST /api/auth/logout", s.requireIdentity(http.HandlerFunc(s.handleAuthLogout)))
	s.mux.Handle("GET /api/auth/me", s.requireIdentity(http.HandlerFunc(s.handleAuthMe)))
	s.mux.Handle("POST /api/auth/change-password", s.requireIdentity(http.HandlerFunc(s.handleAuthChangePassword)))
	s.mux.HandleFunc("POST /api/auth/device-login", s.handleDeviceLogin)
	s.mux.HandleFunc("POST /api/auth/device-logout", s.handleDeviceLogout)

	s.mux.Handle("GET /api/teams", s.requireIdentity(http.HandlerFunc(s.handleListTeams)))
	s.mux.Handle("POST /api/teams", s.requireIdentity(http.HandlerFunc(s.handleCreateTeam)))
	s.mux.Handle("POST /api/teams/join", s.requireIdentity(http.HandlerFunc(s.handleJoinTeam)))
	s.mux.Handle("POST /api/teams/{team_id}/projects", s.requireIdentity(s.requireTeamRole()(http.HandlerFunc(s.handleCreateProjectV2))))
	s.mux.Handle("GET /api/teams/{team_id}/projects", s.requireIdentity(s.requireTeamRole()(http.HandlerFunc(s.handleListProjectsV2))))
	s.mux.Handle("GET /api/teams/{team_id}", s.requireIdentity(s.requireTeamRole()(http.HandlerFunc(s.handleGetTeam))))
	s.mux.Handle("GET /api/teams/{team_id}/members", s.requireIdentity(s.requireTeamRole()(http.HandlerFunc(s.handleListTeamMembers))))
	s.mux.Handle("POST /api/teams/{team_id}/invite/rotate", s.requireIdentity(s.requireTeamRole(auth.RoleOwner, auth.RoleAdmin)(http.HandlerFunc(s.handleRotateTeamInvite))))
	s.mux.Handle("PATCH /api/teams/{team_id}/members/{user_id}", s.requireIdentity(s.requireTeamRole(auth.RoleOwner)(http.HandlerFunc(s.handleChangeTeamRole))))
	s.mux.Handle("DELETE /api/teams/{team_id}/members/{user_id}", s.requireIdentity(s.requireTeamRole(auth.RoleOwner, auth.RoleAdmin)(http.HandlerFunc(s.handleRemoveTeamMember))))
	s.mux.Handle("POST /api/teams/{team_id}/transfer-owner", s.requireIdentity(s.requireTeamRole(auth.RoleOwner)(http.HandlerFunc(s.handleTransferTeamOwner))))
	s.mux.Handle("POST /api/teams/{team_id}/leave", s.requireIdentity(s.requireTeamRole()(http.HandlerFunc(s.handleLeaveTeam))))
	s.mux.Handle("POST /api/teams/{team_id}/locks/acquire", s.requireIdentity(s.requireTeamRole()(s.requireActorSession(http.HandlerFunc(s.handleLockAcquireV2)))))
	s.mux.Handle("POST /api/teams/{team_id}/locks/release", s.requireIdentity(s.requireTeamRole()(s.requireActorSession(http.HandlerFunc(s.handleLockReleaseV2)))))
	s.mux.Handle("GET /api/teams/{team_id}/locks", s.requireIdentity(s.requireTeamRole()(http.HandlerFunc(s.handleLockListV2))))
	s.mux.Handle("POST /api/teams/{team_id}/projects/{project_id}/apply", s.requireIdentity(s.requireTeamRole()(s.requireActorSession(http.HandlerFunc(s.handleApplyV2)))))
	s.mux.Handle("GET /api/teams/{team_id}/projects/{project_id}/tree", s.requireIdentity(s.requireTeamRole()(http.HandlerFunc(s.handleTreeV2))))
	s.mux.Handle("GET /api/teams/{team_id}/projects/{project_id}/snapshot", s.requireIdentity(s.requireTeamRole()(http.HandlerFunc(s.handleSnapshotV2))))
	s.mux.Handle("POST /api/teams/{team_id}/agents/heartbeat", s.requireIdentity(s.requireTeamRole()(http.HandlerFunc(s.handleHeartbeatV2))))
	s.mux.Handle("GET /api/teams/{team_id}/agents", s.requireIdentity(s.requireTeamRole()(http.HandlerFunc(s.handleListAgentsV2))))
	s.mux.Handle("PATCH /api/teams/{team_id}/agents/sessions", s.requireIdentity(s.requireTeamRole()(http.HandlerFunc(s.handlePatchSessionsV2))))
	s.mux.Handle("DELETE /api/teams/{team_id}/agents/sessions", s.requireIdentity(s.requireTeamRole()(http.HandlerFunc(s.handleDeleteSessionV2))))

	s.mux.Handle("POST /api/teams/{team_id}/messages", s.requireIdentity(s.requireTeamRole()(s.requireActorSession(http.HandlerFunc(s.handleSendV2)))))
	s.mux.Handle("GET /api/teams/{team_id}/inbox", s.requireIdentity(s.requireTeamRole()(s.requireActorSession(http.HandlerFunc(s.handlePullV2)))))
	s.mux.Handle("POST /api/teams/{team_id}/tasks", s.requireIdentity(s.requireTeamRole()(s.requireActorSession(http.HandlerFunc(s.handleSendTaskV2)))))
	s.mux.Handle("GET /api/teams/{team_id}/tasks", s.requireIdentity(s.requireTeamRole()(s.requireActorSession(http.HandlerFunc(s.handleTaskListV2)))))
	s.mux.Handle("GET /api/teams/{team_id}/tasks/{task_id}", s.requireIdentity(s.requireTeamRole()(http.HandlerFunc(s.handleTaskStatusV2))))
	s.mux.Handle("POST /api/teams/{team_id}/tasks/{task_id}/result", s.requireIdentity(s.requireTeamRole()(http.HandlerFunc(s.handleTaskResultV2))))
	s.mux.Handle("POST /api/teams/{team_id}/tasks/{task_id}/resume", s.requireIdentity(s.requireTeamRole()(http.HandlerFunc(s.handleTaskResumeV2))))
	s.mux.Handle("POST /api/teams/{team_id}/tasks/{task_id}/cancel", s.requireIdentity(s.requireTeamRole()(http.HandlerFunc(s.handleTaskCancelV2))))
	s.mux.Handle("POST /api/teams/{team_id}/tasks/{task_id}/reopen", s.requireIdentity(s.requireTeamRole()(http.HandlerFunc(s.handleTaskReopenV2))))

	s.hubV2 = NewHubV2()
	s.mux.Handle("GET /api/teams/{team_id}/ws", s.requireIdentity(s.requireTeamRole()(http.HandlerFunc(s.handleWSV2))))
	s.mux.Handle("POST /api/teams/{team_id}/projects/{project_id}/preview-grant", s.requireIdentity(s.requireTeamRole()(http.HandlerFunc(s.handleIssuePreviewGrant))))
	// Team-scoped preview: authenticated team members only. This is the canonical
	// form now that the v1 /preview/{id} route (which the transitional
	// /preview/teams/... form was introduced to avoid shadowing) is gone.
	// When a distinct preview origin is configured, member HTML is served only
	// from the grant routes below, on that origin. The cookie route stays for
	// same-origin deployments and returns 404 once isolation is on.
	s.mux.HandleFunc("GET /preview/{team_id}/{project_id}/g/{grant}/ws", s.handlePreviewGrantWS)
	s.mux.HandleFunc("GET /preview/{team_id}/{project_id}/g/{grant}/{$}", s.handlePreviewGrant)
	s.mux.HandleFunc("GET /preview/{team_id}/{project_id}/g/{grant}/{path...}", s.handlePreviewGrant)
	s.mux.HandleFunc("GET /preview/{team_id}/{project_id}/{path...}", s.handlePreviewV2)

	s.mux.Handle("GET /", http.FileServer(http.FS(web.FS)))

	return s
}

func New(addr, dataDir string, rdb *redis.Client) *Server {
	publicURL := "http://localhost:8080"
	if addr != "" && strings.HasPrefix(addr, ":") {
		publicURL = "http://localhost" + addr
	}
	return NewWithOptions(ServerOptions{
		Addr:         addr,
		DataDir:      dataDir,
		Redis:        rdb,
		CookieSecure: false,
		PublicURL:    publicURL,
	})
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// Handler returns the server's HTTP handler. It is exposed so out-of-package
// integration tests can mount the full v2 surface with httptest; production
// serving goes through ListenAndServe.
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) ListenAndServe(addr string) error {
	s.srv = &http.Server{
		Addr:    addr,
		Handler: s.mux,
	}
	fmt.Printf("API server listening on %s\n", addr)
	if s.previewIsolated() {
		fmt.Printf("preview origin %s (read-only, separate from %s)\n", s.previewOrigin, s.publicOrigin)
		fmt.Printf("open the GUI at %s — preview HTML is not served on that host\n", s.publicOrigin)
	}
	return s.srv.ListenAndServe()
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.srv.Shutdown(ctx)
}
