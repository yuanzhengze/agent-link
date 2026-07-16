package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/team/agentlink/pkg/auth"
	"github.com/team/agentlink/pkg/redis"
)

const (
	redisOpTimeout = 30 * time.Second

	// exitPasswordDeliveryFailed means the reset committed but the temporary
	// password could not be reliably delivered. Callers must not retry it
	// automatically because another reset would invalidate that password.
	exitPasswordDeliveryFailed = 3
)

const passwordDeliveryFailedWarning = "password was reset but temporary password delivery failed; DO NOT RETRY AUTOMATICALLY; run a new reset manually"

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

type redisFactory func(addr string) (*redis.Client, io.Closer, error)

func openRedis(addr string) (*redis.Client, io.Closer, error) {
	rdb, err := redis.NewClient(addr)
	if err != nil {
		return nil, nil, err
	}
	return rdb, rdb, nil
}

func main() {
	os.Exit(execute(os.Args[1:], os.Stdout, os.Stderr, os.Getenv, openRedis))
}

func execute(
	args []string,
	stdout, stderr io.Writer,
	getenv func(string) string,
	connect redisFactory,
) int {
	username, code := parseCommand(args, stderr)
	if code != 0 {
		return code
	}

	addr := getenv("REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	rdb, closer, err := connect(addr)
	if err != nil {
		fmt.Fprintln(stderr, "redis connection failed; check REDIS_ADDR and Redis health")
		return 1
	}

	code = resetPassword(username, stdout, stderr, rdb)
	if err := closer.Close(); err != nil {
		fmt.Fprintln(stderr, "warning: failed to close Redis connection")
	}
	return code
}

func run(args []string, stdout, stderr io.Writer, rdb *redis.Client) int {
	username, code := parseCommand(args, stderr)
	if code != 0 {
		return code
	}
	return resetPassword(username, stdout, stderr, rdb)
}

func parseCommand(args []string, stderr io.Writer) (string, int) {
	if len(args) == 0 {
		printUsage(stderr)
		return "", 2
	}

	if args[0] != "user" {
		fmt.Fprintf(stderr, "unknown command: %s\n", args[0])
		printUsage(stderr)
		return "", 2
	}
	if len(args) == 1 {
		printUsage(stderr)
		return "", 2
	}
	if args[1] != "reset-password" {
		fmt.Fprintf(stderr, "unknown command: user %s\n", args[1])
		printUsage(stderr)
		return "", 2
	}
	if len(args) != 3 {
		printUsage(stderr)
		return "", 2
	}
	return args[2], 0
}

func resetPassword(username string, stdout, stderr io.Writer, rdb *redis.Client) int {
	store := auth.NewStore(rdb)
	svc := auth.NewService(store, systemClock{})

	ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
	defer cancel()

	tempPassword, err := svc.ResetPassword(ctx, username)
	if err != nil {
		fmt.Fprintf(stderr, "%s\n", friendlyResetError(err))
		return 1
	}

	output := "temporary_password=" + tempPassword + "\n"
	n, err := io.WriteString(stdout, output)
	if err != nil || n != len(output) {
		fmt.Fprintln(stderr, passwordDeliveryFailedWarning)
		return exitPasswordDeliveryFailed
	}
	return 0
}

func friendlyResetError(err error) string {
	if errors.Is(err, auth.ErrNotFound) {
		return "user not found"
	}
	if msg := normalizeUsernameError(err); msg != "" {
		return msg
	}
	return "reset password failed; check host and Redis health"
}

func normalizeUsernameError(err error) string {
	const prefix = "normalize username: "
	msg := err.Error()
	if strings.HasPrefix(msg, prefix) {
		return strings.TrimPrefix(msg, prefix)
	}
	return ""
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `agentlink-admin - server administration tool

Usage:
  agentlink-admin user reset-password <username>

Exit codes:
  0  password reset and temporary password delivered
  1  Redis connection or reset failed
  2  usage error or unknown command
  3  password reset but delivery failed; DO NOT RETRY AUTOMATICALLY
`)
}
