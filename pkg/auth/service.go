package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
)

const dummyLoginPassword = "timing-safe-dummy-password"

var fixedDummyLoginPHC = buildFixedDummyLoginPHC()

type Clock interface {
	Now() time.Time
}

type RegisterInput struct {
	Username string
	Password string
}

type LoginInput struct {
	Username string
	Password string
	IP       string
}

type WebLoginResult struct {
	User          User
	SessionSecret string
	CSRFSecret    string
}

type DeviceLoginInput struct {
	Username   string
	Password   string
	IP         string
	DeviceName string
}

type DeviceLoginResult struct {
	User             User
	DeviceID         string
	DeviceCredential string
}

type Service struct {
	store    *Store
	clock    Clock
	dummyPHC string
}

func NewService(store *Store, clock Clock) *Service {
	return &Service{store: store, clock: clock, dummyPHC: fixedDummyLoginPHC}
}

func (s *Service) Register(ctx context.Context, input RegisterInput) (WebLoginResult, error) {
	if _, err := NormalizeUsername(input.Username); err != nil {
		return WebLoginResult{}, err
	}
	passwordPHC, err := HashPassword(input.Password)
	if err != nil {
		return WebLoginResult{}, err
	}
	userID, err := NewUserID()
	if err != nil {
		return WebLoginResult{}, err
	}
	now := s.clock.Now().UTC().Truncate(time.Microsecond)
	user := User{
		ID:              userID,
		Username:        input.Username,
		PasswordPHC:     passwordPHC,
		PasswordVersion: 1,
		CreatedAt:       now,
	}
	if err := s.store.CreateUser(ctx, user); err != nil {
		return WebLoginResult{}, err
	}
	created, err := s.store.UserByID(ctx, userID)
	if err != nil {
		return WebLoginResult{}, err
	}
	return s.issueWebSession(ctx, created)
}

func (s *Service) Login(ctx context.Context, input LoginInput) (WebLoginResult, error) {
	user, err := s.authenticateLogin(ctx, input.Username, input.Password, input.IP)
	if err != nil {
		return WebLoginResult{}, err
	}
	return s.issueWebSession(ctx, user)
}

func (s *Service) ResolveWebSession(ctx context.Context, sessionSecret string) (User, WebSession, error) {
	sessionHash := SecretHash(sessionSecret)
	session, err := s.store.ResolveWebSession(ctx, sessionHash, s.clock.Now().UTC())
	if err != nil {
		return User{}, WebSession{}, err
	}
	user, err := s.store.UserByID(ctx, session.UserID)
	if err != nil {
		return User{}, WebSession{}, err
	}
	return user, session, nil
}

func (s *Service) LogoutWeb(ctx context.Context, sessionSecret string) error {
	return s.store.RevokeWebSession(ctx, SecretHash(sessionSecret))
}

func (s *Service) DeviceLogin(ctx context.Context, input DeviceLoginInput) (DeviceLoginResult, error) {
	user, err := s.authenticateLogin(ctx, input.Username, input.Password, input.IP)
	if err != nil {
		return DeviceLoginResult{}, err
	}
	deviceName := strings.TrimSpace(input.DeviceName)
	if deviceName == "" {
		return DeviceLoginResult{}, errors.New("device name is required")
	}

	deviceID, err := NewDeviceID()
	if err != nil {
		return DeviceLoginResult{}, err
	}
	credential, err := NewSecret("ds_", 32)
	if err != nil {
		return DeviceLoginResult{}, err
	}
	sessionHash := SecretHash(credential)
	now := s.clock.Now().UTC().Truncate(time.Millisecond)
	device := Device{
		ID:          deviceID,
		UserID:      user.ID,
		Name:        deviceName,
		SessionHash: sessionHash,
		CreatedAt:   now,
		LastSeenAt:  now,
	}
	if err := s.store.CreateDeviceCredential(ctx, device, sessionHash, DeviceSession{
		UserID:          user.ID,
		DeviceID:        deviceID,
		PasswordVersion: user.PasswordVersion,
		CreatedAt:       now,
		LastSeenAt:      now,
	}); err != nil {
		return DeviceLoginResult{}, err
	}
	return DeviceLoginResult{
		User:             user,
		DeviceID:         deviceID,
		DeviceCredential: credential,
	}, nil
}

func (s *Service) ResolveDeviceSession(ctx context.Context, credential string) (User, DeviceSession, error) {
	sessionHash := SecretHash(credential)
	session, err := s.store.ResolveDeviceSession(ctx, sessionHash, s.clock.Now().UTC())
	if err != nil {
		return User{}, DeviceSession{}, err
	}
	user, err := s.store.UserByID(ctx, session.UserID)
	if err != nil {
		return User{}, DeviceSession{}, err
	}
	return user, session, nil
}

func (s *Service) LogoutDevice(ctx context.Context, credential string) error {
	return s.store.RevokeDeviceCredential(ctx, SecretHash(credential))
}

func (s *Service) ChangePassword(ctx context.Context, userID, currentPassword, newPassword string) error {
	user, err := s.store.UserByID(ctx, userID)
	if err != nil {
		return err
	}
	if err := s.verifyUserPassword(user, currentPassword); err != nil {
		return err
	}
	passwordPHC, err := HashPassword(newPassword)
	if err != nil {
		return err
	}
	return s.store.UpdateUserPasswordAndRevokeSessions(
		ctx,
		userID,
		passwordPHC,
		false,
	)
}

func (s *Service) ResetPassword(ctx context.Context, username string) (string, error) {
	user, err := s.store.UserByUsername(ctx, username)
	if err != nil {
		return "", err
	}
	tempPassword, err := generateResetPassword()
	if err != nil {
		return "", err
	}
	passwordPHC, err := HashPassword(tempPassword)
	if err != nil {
		return "", err
	}
	if err := s.store.UpdateUserPasswordAndRevokeSessions(
		ctx,
		user.ID,
		passwordPHC,
		true,
	); err != nil {
		return "", err
	}
	return tempPassword, nil
}

func (s *Service) issueWebSession(ctx context.Context, user User) (WebLoginResult, error) {
	sessionSecret, err := NewSecret("ws_", 32)
	if err != nil {
		return WebLoginResult{}, err
	}
	csrfSecret, err := NewSecret("csrf_", 32)
	if err != nil {
		return WebLoginResult{}, err
	}
	now := s.clock.Now().UTC().Truncate(time.Millisecond)
	absoluteExpiry := now.Add(WebSessionAbsoluteTTL)
	if err := s.store.CreateWebSession(ctx, SecretHash(sessionSecret), WebSession{
		UserID:            user.ID,
		CSRFHash:          SecretHash(csrfSecret),
		PasswordVersion:   user.PasswordVersion,
		CreatedAt:         now,
		LastSeenAt:        now,
		AbsoluteExpiresAt: absoluteExpiry,
	}); err != nil {
		return WebLoginResult{}, err
	}
	return WebLoginResult{
		User:          user,
		SessionSecret: sessionSecret,
		CSRFSecret:    csrfSecret,
	}, nil
}

func (s *Service) authenticateLogin(ctx context.Context, username, password, ip string) (User, error) {
	trimmedIP := strings.TrimSpace(ip)
	if trimmedIP == "" {
		return User{}, errors.New("ip is required")
	}
	normalized, err := NormalizeUsername(username)
	if err != nil {
		return User{}, err
	}
	if err := s.ensureNotRateLimited(ctx, normalized, trimmedIP); err != nil {
		return User{}, err
	}

	user, err := s.store.UserByUsername(ctx, username)
	unknown := errors.Is(err, ErrNotFound)
	if err != nil {
		if !unknown {
			return User{}, err
		}
	}

	passwordPHC := user.PasswordPHC
	if unknown {
		passwordPHC = s.dummyPHC
	}
	passwordMatches, err := VerifyPassword(passwordPHC, password)
	if err != nil {
		return User{}, err
	}
	if unknown || user.Status != "active" || !passwordMatches {
		return User{}, s.recordLoginFailure(ctx, normalized, trimmedIP)
	}
	if err := s.store.ClearLoginRateLimit(ctx, normalized, trimmedIP); err != nil {
		return User{}, err
	}
	return user, nil
}

func (s *Service) ensureNotRateLimited(ctx context.Context, normalized, ip string) error {
	userCount, ipCount, err := s.store.GetLoginFailureCounts(ctx, normalized, ip)
	if err != nil {
		return err
	}
	if userCount >= loginFailureMaxFailures || ipCount >= loginFailureMaxFailures {
		return ErrRateLimited
	}
	return nil
}

func (s *Service) recordLoginFailure(ctx context.Context, normalized, ip string) error {
	userCount, ipCount, err := s.store.RecordLoginFailure(ctx, normalized, ip)
	if err != nil {
		return err
	}
	if userCount >= loginFailureMaxFailures || ipCount >= loginFailureMaxFailures {
		return ErrRateLimited
	}
	return ErrInvalidCredentials
}

func (s *Service) verifyUserPassword(user User, password string) error {
	ok, err := VerifyPassword(user.PasswordPHC, password)
	if err != nil {
		return err
	}
	if !ok {
		return ErrInvalidCredentials
	}
	return nil
}

func generateResetPassword() (string, error) {
	value := make([]byte, 15)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate reset password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func buildFixedDummyLoginPHC() string {
	salt := []byte("agentlink-dummy!")
	hash := argon2.IDKey(
		[]byte(dummyLoginPassword),
		salt,
		argonIterations,
		argonMemory,
		argonParallelism,
		argonKeyLength,
	)
	return fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version,
		argonMemory,
		argonIterations,
		argonParallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash),
	)
}
