package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/team/agentlink/pkg/api"
	"github.com/team/agentlink/pkg/config"
	"github.com/team/agentlink/pkg/redis"
)

func main() {
	cfg := config.Load()

	rdb, err := redis.NewClient(cfg.RedisAddr)
	if err != nil {
		log.Fatalf("failed to connect to redis: %v", err)
	}

	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "./data"
	}

	srv := api.New(addr, dataDir, rdb, cfg.RegisterPassword)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := srv.ListenAndServe(addr); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("shutdown error: %v", err)
	}
}
