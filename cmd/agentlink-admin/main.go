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

const redisOpTimeout = 30 * time.Second

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

func main() {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}

	rdb, err := redis.NewClient(addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "redis: %v\n", err)
		os.Exit(1)
	}
	defer func() {
		if err := rdb.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "redis close: %v\n", err)
			os.Exit(1)
		}
	}()

	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, rdb))
}

func run(args []string, stdout, stderr io.Writer, rdb *redis.Client) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}

	switch args[0] {
	case "user":
		return runUser(args[1:], stdout, stderr, rdb)
	default:
		fmt.Fprintf(stderr, "unknown command: %s\n", args[0])
		printUsage(stderr)
		return 2
	}
}

func runUser(args []string, stdout, stderr io.Writer, rdb *redis.Client) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}

	switch args[0] {
	case "reset-password":
		if len(args) != 2 {
			printUsage(stderr)
			return 2
		}
		return resetPassword(args[1], stdout, stderr, rdb)
	default:
		fmt.Fprintf(stderr, "unknown command: user %s\n", args[0])
		printUsage(stderr)
		return 2
	}
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

	fmt.Fprintf(stdout, "temporary_password=%s\n", tempPassword)
	return 0
}

func friendlyResetError(err error) string {
	if errors.Is(err, auth.ErrNotFound) {
		return "user not found"
	}
	if msg := normalizeUsernameError(err); msg != "" {
		return msg
	}
	return "reset password failed"
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
`)
}
