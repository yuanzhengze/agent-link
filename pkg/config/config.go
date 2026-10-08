package config

import (
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	RedisAddr        string
	PublicURL        string
	CookieSecure     bool
	PreviewPublicURL string
}

func Load() *Config {
	publicURL := getEnv("PUBLIC_URL", "http://localhost:8080")
	return &Config{
		RedisAddr:        getEnv("REDIS_ADDR", "localhost:6379"),
		PublicURL:        publicURL,
		CookieSecure:     getEnvBool("COOKIE_SECURE", false),
		PreviewPublicURL: derivePreviewPublicURL(publicURL, os.Getenv("PREVIEW_PUBLIC_URL")),
	}
}

// derivePreviewPublicURL picks the origin that serves member-authored preview
// HTML. An explicit URL wins. "off", "same-origin", and "disabled" keep preview
// on the application origin. When unset, localhost automatically splits onto
// 127.0.0.1 (a different origin that always resolves); any other host stays
// same-origin until PREVIEW_PUBLIC_URL is set to a real second hostname.
func derivePreviewPublicURL(publicURL, explicit string) string {
	switch strings.ToLower(strings.TrimSpace(explicit)) {
	case "off", "same-origin", "disabled":
		return ""
	}
	if strings.TrimSpace(explicit) != "" {
		return strings.TrimSpace(explicit)
	}
	u, err := url.Parse(publicURL)
	if err != nil || u.Scheme == "" || u.Hostname() == "" {
		return ""
	}
	if !strings.EqualFold(u.Hostname(), "localhost") {
		return ""
	}
	host := "127.0.0.1"
	if port := u.Port(); port != "" {
		host = net.JoinHostPort(host, port)
	}
	return u.Scheme + "://" + host
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
