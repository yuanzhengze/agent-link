package config

import (
	"os"
	"strconv"
)

type Config struct {
	RedisAddr    string
	PublicURL    string
	CookieSecure bool
}

func Load() *Config {
	return &Config{
		RedisAddr:    getEnv("REDIS_ADDR", "localhost:6379"),
		PublicURL:    getEnv("PUBLIC_URL", "http://localhost:8080"),
		CookieSecure: getEnvBool("COOKIE_SECURE", false),
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvBool(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return parsed
}
