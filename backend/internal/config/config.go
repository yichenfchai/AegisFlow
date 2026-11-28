package config

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"

	"github.com/yichenfchai/AegisFlow/backend/internal/tenants"
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
	// AuditRetention deletes audit rows older than this window; zero (the
	// default) keeps them forever.
	AuditRetention time.Duration
	// MetricsAddr enables the unauthenticated Prometheus endpoint on a separate
	// listener; empty (the default) disables it.
	MetricsAddr string
	// SecretKey seals deployment-target credentials at rest (AES-256-GCM).
	// Empty disables secret storage; targets without a secret still work.
	SecretKey []byte
	// PreviousSecretKey decrypts credentials sealed before a key rotation. It
	// never seals anything: new writes always use SecretKey. Empty disables
	// the fallback.
	PreviousSecretKey []byte
	// DeployCheckTimeout bounds one deployment-target connectivity probe.
	DeployCheckTimeout time.Duration
	// DeployTimeout bounds one deployment execution (default 10m, max 1h).
	DeployTimeout time.Duration
	// DeployHealthAttempts is how many health probes follow a 2xx deploy
	// response; 0 disables the post-deploy check (default 3, max 20).
	DeployHealthAttempts int
	// DeployHealthInterval spaces those probes (default 2s, 1s–1m).
	DeployHealthInterval time.Duration
	// ApprovalEnvironments lists target environments that require an approval
	// before execution. When the variable is unset the default is
	// ["production"]; an explicitly set empty value (including only
	// commas/whitespace) disables approvals and keeps the slice non-nil so
	// callers cannot confuse it with the unset default.
	ApprovalEnvironments []string
	// ApprovalTTL bounds how long a pending approval stays valid (default 24h).
	ApprovalTTL time.Duration
	// ApprovalWebhookURL enables approval notifications; empty disables them.
	// ApprovalWebhookSecret signs the JSON body with HMAC-SHA256 when set.
	ApprovalWebhookURL    string
	ApprovalWebhookSecret []byte
	// ApprovalWebhookTimeout bounds one webhook delivery (default 5s, max 30s).
	ApprovalWebhookTimeout time.Duration
	// ReviewWebhookSecret verifies the HMAC-SHA256 signature of inbound external
	// review-result deliveries (AEGIS_REVIEW_WEBHOOK_SECRET). Empty disables the
	// external-results endpoint (it answers 503), so a deployment that does not
	// integrate external tools keeps a closed inbound surface. The outbound
	// review-event notifications reuse ApprovalWebhookURL/Secret/Timeout: one
	// receiver channel carries both approval.* and review.* events.
	ReviewWebhookSecret []byte
	// IngestWebhookSecret verifies the HMAC-SHA256 signature of inbound scan and
	// security-event deliveries (AEGIS_INGEST_WEBHOOK_SECRET, P5 batch A). Empty
	// keeps both signed ingest endpoints closed (503) while their authenticated
	// read routes stay available; a configured value must be 16-256 bytes and is
	// never echoed in an error.
	IngestWebhookSecret []byte
	// OIDC is the optional OIDC single sign-on configuration (P5 batch D). The
	// zero value disables the feature completely: no route gains a working
	// authorization flow and the login page keeps its password-only behavior.
	OIDC OIDCConfig
	// LDAP is the optional directory authentication configuration (phase G).
	// AEGIS_LDAP_URL is the master switch: the zero value keeps the feature
	// completely disabled, so an upgrade without the variable changes nothing.
	LDAP LDAPConfig
	// RequireReviewEnvironments lists target environments whose deployments
	// must reference an approved review. Empty (the default, including an
	// explicitly empty value or only commas/whitespace) disables the gate: the
	// review gate is opt-in so an upgrade keeps the previous deployment
	// behavior, and only the three documented environments are valid.
	RequireReviewEnvironments []string
	// DefaultTenantSlug is the deployment's default tenant slug
	// (AEGIS_DEFAULT_TENANT_SLUG). Startup renames the seeded tenant when the
	// configured slug differs and no other tenant exists, and fails when the
	// slug is still missing, so a misconfigured deployment never starts with an
	// unreachable default tenant.
	DefaultTenantSlug string
}

// OIDCConfig is the OIDC single sign-on configuration (P5 batch D). Issuer is
// the master switch: an empty issuer disables the feature entirely, and every
// other OIDC variable then fails startup instead of being silently ignored.
// The startup only validates syntax and URL safety; the actual provider
// discovery happens in cmd/server before the HTTP listener opens, and a
// failed discovery refuses to start.
type OIDCConfig struct {
	// Issuer is the exact OIDC issuer identifier (https, or loopback http for
	// local tests). It is the expected `iss` claim and the discovery base URL.
	Issuer string
	// ClientID and ClientSecret identify the registered OIDC client. An empty
	// ClientSecret selects the public-client PKCE flow (client_secret_post is
	// used only when a secret is configured).
	ClientID     string
	ClientSecret string
	// RedirectURL is the absolute callback URL registered with the provider
	// (GET /api/v1/auth/oidc/callback). It follows the same https/loopback
	// rule as the issuer and must not contain userinfo, query or fragment.
	RedirectURL string
	// Scopes is the authorization request scope list; it always contains
	// "openid" and defaults to openid profile email.
	Scopes []string
	// AutoProvision creates a member account on first login when the verified
	// email matches no local account. It never creates an admin.
	AutoProvision bool
	// Label is the login-page button text; never an empty string.
	Label string
	// DiscoveryTimeout bounds the startup discovery fetch and every provider
	// HTTP call (default 5s, at most 30s).
	DiscoveryTimeout time.Duration
}

// Enabled reports whether the OIDC feature is configured. The zero OIDCConfig
// is disabled, so a deployment that sets none of the AEGIS_OIDC_* variables
// keeps exactly the previous password-only surface.
func (c OIDCConfig) Enabled() bool { return c.Issuer != "" }

// LDAPConfig is the optional LDAP directory authentication configuration
// (phase G). URL is the master switch: an empty URL disables the feature
// completely, and every other AEGIS_LDAP_* variable then fails startup instead
// of being silently ignored. The directory is only a credential source: no
// directory data is persisted and no local password is written.
type LDAPConfig struct {
	// URL is the directory endpoint (ldap:// or ldaps://). It never contains
	// userinfo, query, fragment or a path; plain ldap:// is accepted only for a
	// loopback host or with StartTLS.
	URL string
	// StartTLS upgrades a plain ldap:// connection before the first bind. It is
	// rejected together with ldaps://, which is already encrypted.
	StartTLS bool
	// BindDN/BindPassword are the optional service account used to run the
	// search. Both are set or both are empty (anonymous search).
	BindDN       string
	BindPassword string
	// BaseDN is the search base. Required when the feature is enabled.
	BaseDN string
	// UserFilter is the search filter with exactly one {username} placeholder;
	// the placeholder is replaced with the RFC 4515-escaped login name.
	UserFilter string
	// UsernameAttribute, EmailAttribute and DisplayNameAttribute select the
	// attributes read from the single matching entry.
	UsernameAttribute    string
	EmailAttribute       string
	DisplayNameAttribute string
	// AutoProvision creates a member account on first login when the directory
	// identity matches no local account. It never creates an admin and requires
	// a valid directory email.
	AutoProvision bool
	// Label is the login-page entry text; never an empty string.
	Label string
	// Timeout bounds one directory authentication: connect, optional StartTLS,
	// service bind, search and the user bind share the same deadline (default
	// 5s, at most 30s).
	Timeout time.Duration
}

// Enabled reports whether LDAP directory authentication is configured. The
// zero LDAPConfig is disabled, so a deployment that sets none of the
// AEGIS_LDAP_* variables keeps exactly the previous password-only surface.
func (c LDAPConfig) Enabled() bool { return c.URL != "" }

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
	dbURL, parseErr := validateDatabaseURL(c.DatabaseURL)
	if parseErr != nil {
		return c, parseErr
	}
	c.DatabaseURL = dbURL
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
	// The default tenant exists in every deployment; phase F1 never creates a
	// second one. The slug is validated here so a bad value fails startup rather
	// than surfacing during the tenant lookup below.
	c.DefaultTenantSlug = env("AEGIS_DEFAULT_TENANT_SLUG", tenants.DefaultSlug)
	if !tenants.ValidSlug(c.DefaultTenantSlug) {
		return c, errors.New("AEGIS_DEFAULT_TENANT_SLUG must be 1-32 lowercase ASCII letters, digits and inner hyphens, and must not start or end with a hyphen")
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
	// Audit retention is opt-in deletion: unset or "0" keeps every row forever,
	// because an audit trail should not lose evidence unless the operator asks.
	if raw := strings.TrimSpace(os.Getenv("AEGIS_AUDIT_RETENTION")); raw == "" || raw == "0" {
		c.AuditRetention = 0
	} else {
		c.AuditRetention, err = duration("AEGIS_AUDIT_RETENTION", raw)
		if err != nil {
			return c, err
		}
		if c.AuditRetention < 24*time.Hour || c.AuditRetention > 8760*time.Hour {
			return c, errors.New("AEGIS_AUDIT_RETENTION must be 0 (keep forever) or between 24h and 8760h")
		}
	}
	// Metrics listen on their own address so the public API origin never exposes
	// deployment-wide counters; the endpoint itself has no user authentication,
	// so bind loopback or protect it at the proxy.
	c.MetricsAddr = strings.TrimSpace(os.Getenv("AEGIS_METRICS_ADDR"))
	if c.MetricsAddr != "" {
		_, port, splitErr := net.SplitHostPort(c.MetricsAddr)
		if splitErr != nil {
			return c, errors.New("AEGIS_METRICS_ADDR must be host:port, for example 127.0.0.1:9090")
		}
		numericPort, convErr := strconv.Atoi(port)
		if convErr != nil || numericPort < 0 || numericPort > 65535 {
			return c, errors.New("AEGIS_METRICS_ADDR must use a numeric port between 0 and 65535")
		}
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
	// A rotation keeps the retiring key available for decryption only. It must
	// be a different key: equal values mean the primary was not rotated and
	// only hide that mistake.
	if rawPrevious := strings.TrimSpace(os.Getenv("AEGIS_SECRET_KEY_PREVIOUS")); rawPrevious != "" {
		previous, decodeErr := base64.StdEncoding.DecodeString(rawPrevious)
		if decodeErr != nil || len(previous) != 32 {
			return c, errors.New("AEGIS_SECRET_KEY_PREVIOUS must be base64 of exactly 32 bytes")
		}
		if len(c.SecretKey) == 0 {
			return c, errors.New("AEGIS_SECRET_KEY_PREVIOUS requires AEGIS_SECRET_KEY")
		}
		if bytes.Equal(previous, c.SecretKey) {
			return c, errors.New("AEGIS_SECRET_KEY_PREVIOUS must differ from AEGIS_SECRET_KEY")
		}
		c.PreviousSecretKey = previous
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
	// Post-deploy health verification. A 2xx from the deploy endpoint only
	// proves the request was accepted, so by default the worker re-probes the
	// target's health path; 0 disables that second opinion.
	c.DeployHealthAttempts, err = intInRange("AEGIS_DEPLOY_HEALTH_ATTEMPTS", "3", 0, 20)
	if err != nil {
		return c, err
	}
	c.DeployHealthInterval, err = duration("AEGIS_DEPLOY_HEALTH_INTERVAL", "2s")
	if err != nil {
		return c, err
	}
	if c.DeployHealthInterval > time.Minute {
		return c, errors.New("AEGIS_DEPLOY_HEALTH_INTERVAL must be at most 1m")
	}
	// Deployment approvals. Presence of the variable is authoritative: an
	// explicitly empty value disables approvals, while an unset variable uses
	// the production default. Only the three documented environments are valid.
	// The result is always non-nil: deployments.New treats nil as "unset,
	// default to production", so returning nil for an explicit empty value
	// would silently re-enable approvals.
	rawEnvironments, present := os.LookupEnv("AEGIS_DEPLOY_APPROVAL_ENVIRONMENTS")
	if !present {
		rawEnvironments = "production"
	}
	c.ApprovalEnvironments = []string{}
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
	// Deployment review gate (P5-A E2). Unlike the approval default there is no
	// environment fallback: an unset or explicitly empty value keeps the gate
	// disabled, and only the three documented environments are accepted. The
	// slice is always non-nil, so an empty value can never be mistaken for an
	// unset default by a future caller.
	c.RequireReviewEnvironments = []string{}
	for _, entry := range strings.Split(os.Getenv("AEGIS_DEPLOY_REQUIRE_REVIEW_ENVIRONMENTS"), ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		switch entry {
		case "development", "staging", "production":
		default:
			return c, errors.New("AEGIS_DEPLOY_REQUIRE_REVIEW_ENVIRONMENTS must contain only development, staging or production")
		}
		if !slices.Contains(c.RequireReviewEnvironments, entry) {
			c.RequireReviewEnvironments = append(c.RequireReviewEnvironments, entry)
		}
	}
	c.ApprovalTTL, err = duration("AEGIS_APPROVAL_TTL", "24h")
	if err != nil {
		return c, err
	}
	if c.ApprovalTTL < time.Hour || c.ApprovalTTL > 168*time.Hour {
		return c, errors.New("AEGIS_APPROVAL_TTL must be between 1h and 168h")
	}
	// Approval notifications are opt-in. A configured URL must be HTTPS, or
	// HTTP only for a loopback receiver used by local integrations/tests; the
	// URL is never logged. The signing secret is optional but, when present,
	// must be long enough to make HMAC-SHA256 meaningful.
	c.ApprovalWebhookURL = strings.TrimSpace(os.Getenv("AEGIS_APPROVAL_WEBHOOK_URL"))
	if c.ApprovalWebhookURL != "" {
		if len(c.ApprovalWebhookURL) > 2048 {
			return c, errors.New("AEGIS_APPROVAL_WEBHOOK_URL must be at most 2048 bytes")
		}
		webhookURL, parseErr := url.Parse(c.ApprovalWebhookURL)
		if parseErr != nil || (webhookURL.Scheme != "http" && webhookURL.Scheme != "https") ||
			webhookURL.Host == "" || webhookURL.User != nil || webhookURL.Fragment != "" {
			return c, errors.New("AEGIS_APPROVAL_WEBHOOK_URL must be an http(s) URL without userinfo or fragment")
		}
		if webhookURL.Scheme == "http" && !isLoopbackHost(webhookURL.Hostname()) {
			return c, errors.New("AEGIS_APPROVAL_WEBHOOK_URL must use https unless the host is loopback")
		}
		if raw := os.Getenv("AEGIS_APPROVAL_WEBHOOK_SECRET"); raw != "" {
			if len(raw) < 16 || len(raw) > 256 {
				return c, errors.New("AEGIS_APPROVAL_WEBHOOK_SECRET must contain 16-256 bytes")
			}
			c.ApprovalWebhookSecret = []byte(raw)
		}
		c.ApprovalWebhookTimeout, err = duration("AEGIS_APPROVAL_WEBHOOK_TIMEOUT", "5s")
		if err != nil {
			return c, err
		}
		if c.ApprovalWebhookTimeout > 30*time.Second {
			return c, errors.New("AEGIS_APPROVAL_WEBHOOK_TIMEOUT must be at most 30s")
		}
	} else if os.Getenv("AEGIS_APPROVAL_WEBHOOK_SECRET") != "" {
		return c, errors.New("AEGIS_APPROVAL_WEBHOOK_SECRET requires AEGIS_APPROVAL_WEBHOOK_URL")
	}
	// The inbound external-result webhook is separate from the outbound approval
	// channel: its secret authenticates third-party requests, so an unset value
	// simply leaves that endpoint closed (503) instead of disabling anything
	// else. A configured value must be long enough for HMAC-SHA256 to be
	// meaningful; it is never echoed in an error.
	if raw := os.Getenv("AEGIS_REVIEW_WEBHOOK_SECRET"); raw != "" {
		if len(raw) < 16 || len(raw) > 256 {
			return c, errors.New("AEGIS_REVIEW_WEBHOOK_SECRET must contain 16-256 bytes")
		}
		c.ReviewWebhookSecret = []byte(raw)
	}
	// The scan/security-event ingest webhook shares the exact signature scheme
	// of the external-result endpoint but keeps its own secret: a tool that may
	// report scan results does not automatically gain access to review evidence.
	// An unset value leaves both ingest endpoints closed (503), never affects the
	// read routes, and is not an error.
	if raw := os.Getenv("AEGIS_INGEST_WEBHOOK_SECRET"); raw != "" {
		if len(raw) < 16 || len(raw) > 256 {
			return c, errors.New("AEGIS_INGEST_WEBHOOK_SECRET must contain 16-256 bytes")
		}
		c.IngestWebhookSecret = []byte(raw)
	}
	// OIDC single sign-on (P5 batch D) is opt-in: an unset issuer disables the
	// feature completely, including route effects. Setting any other OIDC
	// variable without the issuer is a configuration mistake and fails closed.
	if err = loadOIDC(&c); err != nil {
		return c, err
	}
	// LDAP directory authentication (phase G) is opt-in the same way: an unset
	// URL disables it completely, and any other AEGIS_LDAP_* variable without
	// the URL fails closed.
	if err = loadLDAP(&c); err != nil {
		return c, err
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

// DatabaseURL validates and returns the shared PostgreSQL connection
// configuration. Operational commands (cmd/tenant) use it instead of the full
// server configuration: they need the same connection settings and validation,
// but none of the HTTP/auth/build variables. Nothing new has to be configured
// for them beyond the existing AEGIS_DATABASE_URL.
func DatabaseURL() (string, error) {
	if os.Getenv("AEGIS_DATABASE_PATH") != "" {
		return "", errors.New("AEGIS_DATABASE_PATH is obsolete; configure PostgreSQL with AEGIS_DATABASE_URL")
	}
	return validateDatabaseURL(os.Getenv("AEGIS_DATABASE_URL"))
}

// validateDatabaseURL enforces the documented PostgreSQL URL shape for every
// caller (server start and operational commands), so a malformed value fails the
// same way everywhere.
func validateDatabaseURL(raw string) (string, error) {
	dbURL, parseErr := url.Parse(raw)
	if parseErr != nil || (dbURL.Scheme != "postgres" && dbURL.Scheme != "postgresql") || dbURL.Host == "" || dbURL.Path == "" || dbURL.Path == "/" {
		return "", errors.New("AEGIS_DATABASE_URL must be a PostgreSQL URL with host and database name")
	}
	return raw, nil
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

// isLoopbackHost reports whether host is a literal loopback address or the
// localhost name. It is deliberately conservative: only an explicit loopback
// receiver may use plain HTTP for webhook delivery.
func isLoopbackHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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

// loadOIDC parses and validates the optional OIDC single sign-on settings. It
// never reaches the network: provider discovery is a startup step in cmd/server
// because only that layer can fail the process before the listener opens.
func loadOIDC(c *Config) error {
	rawIssuer := strings.TrimSpace(os.Getenv("AEGIS_OIDC_ISSUER"))
	if rawIssuer == "" {
		// Fail closed on partial configuration instead of silently starting a
		// server whose operator believes SSO is enabled.
		for _, key := range []string{"AEGIS_OIDC_CLIENT_ID", "AEGIS_OIDC_CLIENT_SECRET", "AEGIS_OIDC_REDIRECT_URL", "AEGIS_OIDC_SCOPES", "AEGIS_OIDC_AUTO_PROVISION", "AEGIS_OIDC_LABEL", "AEGIS_OIDC_DISCOVERY_TIMEOUT"} {
			if os.Getenv(key) != "" {
				return fmt.Errorf("%s requires AEGIS_OIDC_ISSUER; OIDC single sign-on is disabled when the issuer is unset", key)
			}
		}
		return nil
	}
	// A trailing slash would produce "<issuer>//.well-known/...", so normalize
	// before the syntax check; discovery later requires an exact issuer match.
	issuer, err := validateOIDCEndpoint(strings.TrimRight(rawIssuer, "/"), "AEGIS_OIDC_ISSUER")
	if err != nil {
		return err
	}
	c.OIDC.Issuer = issuer
	clientID := strings.TrimSpace(os.Getenv("AEGIS_OIDC_CLIENT_ID"))
	if !validOIDCText(clientID, 1, 256) {
		return errors.New("AEGIS_OIDC_CLIENT_ID must be a non-empty single line of at most 256 bytes when OIDC is enabled")
	}
	c.OIDC.ClientID = clientID
	// Optional client secret: present selects a confidential client
	// (client_secret_post); absent keeps the public-client PKCE flow.
	if secret := os.Getenv("AEGIS_OIDC_CLIENT_SECRET"); secret != "" {
		if len(secret) > 512 || strings.IndexFunc(secret, unicode.IsControl) >= 0 {
			return errors.New("AEGIS_OIDC_CLIENT_SECRET must be a single line of at most 512 bytes")
		}
		c.OIDC.ClientSecret = secret
	}
	redirect, err := validateOIDCEndpoint(strings.TrimSpace(os.Getenv("AEGIS_OIDC_REDIRECT_URL")), "AEGIS_OIDC_REDIRECT_URL")
	if err != nil {
		return err
	}
	c.OIDC.RedirectURL = redirect
	scopes := strings.Fields(os.Getenv("AEGIS_OIDC_SCOPES"))
	if len(scopes) == 0 {
		scopes = []string{"openid", "profile", "email"}
	}
	for _, scope := range scopes {
		if !validOIDCScope(scope) {
			return errors.New("AEGIS_OIDC_SCOPES must contain space-separated scopes of 1-64 printable ASCII bytes")
		}
		if !slices.Contains(c.OIDC.Scopes, scope) {
			c.OIDC.Scopes = append(c.OIDC.Scopes, scope)
		}
	}
	if !slices.Contains(c.OIDC.Scopes, "openid") {
		return errors.New("AEGIS_OIDC_SCOPES must include the openid scope")
	}
	c.OIDC.AutoProvision, err = parseBool("AEGIS_OIDC_AUTO_PROVISION", false)
	if err != nil {
		return err
	}
	c.OIDC.Label = strings.TrimSpace(env("AEGIS_OIDC_LABEL", "企业单点登录"))
	if !validOIDCText(c.OIDC.Label, 1, 128) {
		return errors.New("AEGIS_OIDC_LABEL must be a non-empty single line of at most 128 bytes")
	}
	c.OIDC.DiscoveryTimeout, err = duration("AEGIS_OIDC_DISCOVERY_TIMEOUT", "5s")
	if err != nil {
		return err
	}
	if c.OIDC.DiscoveryTimeout > 30*time.Second {
		return errors.New("AEGIS_OIDC_DISCOVERY_TIMEOUT must be at most 30s")
	}
	return nil
}

// validateOIDCEndpoint accepts an absolute http(s) URL without userinfo, query
// or fragment. Plain HTTP is only allowed for loopback hosts, so local test
// providers work without weakening a production configuration. The issuer and
// redirect URL use exactly the same rule.
func validateOIDCEndpoint(raw, key string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("%s is required when OIDC single sign-on is enabled", key)
	}
	if len(raw) > 2048 || !utf8.ValidString(raw) || strings.IndexFunc(raw, unicode.IsControl) >= 0 || strings.ContainsAny(raw, " \t") {
		return "", fmt.Errorf("%s must be a single-line absolute URL of at most 2048 bytes", key)
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" {
		return "", fmt.Errorf("%s must be an absolute http(s) URL without userinfo, query or fragment", key)
	}
	switch parsed.Scheme {
	case "https":
	case "http":
		if !isLoopbackHost(parsed.Hostname()) {
			return "", fmt.Errorf("%s must use https unless the host is loopback", key)
		}
	default:
		return "", fmt.Errorf("%s must be an http(s) URL", key)
	}
	return raw, nil
}

// validOIDCText accepts a non-empty UTF-8 single line inside the byte bounds.
func validOIDCText(value string, min, max int) bool {
	return utf8.ValidString(value) && len(value) >= min && len(value) <= max && strings.IndexFunc(value, unicode.IsControl) < 0
}

// validOIDCScope accepts the RFC 6749 scope-token shape: printable ASCII
// without spaces, quotes or backslashes.
func validOIDCScope(scope string) bool {
	if len(scope) < 1 || len(scope) > 64 {
		return false
	}
	for i := 0; i < len(scope); i++ {
		ch := scope[i]
		if ch < 0x21 || ch > 0x7e || ch == '"' || ch == '\\' {
			return false
		}
	}
	return true
}
