package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"
)

type Config struct {
	Address, DatabasePath, JWTSecret, BootstrapUsername, BootstrapPassword string
	AllowedOrigins                                                         []string
	AccessTTL, RefreshTTL                                                  time.Duration
}

// Load fails closed: credentials have no built-in defaults. Durations use Go syntax.
func Load() (Config, error) {
	c := Config{Address: env("AEGIS_HTTP_ADDR", "127.0.0.1:8080"), DatabasePath: env("AEGIS_DATABASE_PATH", "data/aegisflow.db"), JWTSecret: os.Getenv("AEGIS_JWT_SECRET"), BootstrapUsername: os.Getenv("AEGIS_BOOTSTRAP_USERNAME"), BootstrapPassword: os.Getenv("AEGIS_BOOTSTRAP_PASSWORD")}
	if len(c.JWTSecret) < 32 {
		return c, errors.New("AEGIS_JWT_SECRET must contain at least 32 bytes")
	}
	if _, _, err := net.SplitHostPort(c.Address); err != nil {
		return c, fmt.Errorf("AEGIS_HTTP_ADDR: %w", err)
	}
	if (c.BootstrapUsername == "") != (c.BootstrapPassword == "") {
		return c, errors.New("both bootstrap username and password must be configured together")
	}
	var err error
	c.AccessTTL, err = duration("AEGIS_ACCESS_TTL", "15m")
	if err != nil {
		return c, err
	}
	c.RefreshTTL, err = duration("AEGIS_REFRESH_TTL", "720h")
	if err != nil {
		return c, err
	}
	if c.AccessTTL > time.Hour || c.RefreshTTL <= c.AccessTTL {
		return c, errors.New("access TTL must be <= 1h and refresh TTL must exceed access TTL")
	}
	for _, origin := range strings.Split(env("AEGIS_ALLOWED_ORIGINS", "http://localhost:5173"), ",") {
		origin = strings.TrimSpace(origin)
		u, e := url.Parse(origin)
		if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return c, fmt.Errorf("invalid CORS origin %q", origin)
		}
		c.AllowedOrigins = append(c.AllowedOrigins, origin)
	}
	return c, nil
}
func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
func duration(key, fallback string) (time.Duration, error) {
	d, err := time.ParseDuration(env(key, fallback))
	if err != nil || d < time.Second {
		return 0, fmt.Errorf("%s must be a duration of at least one second", key)
	}
	return d, nil
}
