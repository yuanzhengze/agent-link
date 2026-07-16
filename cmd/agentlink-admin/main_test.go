package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	goredis "github.com/redis/go-redis/v9"
	"github.com/team/agentlink/pkg/auth"
	"github.com/team/agentlink/pkg/redis"
)

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

func newTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	rdb, err := redis.NewClient("localhost:6379")
	if err != nil {
		t.Fatalf("connect to test Redis: %v", err)
	}
	t.Cleanup(func() {
		if err := rdb.Close(); err != nil {
			t.Errorf("close test Redis: %v", err)
		}
	})
	return rdb
}

func uniqueTestUsername(t *testing.T) string {
	t.Helper()
	username, err := auth.NewSecret("u", 6)
	if err != nil {
		t.Fatal(err)
	}
	return strings.ToLower(username)
}

func setupUserWithSessions(t *testing.T) (*auth.Service, *auth.Store, *redis.Client, auth.User, string, auth.WebLoginResult, auth.DeviceLoginResult) {
	t.Helper()
	clock := fixedClock{t: time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)}
	rdb := newTestRedis(t)
	store := auth.NewStore(rdb)
	svc := auth.NewService(store, clock)
	username := uniqueTestUsername(t)
	password := "correct horse battery staple"

	result, err := svc.Register(context.Background(), auth.RegisterInput{
		Username: username,
		Password: password,
	})
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	user := result.User

	web, err := svc.Login(context.Background(), auth.LoginInput{
		Username: username,
		Password: password,
		IP:       "203.0.113.1",
	})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}

	device, err := svc.DeviceLogin(context.Background(), auth.DeviceLoginInput{
		Username:   username,
		Password:   password,
		IP:         "203.0.113.2",
		DeviceName: "admin-reset-test",
	})
	if err != nil {
		t.Fatalf("DeviceLogin() error = %v", err)
	}

	return svc, store, rdb, user, username, web, device
}

func parseTemporaryPassword(t *testing.T, stdout string) string {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	if len(lines) != 1 || lines[0] == "" {
		t.Fatalf("stdout = %q; want exactly one line", stdout)
	}
	const prefix = "temporary_password="
	if !strings.HasPrefix(lines[0], prefix) {
		t.Fatalf("stdout line = %q; want temporary_password= prefix", lines[0])
	}
	return strings.TrimPrefix(lines[0], prefix)
}

func assertURLSafe20CharPassword(t *testing.T, password string) {
	t.Helper()
	if len(password) != 20 {
		t.Fatalf("password length = %d; want 20", len(password))
	}
	if _, err := base64.RawURLEncoding.DecodeString(password); err != nil {
		t.Fatalf("password %q is not URL-safe base64: %v", password, err)
	}
}

func assertNoSecretLeak(t *testing.T, output string) {
	t.Helper()
	patterns := []string{
		"$argon2id$",
		"agentlink:v2:",
		"password_phc",
		"ws_",
		"csrf_",
		"ds_",
	}
	for _, pattern := range patterns {
		if strings.Contains(output, pattern) {
			t.Fatalf("output leaks secret-like value %q in %q", pattern, output)
		}
	}
}

func assertPasswordAppearsOnce(t *testing.T, stdout, password string) {
	t.Helper()
	if strings.Count(stdout, password) != 1 {
		t.Fatalf("password appears %d times in stdout %q; want 1", strings.Count(stdout, password), stdout)
	}
}

func TestRunResetPasswordSuccess(t *testing.T) {
	svc, store, rdb, user, username, web, device := setupUserWithSessions(t)
	beforeVersion := user.PasswordVersion
	oldPassword := "correct horse battery staple"

	var stdout, stderr bytes.Buffer
	code := run([]string{"user", "reset-password", username}, &stdout, &stderr, rdb)
	if code != 0 {
		t.Fatalf("run() code = %d stderr = %q; want 0", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q; want empty", stderr.String())
	}

	tempPassword := parseTemporaryPassword(t, stdout.String())
	assertURLSafe20CharPassword(t, tempPassword)
	assertPasswordAppearsOnce(t, stdout.String(), tempPassword)
	assertNoSecretLeak(t, stdout.String())

	stored, err := store.UserByID(context.Background(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.MustChangePassword {
		t.Fatal("must_change_password not set")
	}
	if stored.PasswordVersion != beforeVersion+1 {
		t.Fatalf("password_version = %d; want %d", stored.PasswordVersion, beforeVersion+1)
	}
	if ok, err := auth.VerifyPassword(stored.PasswordPHC, tempPassword); err != nil || !ok {
		t.Fatalf("stored password verify with temp = %v, %v; want true, nil", ok, err)
	}
	if ok, err := auth.VerifyPassword(stored.PasswordPHC, oldPassword); err != nil || ok {
		t.Fatalf("old password still valid = %v, %v; want false", ok, err)
	}

	if _, _, err := svc.ResolveWebSession(context.Background(), web.SessionSecret); !errors.Is(err, auth.ErrSessionExpired) {
		t.Fatalf("web session still valid: %v", err)
	}
	if _, _, err := svc.ResolveDeviceSession(context.Background(), device.DeviceCredential); !errors.Is(err, auth.ErrSessionExpired) {
		t.Fatalf("device session still valid: %v", err)
	}

	loginResult, err := svc.Login(context.Background(), auth.LoginInput{
		Username: username,
		Password: tempPassword,
		IP:       "203.0.113.3",
	})
	if err != nil {
		t.Fatalf("Login() with temp password error = %v", err)
	}
	if !loginResult.User.MustChangePassword {
		t.Fatal("login with temp password must require password change")
	}
}

func TestRunResetPasswordCaseNormalization(t *testing.T) {
	svc, store, rdb, user, username, web, device := setupUserWithSessions(t)
	mixedCase := strings.ToUpper(username[:1]) + username[1:]

	var stdout, stderr bytes.Buffer
	code := run([]string{"user", "reset-password", mixedCase}, &stdout, &stderr, rdb)
	if code != 0 {
		t.Fatalf("run() code = %d stderr = %q; want 0", code, stderr.String())
	}

	tempPassword := parseTemporaryPassword(t, stdout.String())
	stored, err := store.UserByID(context.Background(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.PasswordVersion != user.PasswordVersion+1 {
		t.Fatalf("password_version = %d; want %d", stored.PasswordVersion, user.PasswordVersion+1)
	}
	if _, _, err := svc.ResolveWebSession(context.Background(), web.SessionSecret); !errors.Is(err, auth.ErrSessionExpired) {
		t.Fatalf("web session still valid: %v", err)
	}
	if _, _, err := svc.ResolveDeviceSession(context.Background(), device.DeviceCredential); !errors.Is(err, auth.ErrSessionExpired) {
		t.Fatalf("device session still valid: %v", err)
	}
	loginResult, err := svc.Login(context.Background(), auth.LoginInput{
		Username: mixedCase,
		Password: tempPassword,
		IP:       "203.0.113.4",
	})
	if err != nil {
		t.Fatalf("Login() with mixed-case username error = %v", err)
	}
	if !loginResult.User.MustChangePassword {
		t.Fatal("login after reset must require password change")
	}
}

func TestRunResetPasswordUnknownUser(t *testing.T) {
	rdb := newTestRedis(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"user", "reset-password", "nosuchuser000000000000000000"}, &stdout, &stderr, rdb)
	if code != 1 {
		t.Fatalf("run() code = %d; want 1", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q; want empty on failure", stdout.String())
	}
	errOut := stderr.String()
	if errOut == "" {
		t.Fatal("stderr empty; want user-facing error")
	}
	if !strings.Contains(errOut, "not found") {
		t.Fatalf("stderr = %q; want clear not-found message", errOut)
	}
	assertNoSecretLeak(t, errOut)
}

func TestRunResetPasswordInvalidUsername(t *testing.T) {
	rdb := newTestRedis(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"user", "reset-password", "bad!name"}, &stdout, &stderr, rdb)
	if code != 1 {
		t.Fatalf("run() code = %d; want 1", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q; want empty on failure", stdout.String())
	}
	errOut := stderr.String()
	if errOut == "" {
		t.Fatal("stderr empty; want validation error")
	}
	if !strings.Contains(errOut, "username") {
		t.Fatalf("stderr = %q; want username validation message", errOut)
	}
	assertNoSecretLeak(t, errOut)
}

func TestRunUnknownCommandDoesNotUseRedis(t *testing.T) {
	rdb := unreachableRedisClient(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"bogus-command"}, &stdout, &stderr, rdb)
	if code != 2 {
		t.Fatalf("run() code = %d stderr = %q; want 2 without touching Redis", code, stderr.String())
	}
}

func TestRunMissingResetPasswordArgsDoesNotUseRedis(t *testing.T) {
	rdb := unreachableRedisClient(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"user", "reset-password"}, &stdout, &stderr, rdb)
	if code != 2 {
		t.Fatalf("run() code = %d stderr = %q; want 2 without touching Redis", code, stderr.String())
	}
}

func TestRunResetPasswordAtomicFailureNoStdoutSecret(t *testing.T) {
	_, store, rdb, user, username, _, _ := setupUserWithSessions(t)
	ctx := context.Background()
	if err := rdb.Del(ctx, "agentlink:v2:user:"+user.ID).Err(); err != nil {
		t.Fatal(err)
	}
	before, err := store.UserByID(ctx, user.ID)
	if err == nil {
		t.Fatalf("UserByID() before failure = %+v; want missing user hash", before)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"user", "reset-password", username}, &stdout, &stderr, rdb)
	if code != 1 {
		t.Fatalf("run() code = %d; want 1", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q; want empty on atomic failure", stdout.String())
	}
	assertNoSecretLeak(t, stderr.String())
	if matched, _ := regexp.MatchString(`temporary_password=`, stdout.String()); matched {
		t.Fatalf("stdout leaked temporary_password on failure: %q", stdout.String())
	}
}

func TestRunResetPasswordStdoutFormat(t *testing.T) {
	_, _, rdb, _, username, _, _ := setupUserWithSessions(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"user", "reset-password", username}, &stdout, &stderr, rdb)
	if code != 0 {
		t.Fatalf("run() code = %d; want 0", code)
	}
	if !strings.HasSuffix(stdout.String(), "\n") {
		t.Fatalf("stdout = %q; want trailing newline", stdout.String())
	}
	if utf8.RuneCountInString(strings.TrimSuffix(stdout.String(), "\n")) == 0 {
		t.Fatal("stdout empty after trim")
	}
}

func unreachableRedisClient(t *testing.T) *redis.Client {
	t.Helper()
	inner := goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { _ = inner.Close() })
	return &redis.Client{Client: inner}
}
