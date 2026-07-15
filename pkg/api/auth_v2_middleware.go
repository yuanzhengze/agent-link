package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"strings"

	"github.com/team/agentlink/pkg/auth"
)

const (
	sessionCookieName = "al_session"
	csrfCookieName    = "al_csrf"
	maxAuthBodyBytes  = 64 * 1024
)

type authSource string

const (
	authSourceWeb    authSource = "web"
	authSourceDevice authSource = "device"
)

type contextKey int

const (
	contextKeyActor contextKey = iota + 1
	contextKeyAuthSource
	contextKeyWebSession
	contextKeyDeviceSession
	contextKeySessionSecret
)

func ActorFromContext(ctx context.Context) (auth.Actor, bool) {
	actor, ok := ctx.Value(contextKeyActor).(auth.Actor)
	return actor, ok
}

func (s *Server) requireIdentity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, err := s.authenticateRequest(w, r)
		if err != nil {
			return
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) authenticateRequest(w http.ResponseWriter, r *http.Request) (context.Context, error) {
	authHeader := r.Header.Get("Authorization")
	if authHeader != "" {
		return s.authenticateDevice(w, r, authHeader)
	}
	return s.authenticateWeb(w, r)
}

func (s *Server) authenticateDevice(w http.ResponseWriter, r *http.Request, authHeader string) (context.Context, error) {
	const prefix = "Device "
	if !strings.HasPrefix(authHeader, prefix) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return nil, errors.New("unauthorized")
	}
	credential := strings.TrimSpace(authHeader[len(prefix):])
	if credential == "" || !strings.HasPrefix(credential, "ds_") {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return nil, errors.New("unauthorized")
	}

	user, session, err := s.authService.ResolveDeviceSession(r.Context(), credential)
	if err != nil {
		if errors.Is(err, auth.ErrSessionExpired) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return nil, err
		}
		writeError(w, http.StatusInternalServerError, "internal error")
		return nil, err
	}

	actor := auth.Actor{
		UserID:     user.ID,
		Username:   user.Username,
		DeviceID:   session.DeviceID,
		DeviceName: session.DeviceName,
		ClientType: "device",
	}
	ctx := r.Context()
	ctx = context.WithValue(ctx, contextKeyActor, actor)
	ctx = context.WithValue(ctx, contextKeyAuthSource, authSourceDevice)
	ctx = context.WithValue(ctx, contextKeyDeviceSession, session)
	ctx = context.WithValue(ctx, contextKeySessionSecret, credential)

	if err := s.enforceMustChangePassword(w, r, user, actor); err != nil {
		return nil, err
	}
	return ctx, nil
}

func (s *Server) authenticateWeb(w http.ResponseWriter, r *http.Request) (context.Context, error) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return nil, errors.New("unauthorized")
	}

	user, session, err := s.authService.ResolveWebSession(r.Context(), cookie.Value)
	if err != nil {
		if errors.Is(err, auth.ErrSessionExpired) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return nil, err
		}
		writeError(w, http.StatusInternalServerError, "internal error")
		return nil, err
	}

	if isCookieWriteMethod(r.Method) {
		if err := s.validateCSRFForCookieWrite(w, r, session); err != nil {
			return nil, err
		}
	}

	actor := auth.Actor{
		UserID:      user.ID,
		Username:    user.Username,
		DeviceID:    "web",
		DeviceName:  "web",
		SessionName: "gui",
		ClientType:  "web",
	}
	ctx := r.Context()
	ctx = context.WithValue(ctx, contextKeyActor, actor)
	ctx = context.WithValue(ctx, contextKeyAuthSource, authSourceWeb)
	ctx = context.WithValue(ctx, contextKeyWebSession, session)
	ctx = context.WithValue(ctx, contextKeySessionSecret, cookie.Value)

	if err := s.enforceMustChangePassword(w, r, user, actor); err != nil {
		return nil, err
	}
	return ctx, nil
}

func (s *Server) enforceMustChangePassword(w http.ResponseWriter, r *http.Request, user auth.User, actor auth.Actor) error {
	if !user.MustChangePassword {
		return nil
	}
	if isMustChangeAllowedRoute(r.Method, r.URL.Path, actor.ClientType) {
		return nil
	}
	writeError(w, http.StatusForbidden, "password change required")
	return errors.New("password change required")
}

func isMustChangeAllowedRoute(method, path, clientType string) bool {
	switch path {
	case "/api/auth/me":
		return method == http.MethodGet
	case "/api/auth/change-password":
		return method == http.MethodPost
	case "/api/auth/logout":
		return method == http.MethodPost && clientType == "web"
	case "/api/auth/device-logout":
		return method == http.MethodPost && clientType == "device"
	default:
		return false
	}
}

func isCookieWriteMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

func (s *Server) validateCSRFForCookieWrite(w http.ResponseWriter, r *http.Request, session auth.WebSession) error {
	if r.Header.Get("Origin") != s.publicOrigin {
		writeError(w, http.StatusForbidden, "invalid origin")
		return errors.New("invalid origin")
	}
	csrfCookie, err := r.Cookie(csrfCookieName)
	headerToken := r.Header.Get("X-CSRF-Token")
	if err != nil ||
		headerToken == "" ||
		subtle.ConstantTimeCompare([]byte(csrfCookie.Value), []byte(headerToken)) != 1 ||
		sha256Hex(headerToken) != session.CSRFHash {
		writeError(w, http.StatusForbidden, "invalid csrf token")
		return errors.New("invalid csrf token")
	}
	return nil
}

func (s *Server) validatePublicOrigin(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	if origin != s.publicOrigin {
		writeError(w, http.StatusForbidden, "invalid origin")
		return false
	}
	return true
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return strings.TrimSpace(r.RemoteAddr)
	}
	return host
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
