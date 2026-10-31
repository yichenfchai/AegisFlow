package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"slices"
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
	BuildImage                                                                                      string
	BuildCPU, BuildMemory                                                                           string
	BuildPids                                                                                       int
	// BuildUser is the dedicated OS account for AEGIS_BUILD_SANDBOX=user.
	BuildUser string
	// BuildDisk caps the total size of one build workspace (cloned repo plus
	// generated files); the worker cancels the build when it is exceeded.
	BuildDisk int64
	// BuildClone enables cloning the project repository before running the
	// command; BuildAllowLocalRepo permits file:/// and absolute local paths.
	BuildClone          bool
	BuildAllowLocalRepo bool
	// SecretKey seals deployment-target credentials at rest (AES-256-GCM).
	// Empty disables secret storage; targets without a secret still work.
	SecretKey []byte
	// DeployCheckTimeout bounds one deployment-target connectivity probe.
	DeployCheckTimeout time.Duration
	// DeployTimeout bounds one deployment execution (default 10m, max 1h).
	DeployTimeout time.Duration
	// ApprovalEnvironments lists target environments that require an approval
	// before execution. An empty slice disables approvals; when the variable is
	// unset the default is ["production"].
	ApprovalEnvironments []string
	// ApprovalTTL bounds how long a pending approval stays valid (default 24h).
	ApprovalTTL time.Duration
}

// BuildSandbox is how build commands are executed. It mirrors the options in
// the build execution design (maintained locally, not committed): "none" runs
// commands as the service account, "user" a dedicated OS account and
// "container" a one-shot container per build.
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
	// Deployment-target credential sealing. The key is never echoed in an
	// error; an unset variable is a valid, secret-storage-disabled start.
	if rawKey := strings.TrimSpace(os.Getenv("AEGIS_SECRET_KEY")); rawKey != "" {
		key, decodeErr := base64.StdEncoding.DecodeString(rawKey)
		if decodeErr != nil || len(key) != 32 {
			return c, errors.New("AEGIS_SECRET_KEY must be base64 of exactly 32 bytes")
		}
		c.SecretKey = key
	}
	c.DeployCheckTimeout, err = duration("AEGIS_DEPLOY_CHECK_TIMEOUT", "5s")
	if err != nil {
		return c, err
	}
	if c.DeployCheckTimeout > time.Minute {
		return c, errors.New("AEGIS_DEPLOY_CHECK_TIMEOUT must be at most 1m")
	}
	// Deployment execution bound; the worker aborts the HTTP request past it.
	c.DeployTimeout, err = duration("AEGIS_DEPLOY_TIMEOUT", "10m")
	if err != nil {
		return c, err
	}
	if c.DeployTimeout > time.Hour {
		return c, errors.New("AEGIS_DEPLOY_TIMEOUT must be at most 1h")
	}
	// Deployment approvals. Presence of the variable is authoritative: an
	// explicitly empty value disables approvals, while an unset variable uses
	// the production default. Only the three documented environments are valid.
	rawEnvironments, present := os.LookupEnv("AEGIS_DEPLOY_APPROVAL_ENVIRONMENTS")
	if !present {
		rawEnvironments = "production"
	}
	for _, entry := range strings.Split(rawEnvironments, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		switch entry {
		case "development", "staging", "production":
		default:
			return c, errors.New("AEGIS_DEPLOY_APPROVAL_ENVIRONMENTS must contain only development, staging or production")
		}
		if !slices.Contains(c.ApprovalEnvironments, entry) {
			c.ApprovalEnvironments = append(c.ApprovalEnvironments, entry)
		}
	}
	c.ApprovalTTL, err = duration("AEGIS_APPROVAL_TTL", "24h")
	if err != nil {
		return c, err
	}
	if c.ApprovalTTL < time.Hour || c.ApprovalTTL > 168*time.Hour {
		return c, errors.New("AEGIS_APPROVAL_TTL must be between 1h and 168h")
	}
	for _, origin := range strings.Split(env("AEGIS_ALLOWED_ORIGINS", "http://localhost:5173"), ",") {
		origin = strings.TrimSpace(origin)
		u, e := url.Parse(origin)
		if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return c, fmt.Errorf("invalid CORS origin %q", origin)
		}
		c.AllowedOrigins = append(c.AllowedOrigins, origin)
	}
	// Build isolation switch (build execution design, local-only). "container"
	// runs each build in a one-shot container; "user" drops each build to a
	// dedicated OS account (Linux, service must start as root); "none" is the
	// developer path that runs commands as the service account.
	switch value := BuildSandbox(os.Getenv("AEGIS_BUILD_SANDBOX")); value {
	case "", BuildSandboxNone:
		c.BuildSandbox = BuildSandboxNone
	case BuildSandboxContainer:
		c.BuildSandbox = BuildSandboxContainer
		c.BuildImage = env("AEGIS_BUILD_IMAGE", "alpine:3.20")
		if c.BuildImage != strings.TrimSpace(c.BuildImage) || c.BuildImage == "" || strings.ContainsAny(c.BuildImage, " \t\r\n") {
			return c, errors.New("AEGIS_BUILD_IMAGE must be a non-empty container image reference without whitespace")
		}
		c.BuildCPU = env("AEGIS_BUILD_CPU", "2")
		if !validCPU(c.BuildCPU) {
			return c, errors.New("AEGIS_BUILD_CPU must be a number of cores greater than 0 and at most 64, e.g. 2 or 1.5")
		}
		c.BuildMemory = env("AEGIS_BUILD_MEMORY", "2g")
		if !validMemory(c.BuildMemory) {
			return c, errors.New("AEGIS_BUILD_MEMORY must be a Docker memory limit such as 512m or 2g")
		}
		c.BuildPids, poolErr = intInRange("AEGIS_BUILD_PIDS", "512", 16, 32768)
		if poolErr != nil {
			return c, poolErr
		}
	case BuildSandboxUser:
		c.BuildSandbox = BuildSandboxUser
		c.BuildUser = strings.TrimSpace(os.Getenv("AEGIS_BUILD_USER"))
		if c.BuildUser == "" || strings.ContainsAny(c.BuildUser, " \t\r\n") {
			return c, errors.New("AEGIS_BUILD_SANDBOX=user requires AEGIS_BUILD_USER naming a dedicated OS account")
		}
	default:
		return c, fmt.Errorf("AEGIS_BUILD_SANDBOX must be none, user or container, got %q", value)
	}
	if c.BuildSandbox != BuildSandboxUser && os.Getenv("AEGIS_BUILD_USER") != "" {
		return c, errors.New("AEGIS_BUILD_USER only applies to AEGIS_BUILD_SANDBOX=user")
	}
	// Workspace cap shared by every sandbox mode: a build must not fill the host
	// disk even when the command itself is unprivileged.
	c.BuildDisk, err = parseBytes("AEGIS_BUILD_DISK", "2g")
	if err != nil {
		return c, err
	}
	if c.BuildDisk < 16<<20 || c.BuildDisk > 1<<40 {
		return c, errors.New("AEGIS_BUILD_DISK must be between 16MiB and 1TiB")
	}
	c.BuildClone, err = parseBool("AEGIS_BUILD_CLONE", false)
	if err != nil {
		return c, err
	}
	c.BuildAllowLocalRepo, err = parseBool("AEGIS_BUILD_ALLOW_LOCAL_REPO", false)
	if err != nil {
		return c, err
	}
	if c.BuildAllowLocalRepo && !c.BuildClone {
		return c, errors.New("AEGIS_BUILD_ALLOW_LOCAL_REPO requires AEGIS_BUILD_CLONE=true")
	}
	return c, nil
}

// validCPU accepts a positive decimal core count; Docker's --cpus takes the
// same shape, so rejecting anything else here keeps a bad value from reaching
// the runtime.
func validCPU(value string) bool {
	n, err := strconv.ParseFloat(value, 64)
	return err == nil && n > 0 && n <= 64
}

// validMemory accepts Docker's byte-suffix form (m/g), which is what the
// defaults and documentation use.
func validMemory(value string) bool {
	if len(value) < 2 {
		return false
	}
	suffix := value[len(value)-1]
	if suffix != 'm' && suffix != 'g' {
		return false
	}
	n, err := strconv.Atoi(value[:len(value)-1])
	return err == nil && n > 0
}

func intInRange(key, fallback string, min, max int) (int, error) {
	n, err := strconv.Atoi(env(key, fallback))
	if err != nil || n < min || n > max {
		return 0, fmt.Errorf("%s must be between %d and %d", key, min, max)
	}
	return n, nil
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

// parseBytes accepts the byte-suffix form used across the build settings
// (512m, 2g, 1t). The fallback is only used when the variable is unset.
func parseBytes(key, fallback string) (int64, error) {
	value := strings.ToLower(strings.TrimSpace(env(key, fallback)))
	if len(value) < 2 {
		return 0, fmt.Errorf("%s must be a size such as 512m, 2g or 1t", key)
	}
	var multiplier int64
	switch value[len(value)-1] {
	case 'k':
		multiplier = 1 << 10
	case 'm':
		multiplier = 1 << 20
	case 'g':
		multiplier = 1 << 30
	case 't':
		multiplier = 1 << 40
	default:
		return 0, fmt.Errorf("%s must be a size such as 512m, 2g or 1t", key)
	}
	n, err := strconv.ParseInt(value[:len(value)-1], 10, 64)
	if err != nil || n <= 0 || n > (1<<62)/multiplier {
		return 0, fmt.Errorf("%s must be a positive whole-number size such as 512m, 2g or 1t", key)
	}
	return n * multiplier, nil
}

// parseBool accepts only the explicit boolean spellings; an empty value falls
// back to the default so an unset variable never guesses.
func parseBool(key string, fallback bool) (bool, error) {
	switch value := os.Getenv(key); value {
	case "":
		return fallback, nil
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("%s must be true or false", key)
	}
}
