package redis

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
)

type Client struct {
	*redis.Client
}

func NewClient(addr string) (*Client, error) {
	return NewClientDB(addr, 0)
}

func NewClientDB(addr string, db int) (*Client, error) {
	c := redis.NewClient(&redis.Options{
		Addr: addr,
		DB:   db,
	})

	if err := c.Ping(context.Background()).Err(); err != nil {
		return nil, fmt.Errorf("redis ping: %w", err)
	}

	return &Client{c}, nil
}
