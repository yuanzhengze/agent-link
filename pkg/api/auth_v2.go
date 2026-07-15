package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/team/agentlink/pkg/auth"
)

type authUserView struct {
	ID                 string `json:"id"`
	Username           string `json:"username"`
	MustChangePassword bool   `json:"must_change_password"`
}

type registerLoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

type deviceLoginRequest struct {
	Username   string `json:"username"`
	Password   string `json:"password"`
	DeviceName string `json:"device_name"`
}

type registerLoginResponse struct {
	User authUserView `json:"user"`
}

type meResponse struct {
	User authUserView `json:"user"`
}

type deviceLoginResponse struct {
	User             authUserView `json:"user"`
	DeviceID         string       `json:"device_id"`
	DeviceCredential string       `json:"device_credential"`
}

func toAuthUserView(user auth.User) authUserView {
	return authUserView{
		ID:                 user.ID,
		Username:           user.Username,
		MustChangePassword: user.MustChangePassword,
	}
}

func decodeAuthJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxAuthBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	return true
}

func (s *Server) handleAuthRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.validatePublicOrigin(w, r) {
		return
	}

	var req registerLoginRequest
	if !decodeAuthJSON(w, r, &req) {
		return
	}

	result, err := s.authService.Register(r.Context(), auth.RegisterInput{
		Username: req.Username,
		Password: req.Password,
	})
	if err != nil {
		s.writeAuthServiceError(w, err)
		return
	}
	s.setWebAuthCookies(w, result.SessionSecret, result.CSRFSecret)
	writeJSON(w, http.StatusOK, registerLoginResponse{User: toAuthUserView(result.User)})
}

func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.validatePublicOrigin(w, r) {
		return
	}

	var req registerLoginRequest
	if !decodeAuthJSON(w, r, &req) {
		return
	}

	result, err := s.authService.Login(r.Context(), auth.LoginInput{
		Username: req.Username,
		Password: req.Password,
		IP:       clientIP(r),
	})
	if err != nil {
		s.writeAuthServiceError(w, err)
		return
	}
	s.setWebAuthCookies(w, result.SessionSecret, result.CSRFSecret)
	writeJSON(w, http.StatusOK, registerLoginResponse{User: toAuthUserView(result.User)})
}

func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	sessionSecret, ok := r.Context().Value(contextKeySessionSecret).(string)
	if !ok || sessionSecret == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if err := s.authService.LogoutWeb(r.Context(), sessionSecret); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.clearWebAuthCookies(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAuthMe(w http.ResponseWriter, r *http.Request) {
	source, _ := r.Context().Value(contextKeyAuthSource).(authSource)
	switch source {
	case authSourceWeb:
		sessionSecret, ok := r.Context().Value(contextKeySessionSecret).(string)
		if !ok || sessionSecret == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		user, _, csrfSecret, err := s.authService.RotateWebCSRF(r.Context(), sessionSecret)
		if err != nil {
			if errors.Is(err, auth.ErrSessionExpired) {
				writeError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		s.setCSRFCookie(w, csrfSecret)
		writeJSON(w, http.StatusOK, meResponse{User: toAuthUserView(user)})
	case authSourceDevice:
		sessionSecret, ok := r.Context().Value(contextKeySessionSecret).(string)
		if !ok || sessionSecret == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		user, _, err := s.authService.ResolveDeviceSession(r.Context(), sessionSecret)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		writeJSON(w, http.StatusOK, meResponse{User: toAuthUserView(user)})
	default:
		writeError(w, http.StatusUnauthorized, "unauthorized")
	}
}

func (s *Server) handleAuthChangePassword(w http.ResponseWriter, r *http.Request) {
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req changePasswordRequest
	if !decodeAuthJSON(w, r, &req) {
		return
	}

	if err := s.authService.ChangePassword(r.Context(), actor.UserID, req.CurrentPassword, req.NewPassword); err != nil {
		s.writeAuthServiceError(w, err)
		return
	}

	if source, _ := r.Context().Value(contextKeyAuthSource).(authSource); source == authSourceWeb {
		s.clearWebAuthCookies(w)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeviceLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req deviceLoginRequest
	if !decodeAuthJSON(w, r, &req) {
		return
	}

	result, err := s.authService.DeviceLogin(r.Context(), auth.DeviceLoginInput{
		Username:   req.Username,
		Password:   req.Password,
		IP:         clientIP(r),
		DeviceName: req.DeviceName,
	})
	if err != nil {
		s.writeAuthServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, deviceLoginResponse{
		User:             toAuthUserView(result.User),
		DeviceID:         result.DeviceID,
		DeviceCredential: result.DeviceCredential,
	})
}

func (s *Server) handleDeviceLogout(w http.ResponseWriter, r *http.Request) {
	authHeader := r.Header.Get("Authorization")
	const prefix = "Device "
	if !strings.HasPrefix(authHeader, prefix) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	credential := strings.TrimSpace(authHeader[len(prefix):])
	if credential == "" || !strings.HasPrefix(credential, "ds_") {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if err := s.authService.LogoutDevice(r.Context(), credential); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) writeAuthServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, auth.ErrUsernameExists):
		writeError(w, http.StatusConflict, "username already exists")
	case errors.Is(err, auth.ErrInvalidCredentials):
		writeError(w, http.StatusUnauthorized, "invalid credentials")
	case errors.Is(err, auth.ErrRateLimited):
		writeError(w, http.StatusTooManyRequests, "rate limited")
	default:
		msg := err.Error()
		if strings.Contains(strings.ToLower(msg), "password") && !strings.Contains(msg, "at least") {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if strings.Contains(msg, "username") || strings.Contains(msg, "device name") || strings.Contains(msg, "at least") {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func (s *Server) setWebAuthCookies(w http.ResponseWriter, sessionSecret, csrfSecret string) {
	s.setSessionCookie(w, sessionSecret)
	s.setCSRFCookie(w, csrfSecret)
}

func (s *Server) setSessionCookie(w http.ResponseWriter, sessionSecret string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    sessionSecret,
		Path:     "/",
		MaxAge:   int(auth.WebSessionAbsoluteTTL.Seconds()),
		HttpOnly: true,
		Secure:   s.cookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) setCSRFCookie(w http.ResponseWriter, csrfSecret string) {
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    csrfSecret,
		Path:     "/",
		MaxAge:   int(auth.WebSessionAbsoluteTTL.Seconds()),
		HttpOnly: false,
		Secure:   s.cookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) clearWebAuthCookies(w http.ResponseWriter) {
	for _, name := range []string{sessionCookieName, csrfCookieName} {
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			HttpOnly: name == sessionCookieName,
			Secure:   s.cookieSecure,
			SameSite: http.SameSiteLaxMode,
		})
	}
}
