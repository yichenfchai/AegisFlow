package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"
	"time"
)

type Config struct {
	Address, DatabaseURL, RedisURL, RedisNamespace, JWTSecret, BootstrapUsername, BootstrapPassword string
	DBMaxOpen, DBMaxIdle, LoginRateLimit                                                            int
	TrustedProxies                                                                                  []string
	AllowedOrigins                                                                                  []string
	AccessTTL, RefreshTTL, BuildTimeout                                                             time.Duration
	CookieSecure                                                                                    bool
	BuildSandbox                                                                                    BuildSandbox
}

// BuildSandbox is how build commands are executed. It mirrors the options in
// docs/build-execution-design.md: "none" runs commands as the service account,
// "user" a dedicated OS account and "container" a one-shot container per build.
type BuildSandbox string

const (
	BuildSandboxNone      BuildSandbox = "none"
	BuildSandboxUser      BuildSandbox = "user"
	BuildSandboxContainer BuildSandbox = "container"
)

// Load fails closed: credentials have no built-in defaults. Durations use Go syntax.
func Load() (Config, error) {
	c := Config{Address: env("AEGIS_HTTP_ADDR", "127.0.0.1:8080"), DatabaseURL: os.Getenv("AEGIS_DATABASE_URL"), RedisURL: os.Getenv("AEGIS_REDIS_URL"), RedisNamespace: env("AEGIS_REDIS_NAMESPACE", "aegisflow"), JWTSecret: os.Getenv("AEGIS_JWT_SECRET"), BootstrapUsername: os.Getenv("AEGIS_BOOTSTRAP_USERNAME"), BootstrapPassword: os.Getenv("AEGIS_BOOTSTRAP_PASSWORD")}
	if os.Getenv("AEGIS_DATABASE_PATH") != "" {
		return c, errors.New("AEGIS_DATABASE_PATH is obsolete; configure PostgreSQL with AEGIS_DATABASE_URL")
	}
	dbURL, parseErr := url.Parse(c.DatabaseURL)
	if parseErr != nil || (dbURL.Scheme != "postgres" && dbURL.Scheme != "postgresql") || dbURL.Host == "" || dbURL.Path == "" || dbURL.Path == "/" {
		return c, errors.New("AEGIS_DATABASE_URL must be a PostgreSQL URL with host and database name")
	}
	if _, err := redis.ParseURL(c.RedisURL); err != nil {
		return c, errors.New("AEGIS_REDIS_URL must be a valid redis:// or rediss:// URL")
	}
	if strings.TrimSpace(c.RedisNamespace) != c.RedisNamespace || len(c.RedisNamespace) == 0 || len(c.RedisNamespace) > 64 {
		return c, errors.New("AEGIS_REDIS_NAMESPACE must contain 1-64 non-whitespace bytes")
	}
	var poolErr error
	c.DBMaxOpen, poolErr = positiveInt("AEGIS_DB_MAX_OPEN", "20")
	if poolErr != nil {
		return c, poolErr
	}
	c.DBMaxIdle, poolErr = positiveInt("AEGIS_DB_MAX_IDLE", "5")
	if poolErr != nil {
		return c, poolErr
	}
	if c.DBMaxIdle > c.DBMaxOpen {
		return c, errors.New("AEGIS_DB_MAX_IDLE must not exceed AEGIS_DB_MAX_OPEN")
	}
	// Login quota per client address and minute. The default stays at the
	// production value; test environments with several accounts on one
	// address raise it explicitly instead of bypassing the shared limiter.
	c.LoginRateLimit, poolErr = positiveInt("AEGIS_LOGIN_RATE_LIMIT", "10")
	if poolErr != nil {
		return c, poolErr
	}
	for _, cidr := range strings.Split(os.Getenv("AEGIS_TRUSTED_PROXIES"), ",") {
		cidr = strings.TrimSpace(cidr)
		if cidr == "" {
			continue
		}
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return c, errors.New("AEGIS_TRUSTED_PROXIES must contain CIDRs")
		}
		c.TrustedProxies = append(c.TrustedProxies, cidr)
	}
	if len(c.JWTSecret) < 32 {
		return c, errors.New("AEGIS_JWT_SECRET must contain at least 32 bytes")
	}
	if _, _, err := net.SplitHostPort(c.Address); err != nil {
		return c, fmt.Errorf("AEGIS_HTTP_ADDR: %w", err)
	}
	if (c.BootstrapUsername == "") != (c.BootstrapPassword == "") {
		return c, errors.New("both bootstrap username and password must be configured together")
	}
	// Deliberately accept only the explicit boolean spellings, not proxy headers
	// or a guessed environment. Local HTTP defaults false; production must set true.
	switch value := os.Getenv("AEGIS_COOKIE_SECURE"); value {
	case "", "false":
		c.CookieSecure = false
	case "true":
		c.CookieSecure = true
	default:
		return c, errors.New("AEGIS_COOKIE_SECURE must be true or false")
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
	// Build execution bound; the worker kills processes past it.
	c.BuildTimeout, err = duration("AEGIS_BUILD_TIMEOUT", "10m")
	if err != nil {
		return c, err
	}
	if c.BuildTimeout > 24*time.Hour {
		return c, errors.New("AEGIS_BUILD_TIMEOUT must be at most 24h")
	}
	for _, origin := range strings.Split(env("AEGIS_ALLOWED_ORIGINS", "http://localhost:5173"), ",") {
		origin = strings.TrimSpace(origin)
		u, e := url.Parse(origin)
		if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return c, fmt.Errorf("invalid CORS origin %q", origin)
		}
		c.AllowedOrigins = append(c.AllowedOrigins, origin)
	}
	// Build isolation switch. Only the unisolated mode exists today: accepting
	// "user" or "container" while the executor still runs commands as the
	// service account would be a false guarantee, so it fails closed until the
	// controlled execution environment lands (docs/build-execution-design.md §7).
	switch value := BuildSandbox(os.Getenv("AEGIS_BUILD_SANDBOX")); value {
	case "", BuildSandboxNone:
		c.BuildSandbox = BuildSandboxNone
	case BuildSandboxUser, BuildSandboxContainer:
		return c, fmt.Errorf("AEGIS_BUILD_SANDBOX=%s is not implemented yet: builds would still run as the service account; use none", value)
	default:
		return c, fmt.Errorf("AEGIS_BUILD_SANDBOX must be none, user or container, got %q", value)
	}
	return c, nil
}
func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
func positiveInt(key, fallback string) (int, error) {
	n, err := strconv.Atoi(env(key, fallback))
	if err != nil || n < 1 || n > 1000 {
		return 0, fmt.Errorf("%s must be between 1 and 1000", key)
	}
	return n, nil
}
func duration(key, fallback string) (time.Duration, error) {
	d, err := time.ParseDuration(env(key, fallback))
	if err != nil || d < time.Second {
		return 0, fmt.Errorf("%s must be a duration of at least one second", key)
	}
	return d, nil
}
