package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"reflect"
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

type closeFunc func() error

func (f closeFunc) Close() error { return f() }

type recordingWriter struct {
	writes int
	data   bytes.Buffer
}

func (w *recordingWriter) Write(p []byte) (int, error) {
	w.writes++
	return w.data.Write(p)
}

func (w *recordingWriter) String() string { return w.data.String() }

type controlledFailureWriter struct {
	writes    int
	attempted string
	short     bool
}

func (w *controlledFailureWriter) Write(p []byte) (int, error) {
	w.writes++
	w.attempted = string(p)
	if w.short {
		return len(p) - 1, nil
	}
	return 0, errors.New("controlled stdout failure")
}

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

	var stdout recordingWriter
	var stderr bytes.Buffer
	code := run([]string{"user", "reset-password", username}, &stdout, &stderr, rdb)
	if code != 0 {
		t.Fatalf("run() code = %d stderr = %q; want 0", code, stderr.String())
	}
	if stdout.writes != 1 {
		t.Fatalf("stdout writes = %d; want exactly 1", stdout.writes)
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

func TestRunResetPasswordWriteErrorReturnsPartialSuccess(t *testing.T) {
	assertPasswordDeliveryFailure(t, &controlledFailureWriter{})
}

func TestRunResetPasswordShortWriteReturnsPartialSuccess(t *testing.T) {
	assertPasswordDeliveryFailure(t, &controlledFailureWriter{short: true})
}

func assertPasswordDeliveryFailure(t *testing.T, stdout *controlledFailureWriter) {
	t.Helper()
	svc, store, rdb, user, username, web, device := setupUserWithSessions(t)
	before, err := store.UserByID(context.Background(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer

	code := run([]string{"user", "reset-password", username}, stdout, &stderr, rdb)

	if code != 3 {
		t.Fatalf("run() code = %d stderr = %q; want password-delivery exit 3", code, stderr.String())
	}
	if stdout.writes != 1 {
		t.Fatalf("stdout writes = %d; want exactly 1 without retry", stdout.writes)
	}
	tempPassword := parseTemporaryPassword(t, stdout.attempted)
	if !strings.Contains(stderr.String(), "password was reset but temporary password delivery failed") ||
		!strings.Contains(stderr.String(), "DO NOT RETRY AUTOMATICALLY") {
		t.Fatalf("stderr = %q; want partial-success and no-retry warning", stderr.String())
	}
	if strings.Contains(stderr.String(), tempPassword) ||
		strings.Contains(stderr.String(), "temporary_password") {
		t.Fatalf("stderr leaked temporary password: %q", stderr.String())
	}
	assertNoSecretLeak(t, stderr.String())

	after, err := store.UserByID(context.Background(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.PasswordVersion != before.PasswordVersion+1 {
		t.Fatalf("password_version = %d; want exactly %d", after.PasswordVersion, before.PasswordVersion+1)
	}
	if !after.MustChangePassword {
		t.Fatal("must_change_password not set after delivery failure")
	}
	if after.PasswordPHC == before.PasswordPHC {
		t.Fatal("password hash did not change before delivery failure")
	}
	if ok, err := auth.VerifyPassword(after.PasswordPHC, tempPassword); err != nil || !ok {
		t.Fatalf("temporary password verify = %v, %v; want true, nil", ok, err)
	}
	if ok, err := auth.VerifyPassword(after.PasswordPHC, "correct horse battery staple"); err != nil || ok {
		t.Fatalf("old password verify = %v, %v; want false, nil", ok, err)
	}
	if _, _, err := svc.ResolveWebSession(context.Background(), web.SessionSecret); !errors.Is(err, auth.ErrSessionExpired) {
		t.Fatalf("web session still valid after delivery failure: %v", err)
	}
	if _, _, err := svc.ResolveDeviceSession(context.Background(), device.DeviceCredential); !errors.Is(err, auth.ErrSessionExpired) {
		t.Fatalf("device session still valid after delivery failure: %v", err)
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

func TestRunUnknownUserSubcommandDoesNotUseRedis(t *testing.T) {
	rdb := unreachableRedisClient(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"user", "unknown-subcommand"}, &stdout, &stderr, rdb)
	if code != 2 {
		t.Fatalf("run() code = %d stderr = %q; want 2 without touching Redis", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Usage:") {
		t.Fatalf("stderr = %q; want usage", stderr.String())
	}
}

func TestExecuteRejectsInvalidCommandBeforeConnectingRedis(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "no command"},
		{name: "unknown command", args: []string{"unknown"}},
		{name: "missing username", args: []string{"user", "reset-password"}},
		{name: "unknown user subcommand", args: []string{"user", "unknown-subcommand"}},
		{name: "extra argument", args: []string{"user", "reset-password", "kirby", "extra"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var connectCalls int
			connect := func(string) (*redis.Client, io.Closer, error) {
				connectCalls++
				return nil, nil, errors.New("must not connect")
			}
			getenv := func(string) string { return "127.0.0.1:1" }
			var stdout, stderr bytes.Buffer

			code := execute(tt.args, &stdout, &stderr, getenv, connect)

			if code != 2 {
				t.Fatalf("execute() code = %d stderr = %q; want 2", code, stderr.String())
			}
			if connectCalls != 0 {
				t.Fatalf("Redis connect calls = %d; want 0", connectCalls)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q; want empty", stdout.String())
			}
			if !strings.Contains(stderr.String(), "Usage:") {
				t.Fatalf("stderr = %q; want usage", stderr.String())
			}
		})
	}
}

func TestExecuteCloseFailureWarnsWithoutChangingSuccessCode(t *testing.T) {
	_, _, rdb, _, username, _, _ := setupUserWithSessions(t)
	var closeCalls int
	connect := func(addr string) (*redis.Client, io.Closer, error) {
		if addr != "localhost:6379" {
			t.Fatalf("connect address = %q; want default localhost:6379", addr)
		}
		return rdb, closeFunc(func() error {
			closeCalls++
			return errors.New("controlled close failure")
		}), nil
	}
	var stdout, stderr bytes.Buffer

	code := execute(
		[]string{"user", "reset-password", username},
		&stdout,
		&stderr,
		func(string) string { return "" },
		connect,
	)

	if code != 0 {
		t.Fatalf("execute() code = %d stderr = %q; want 0", code, stderr.String())
	}
	if closeCalls != 1 {
		t.Fatalf("close calls = %d; want 1", closeCalls)
	}
	tempPassword := parseTemporaryPassword(t, stdout.String())
	assertPasswordAppearsOnce(t, stdout.String(), tempPassword)
	if !strings.Contains(stderr.String(), "warning") {
		t.Fatalf("stderr = %q; want close warning", stderr.String())
	}
	if strings.Contains(stderr.String(), tempPassword) ||
		strings.Contains(stderr.String(), "temporary_password") {
		t.Fatalf("close warning leaked temporary password: %q", stderr.String())
	}
	assertNoSecretLeak(t, stderr.String())
}

func TestExecuteCloseFailurePreservesRunFailureCode(t *testing.T) {
	rdb := newTestRedis(t)
	connect := func(string) (*redis.Client, io.Closer, error) {
		return rdb, closeFunc(func() error {
			return errors.New("controlled close failure")
		}), nil
	}
	var stdout, stderr bytes.Buffer

	code := execute(
		[]string{"user", "reset-password", "bad!name"},
		&stdout,
		&stderr,
		func(string) string { return "localhost:6379" },
		connect,
	)

	if code != 1 {
		t.Fatalf("execute() code = %d stderr = %q; want original run code 1", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q; want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), "username") ||
		!strings.Contains(stderr.String(), "warning") {
		t.Fatalf("stderr = %q; want validation error and close warning", stderr.String())
	}
	assertNoSecretLeak(t, stderr.String())
}

func TestExecuteForwardsConfiguredRedisAddr(t *testing.T) {
	_, _, rdb, _, username, _, _ := setupUserWithSessions(t)
	const configuredAddr = "redis.internal.example:6380"
	var connectedAddr string
	connect := func(addr string) (*redis.Client, io.Closer, error) {
		connectedAddr = addr
		return rdb, closeFunc(func() error { return nil }), nil
	}
	var stdout, stderr bytes.Buffer

	code := execute(
		[]string{"user", "reset-password", username},
		&stdout,
		&stderr,
		func(key string) string {
			if key != "REDIS_ADDR" {
				t.Fatalf("getenv key = %q; want REDIS_ADDR", key)
			}
			return configuredAddr
		},
		connect,
	)

	if code != 0 {
		t.Fatalf("execute() code = %d stderr = %q; want 0", code, stderr.String())
	}
	if connectedAddr != configuredAddr {
		t.Fatalf("connected address = %q; want %q", connectedAddr, configuredAddr)
	}
}

func TestExecuteConnectionFactoryErrorIsSafe(t *testing.T) {
	_, store, _, user, username, web, device := setupUserWithSessions(t)
	ctx := context.Background()
	beforeUser, err := store.UserByID(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	leakedValues := []string{
		beforeUser.PasswordPHC,
		"agentlink:v2:user:" + user.ID,
		web.SessionSecret,
		device.DeviceCredential,
	}
	var connectCalls int
	connect := func(string) (*redis.Client, io.Closer, error) {
		connectCalls++
		return nil, nil, errors.New(strings.Join(leakedValues, " "))
	}
	var stdout, stderr bytes.Buffer

	code := execute(
		[]string{"user", "reset-password", username},
		&stdout,
		&stderr,
		func(string) string { return "redis.invalid:6399" },
		connect,
	)

	if code != 1 {
		t.Fatalf("execute() code = %d; want 1", code)
	}
	if connectCalls != 1 {
		t.Fatalf("connect calls = %d; want 1", connectCalls)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q; want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), "redis connection failed") {
		t.Fatalf("stderr = %q; want connection failure", stderr.String())
	}
	for _, leaked := range leakedValues {
		if strings.Contains(stderr.String(), leaked) {
			t.Fatalf("stderr leaked %q: %q", leaked, stderr.String())
		}
	}
	assertNoSecretLeak(t, stderr.String())

	afterUser, err := store.UserByID(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterUser, beforeUser) {
		t.Fatalf("user changed after connection failure: before=%+v after=%+v", beforeUser, afterUser)
	}
}

func TestRunResetPasswordLuaPreflightFailureIsAtomic(t *testing.T) {
	_, store, rdb, user, username, web, device := setupUserWithSessions(t)
	ctx := context.Background()
	beforeUser, err := store.UserByID(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	webHash := auth.SecretHash(web.SessionSecret)
	deviceHash := auth.SecretHash(device.DeviceCredential)
	webSessionKey := "agentlink:v2:web_session:" + webHash
	deviceSessionKey := "agentlink:v2:device_session:" + deviceHash
	deviceBindingKey := "agentlink:v2:device_credential:" + deviceHash
	deviceKey := "agentlink:v2:device:" + device.DeviceID
	userKey := "agentlink:v2:user:" + user.ID
	webIndexKey := userKey + ":web_sessions"
	deviceIndexKey := userKey + ":device_sessions"
	devicesIndexKey := userKey + ":devices"

	webSessionBefore := redisHash(t, rdb, webSessionKey)
	deviceSessionBefore := redisHash(t, rdb, deviceSessionKey)
	deviceBindingBefore := redisHash(t, rdb, deviceBindingKey)
	deviceBefore := redisHash(t, rdb, deviceKey)
	deviceIndexBefore := redisZSet(t, rdb, deviceIndexKey)
	devicesIndexBefore := redisSet(t, rdb, devicesIndexKey)

	if err := rdb.Del(ctx, webIndexKey).Err(); err != nil {
		t.Fatal(err)
	}
	const wrongTypeValue = "controlled-wrong-type"
	if err := rdb.Set(ctx, webIndexKey, wrongTypeValue, 0).Err(); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"user", "reset-password", username}, &stdout, &stderr, rdb)
	if code != 1 {
		t.Fatalf("run() code = %d; want 1", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q; want empty on atomic failure", stdout.String())
	}
	if got := stderr.String(); got != "reset password failed; check host and Redis health\n" {
		t.Fatalf("stderr = %q; want generic runtime error", got)
	}
	assertNoSecretLeak(t, stderr.String())

	afterUser, err := store.UserByID(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterUser.PasswordPHC != beforeUser.PasswordPHC ||
		afterUser.PasswordVersion != beforeUser.PasswordVersion ||
		afterUser.MustChangePassword != beforeUser.MustChangePassword {
		t.Fatalf("user password state changed: before=%+v after=%+v", beforeUser, afterUser)
	}
	if ok, err := auth.VerifyPassword(afterUser.PasswordPHC, "correct horse battery staple"); err != nil || !ok {
		t.Fatalf("old password verify = %v, %v; want true, nil", ok, err)
	}
	if got, err := rdb.Get(ctx, webIndexKey).Result(); err != nil || got != wrongTypeValue {
		t.Fatalf("wrong-type web index = %q, %v; want unchanged", got, err)
	}
	if got := redisHash(t, rdb, webSessionKey); !reflect.DeepEqual(got, webSessionBefore) {
		t.Fatalf("web session changed: before=%v after=%v", webSessionBefore, got)
	}
	if got := redisHash(t, rdb, deviceSessionKey); !reflect.DeepEqual(got, deviceSessionBefore) {
		t.Fatalf("device session changed: before=%v after=%v", deviceSessionBefore, got)
	}
	if got := redisHash(t, rdb, deviceBindingKey); !reflect.DeepEqual(got, deviceBindingBefore) {
		t.Fatalf("device binding changed: before=%v after=%v", deviceBindingBefore, got)
	}
	if got := redisHash(t, rdb, deviceKey); !reflect.DeepEqual(got, deviceBefore) {
		t.Fatalf("device changed: before=%v after=%v", deviceBefore, got)
	}
	if got := redisZSet(t, rdb, deviceIndexKey); !reflect.DeepEqual(got, deviceIndexBefore) {
		t.Fatalf("device session index changed: before=%v after=%v", deviceIndexBefore, got)
	}
	if got := redisSet(t, rdb, devicesIndexKey); !reflect.DeepEqual(got, devicesIndexBefore) {
		t.Fatalf("devices index changed: before=%v after=%v", devicesIndexBefore, got)
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

func TestUsageDocumentsPasswordDeliveryFailureExitCode(t *testing.T) {
	var usage bytes.Buffer
	printUsage(&usage)
	if !strings.Contains(usage.String(), "3") ||
		!strings.Contains(usage.String(), "DO NOT RETRY AUTOMATICALLY") {
		t.Fatalf("usage does not document password-delivery exit code 3: %q", usage.String())
	}
}

func unreachableRedisClient(t *testing.T) *redis.Client {
	t.Helper()
	inner := goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { _ = inner.Close() })
	return &redis.Client{Client: inner}
}

func redisHash(t *testing.T, rdb *redis.Client, key string) map[string]string {
	t.Helper()
	value, err := rdb.HGetAll(context.Background(), key).Result()
	if err != nil {
		t.Fatalf("read Redis hash %q: %v", key, err)
	}
	return value
}

func redisZSet(t *testing.T, rdb *redis.Client, key string) []goredis.Z {
	t.Helper()
	value, err := rdb.ZRangeWithScores(context.Background(), key, 0, -1).Result()
	if err != nil {
		t.Fatalf("read Redis zset %q: %v", key, err)
	}
	return value
}

func redisSet(t *testing.T, rdb *redis.Client, key string) []string {
	t.Helper()
	value, err := rdb.SMembers(context.Background(), key).Result()
	if err != nil {
		t.Fatalf("read Redis set %q: %v", key, err)
	}
	return value
}
