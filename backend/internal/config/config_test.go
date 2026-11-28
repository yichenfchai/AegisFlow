package config

import (
	"bytes"
	"encoding/base64"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// unsetEnv makes a variable genuinely absent, which is distinct from an
// explicitly empty value for AEGIS_DEPLOY_APPROVAL_ENVIRONMENTS.
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	previous, present := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if present {
			_ = os.Setenv(key, previous)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

func setup(t *testing.T) {
	t.Helper()
	for _, key := range []string{"AEGIS_HTTP_ADDR", "AEGIS_DATABASE_PATH", "AEGIS_BOOTSTRAP_USERNAME", "AEGIS_BOOTSTRAP_PASSWORD", "AEGIS_ACCESS_TTL", "AEGIS_REFRESH_TTL", "AEGIS_ALLOWED_ORIGINS", "AEGIS_COOKIE_SECURE", "AEGIS_BUILD_SANDBOX", "AEGIS_BUILD_IMAGE", "AEGIS_BUILD_CPU", "AEGIS_BUILD_MEMORY", "AEGIS_BUILD_PIDS", "AEGIS_BUILD_USER", "AEGIS_BUILD_DISK", "AEGIS_BUILD_CLONE", "AEGIS_BUILD_ALLOW_LOCAL_REPO", "AEGIS_AUDIT_RETENTION", "AEGIS_METRICS_ADDR", "AEGIS_SECRET_KEY", "AEGIS_SECRET_KEY_PREVIOUS", "AEGIS_DEPLOY_CHECK_TIMEOUT", "AEGIS_DEPLOY_TIMEOUT", "AEGIS_DEPLOY_HEALTH_ATTEMPTS", "AEGIS_DEPLOY_HEALTH_INTERVAL", "AEGIS_APPROVAL_TTL", "AEGIS_APPROVAL_WEBHOOK_URL", "AEGIS_APPROVAL_WEBHOOK_SECRET", "AEGIS_APPROVAL_WEBHOOK_TIMEOUT", "AEGIS_REVIEW_WEBHOOK_SECRET", "AEGIS_INGEST_WEBHOOK_SECRET", "AEGIS_DEFAULT_TENANT_SLUG", "AEGIS_OIDC_ISSUER", "AEGIS_OIDC_CLIENT_ID", "AEGIS_OIDC_CLIENT_SECRET", "AEGIS_OIDC_REDIRECT_URL", "AEGIS_OIDC_SCOPES", "AEGIS_OIDC_AUTO_PROVISION", "AEGIS_OIDC_LABEL", "AEGIS_OIDC_DISCOVERY_TIMEOUT", "AEGIS_LDAP_URL", "AEGIS_LDAP_STARTTLS", "AEGIS_LDAP_BIND_DN", "AEGIS_LDAP_BIND_PASSWORD", "AEGIS_LDAP_BASE_DN", "AEGIS_LDAP_USER_FILTER", "AEGIS_LDAP_USERNAME_ATTRIBUTE", "AEGIS_LDAP_EMAIL_ATTRIBUTE", "AEGIS_LDAP_DISPLAY_NAME_ATTRIBUTE", "AEGIS_LDAP_AUTO_PROVISION", "AEGIS_LDAP_LABEL", "AEGIS_LDAP_TIMEOUT"} {
		t.Setenv(key, "")
	}
	unsetEnv(t, "AEGIS_DEPLOY_APPROVAL_ENVIRONMENTS")
	t.Setenv("AEGIS_DEPLOY_REQUIRE_REVIEW_ENVIRONMENTS", "")
	t.Setenv("AEGIS_JWT_SECRET", strings.Repeat("s", 32))
	t.Setenv("AEGIS_DATABASE_URL", "postgres://test:test@localhost:5432/aegis_test?sslmode=disable")
	t.Setenv("AEGIS_REDIS_URL", "redis://localhost:6379/0")
	for _, key := range []string{"AEGIS_DB_MAX_OPEN", "AEGIS_DB_MAX_IDLE", "AEGIS_TRUSTED_PROXIES", "AEGIS_REDIS_NAMESPACE", "AEGIS_LOGIN_RATE_LIMIT"} {
		t.Setenv(key, "")
	}
}
func TestLoad(t *testing.T) {
	setup(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.CookieSecure || c.AccessTTL != 15*time.Minute || c.Address != "127.0.0.1:8080" || c.LoginRateLimit != 10 {
		t.Fatalf("unexpected config: %+v", c)
	}
}
func TestCookieSecureConfig(t *testing.T) {
	for _, tc := range []struct {
		value   string
		want    bool
		invalid bool
	}{
		{"", false, false}, {"false", false, false}, {"true", true, false},
		{"TRUE", false, true}, {"1", false, true}, {"yes", false, true}, {" false", false, true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			setup(t)
			t.Setenv("AEGIS_COOKIE_SECURE", tc.value)
			c, err := Load()
			if (err != nil) != tc.invalid || (!tc.invalid && c.CookieSecure != tc.want) {
				t.Fatalf("secure=%v err=%v", c.CookieSecure, err)
			}
		})
	}
}
func TestRejectUnsafeConfig(t *testing.T) {
	for _, tc := range []struct{ key, value string }{{"AEGIS_DATABASE_URL", ""}, {"AEGIS_DATABASE_URL", "sqlite://data/file.db"}, {"AEGIS_DATABASE_URL", "postgres://localhost"}, {"AEGIS_DATABASE_PATH", "data/old.db"}, {"AEGIS_REDIS_URL", ""}, {"AEGIS_REDIS_URL", "https://localhost"}, {"AEGIS_DB_MAX_OPEN", "0"}, {"AEGIS_DB_MAX_IDLE", "1001"}, {"AEGIS_DB_MAX_IDLE", "30"}, {"AEGIS_LOGIN_RATE_LIMIT", "0"}, {"AEGIS_LOGIN_RATE_LIMIT", "1001"}, {"AEGIS_LOGIN_RATE_LIMIT", "ten"}, {"AEGIS_TRUSTED_PROXIES", "any"}, {"AEGIS_JWT_SECRET", "short"}, {"AEGIS_ACCESS_TTL", "0s"}, {"AEGIS_ACCESS_TTL", "2h"}, {"AEGIS_REFRESH_TTL", "1m"}, {"AEGIS_BOOTSTRAP_USERNAME", "admin"}, {"AEGIS_ALLOWED_ORIGINS", "*"}, {"AEGIS_ALLOWED_ORIGINS", "https://example.com/path"}, {"AEGIS_HTTP_ADDR", "bad"}} {
		t.Run(tc.key+tc.value, func(t *testing.T) {
			setup(t)
			t.Setenv(tc.key, tc.value)
			if _, err := Load(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestBuildSandboxConfig(t *testing.T) {
	for _, tc := range []struct {
		value   string
		want    BuildSandbox
		invalid bool
	}{
		{"", BuildSandboxNone, false},
		{"none", BuildSandboxNone, false},
		{"user", "", true}, // AEGIS_BUILD_USER is required, covered below
		{"container", BuildSandboxContainer, false},
		{"docker", "", true},
		{"Container", "", true},
	} {
		t.Run("value="+tc.value, func(t *testing.T) {
			setup(t)
			t.Setenv("AEGIS_BUILD_SANDBOX", tc.value)
			c, err := Load()
			if (err != nil) != tc.invalid || (!tc.invalid && c.BuildSandbox != tc.want) {
				t.Fatalf("sandbox=%q err=%v", c.BuildSandbox, err)
			}
			if tc.want == BuildSandboxContainer {
				if c.BuildImage != "alpine:3.20" || c.BuildCPU != "2" || c.BuildMemory != "2g" || c.BuildPids != 512 {
					t.Fatalf("container defaults = image %q cpu %q memory %q pids %d", c.BuildImage, c.BuildCPU, c.BuildMemory, c.BuildPids)
				}
			}
		})
	}
}

func TestBuildUserSandboxConfig(t *testing.T) {
	t.Run("requires an account name", func(t *testing.T) {
		setup(t)
		t.Setenv("AEGIS_BUILD_SANDBOX", "user")
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "AEGIS_BUILD_USER") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("accepts a dedicated account", func(t *testing.T) {
		setup(t)
		t.Setenv("AEGIS_BUILD_SANDBOX", "user")
		t.Setenv("AEGIS_BUILD_USER", "aegis-builder")
		c, err := Load()
		if err != nil || c.BuildSandbox != BuildSandboxUser || c.BuildUser != "aegis-builder" {
			t.Fatalf("sandbox=%q user=%q err=%v", c.BuildSandbox, c.BuildUser, err)
		}
	})
	t.Run("account only applies to user mode", func(t *testing.T) {
		setup(t)
		t.Setenv("AEGIS_BUILD_SANDBOX", "none")
		t.Setenv("AEGIS_BUILD_USER", "aegis-builder")
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "only applies") {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestBuildDiskConfig(t *testing.T) {
	for _, tc := range []struct {
		value   string
		want    int64
		invalid bool
	}{
		{"", 2 << 30, false},
		{"512m", 512 << 20, false},
		{"1g", 1 << 30, false},
		{"1t", 1 << 40, false},
		{"16m", 16 << 20, false},
		{"8m", 0, true},
		{"0g", 0, true},
		{"2gb", 0, true},
		{"2", 0, true},
		{"-1g", 0, true},
	} {
		t.Run("value="+tc.value, func(t *testing.T) {
			setup(t)
			t.Setenv("AEGIS_BUILD_DISK", tc.value)
			c, err := Load()
			if (err != nil) != tc.invalid || (!tc.invalid && c.BuildDisk != tc.want) {
				t.Fatalf("disk=%d err=%v", c.BuildDisk, err)
			}
		})
	}
}

func TestBuildCloneConfig(t *testing.T) {
	for _, tc := range []struct {
		name      string
		clone     string
		allow     string
		wantClone bool
		invalid   bool
	}{
		{"defaults", "", "", false, false},
		{"clone on", "true", "", true, false},
		{"local requires clone", "false", "true", false, true},
		{"local with clone", "true", "true", true, false},
		{"bad boolean", "yes", "", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup(t)
			t.Setenv("AEGIS_BUILD_CLONE", tc.clone)
			t.Setenv("AEGIS_BUILD_ALLOW_LOCAL_REPO", tc.allow)
			c, err := Load()
			if (err != nil) != tc.invalid || (!tc.invalid && c.BuildClone != tc.wantClone) {
				t.Fatalf("clone=%v err=%v", c.BuildClone, err)
			}
		})
	}
}

func TestBuildContainerLimitConfig(t *testing.T) {
	for _, tc := range []struct {
		key, value string
		invalid    bool
	}{
		{"AEGIS_BUILD_CPU", "1.5", false},
		{"AEGIS_BUILD_CPU", "0", true},
		{"AEGIS_BUILD_CPU", "65", true},
		{"AEGIS_BUILD_CPU", "two", true},
		{"AEGIS_BUILD_MEMORY", "512m", false},
		{"AEGIS_BUILD_MEMORY", "3g", false},
		{"AEGIS_BUILD_MEMORY", "0g", true},
		{"AEGIS_BUILD_MEMORY", "2gb", true},
		{"AEGIS_BUILD_MEMORY", "2048", true},
		{"AEGIS_BUILD_PIDS", "1024", false},
		{"AEGIS_BUILD_PIDS", "8", true},
		{"AEGIS_BUILD_PIDS", "40000", true},
		{"AEGIS_BUILD_IMAGE", "alpine:3.20", false},
		{"AEGIS_BUILD_IMAGE", " alpine:3.20", true},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			setup(t)
			t.Setenv("AEGIS_BUILD_SANDBOX", "container")
			t.Setenv(tc.key, tc.value)
			if _, err := Load(); (err != nil) != tc.invalid {
				t.Fatalf("err=%v invalid=%v", err, tc.invalid)
			}
		})
	}
}

func TestAuditRetentionConfig(t *testing.T) {
	for _, tc := range []struct {
		value   string
		want    time.Duration
		invalid bool
	}{
		{"", 0, false},
		{"0", 0, false},
		{"24h", 24 * time.Hour, false},
		{"2160h", 2160 * time.Hour, false},
		{"8760h", 8760 * time.Hour, false},
		{"23h", 0, true},
		{"8761h", 0, true},
		{"30d", 0, true},
		{"-24h", 0, true},
	} {
		t.Run("value="+tc.value, func(t *testing.T) {
			setup(t)
			t.Setenv("AEGIS_AUDIT_RETENTION", tc.value)
			c, err := Load()
			if (err != nil) != tc.invalid || (!tc.invalid && c.AuditRetention != tc.want) {
				t.Fatalf("retention=%v err=%v", c.AuditRetention, err)
			}
		})
	}
}

func TestMetricsAddrConfig(t *testing.T) {
	for _, tc := range []struct {
		value   string
		want    string
		invalid bool
	}{
		{"", "", false},
		{"127.0.0.1:9090", "127.0.0.1:9090", false},
		{"localhost:0", "localhost:0", false},
		{"[::1]:9090", "[::1]:9090", false},
		{"9090", "", true},
		{"127.0.0.1", "", true},
		{"127.0.0.1:abc", "", true},
		{"127.0.0.1:70000", "", true},
	} {
		t.Run("value="+tc.value, func(t *testing.T) {
			setup(t)
			t.Setenv("AEGIS_METRICS_ADDR", tc.value)
			c, err := Load()
			if (err != nil) != tc.invalid || (!tc.invalid && c.MetricsAddr != tc.want) {
				t.Fatalf("metricsAddr=%q err=%v", c.MetricsAddr, err)
			}
		})
	}
}

func TestSecretKeyConfig(t *testing.T) {
	encode := func(size int) string { return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, size)) }
	for _, tc := range []struct {
		name    string
		value   string
		wantLen int
		invalid bool
	}{
		{"unset disables secret storage", "", 0, false},
		{"valid 32-byte key", encode(32), 32, false},
		{"short key", encode(16), 0, true},
		{"long key", encode(33), 0, true},
		{"not base64", "!!!!", 0, true},
		{"base64 of non-32 bytes", base64.StdEncoding.EncodeToString([]byte("too-short")), 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup(t)
			t.Setenv("AEGIS_SECRET_KEY", tc.value)
			c, err := Load()
			if (err != nil) != tc.invalid {
				t.Fatalf("err=%v invalid=%v", err, tc.invalid)
			}
			if err != nil {
				if strings.Contains(err.Error(), tc.value) && tc.value != "" {
					t.Fatalf("error echoed the key: %v", err)
				}
				return
			}
			if len(c.SecretKey) != tc.wantLen {
				t.Fatalf("key length = %d, want %d", len(c.SecretKey), tc.wantLen)
			}
		})
	}
}

func TestPreviousSecretKeyConfig(t *testing.T) {
	encode := func(size int, fill byte) string {
		return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{fill}, size))
	}
	current, previous := encode(32, 1), encode(32, 2)
	for _, tc := range []struct {
		name     string
		current  string
		previous string
		wantPrev bool
		invalid  bool
	}{
		{"no keys", "", "", false, false},
		{"current only", current, "", false, false},
		{"rotation pair", current, previous, true, false},
		{"previous without current", "", previous, false, true},
		{"equal keys", current, current, false, true},
		{"previous not base64", current, "!!!!", false, true},
		{"previous of 16 bytes", current, encode(16, 3), false, true},
		{"previous of 33 bytes", current, encode(33, 3), false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup(t)
			t.Setenv("AEGIS_SECRET_KEY", tc.current)
			t.Setenv("AEGIS_SECRET_KEY_PREVIOUS", tc.previous)
			c, err := Load()
			if (err != nil) != tc.invalid {
				t.Fatalf("err=%v invalid=%v", err, tc.invalid)
			}
			if err != nil {
				return
			}
			if got := len(c.PreviousSecretKey) == 32; got != tc.wantPrev {
				t.Fatalf("previous key set = %v, want %v", got, tc.wantPrev)
			}
		})
	}
}

func TestDeployCheckTimeoutConfig(t *testing.T) {
	for _, tc := range []struct {
		value   string
		want    time.Duration
		invalid bool
	}{
		{"", 5 * time.Second, false},
		{"30s", 30 * time.Second, false},
		{"1m", time.Minute, false},
		{"2m", 0, true},
		{"0s", 0, true},
		{"500ms", 0, true},
		{"soon", 0, true},
	} {
		t.Run("value="+tc.value, func(t *testing.T) {
			setup(t)
			t.Setenv("AEGIS_DEPLOY_CHECK_TIMEOUT", tc.value)
			c, err := Load()
			if (err != nil) != tc.invalid || (!tc.invalid && c.DeployCheckTimeout != tc.want) {
				t.Fatalf("timeout=%v err=%v", c.DeployCheckTimeout, err)
			}
		})
	}
}

func TestDeployTimeoutConfig(t *testing.T) {
	for _, tc := range []struct {
		value   string
		want    time.Duration
		invalid bool
	}{
		{"", 10 * time.Minute, false},
		{"30s", 30 * time.Second, false},
		{"1h", time.Hour, false},
		{"2h", 0, true},
		{"0s", 0, true},
		{"500ms", 0, true},
		{"soon", 0, true},
	} {
		t.Run("value="+tc.value, func(t *testing.T) {
			setup(t)
			t.Setenv("AEGIS_DEPLOY_TIMEOUT", tc.value)
			c, err := Load()
			if (err != nil) != tc.invalid || (!tc.invalid && c.DeployTimeout != tc.want) {
				t.Fatalf("timeout=%v err=%v", c.DeployTimeout, err)
			}
		})
	}
}

func TestDeployHealthConfig(t *testing.T) {
	for _, tc := range []struct {
		attempts  string
		interval  string
		wantCount int
		wantEvery time.Duration
		invalid   bool
	}{
		{"", "", 3, 2 * time.Second, false},
		{"0", "", 0, 2 * time.Second, false},
		{"1", "5s", 1, 5 * time.Second, false},
		{"20", "1m", 20, time.Minute, false},
		{"21", "", 0, 0, true},
		{"-1", "", 0, 0, true},
		{"many", "", 0, 0, true},
		{"3", "500ms", 0, 0, true},
		{"3", "2m", 0, 0, true},
		{"3", "soon", 0, 0, true},
	} {
		t.Run("attempts="+tc.attempts+"/interval="+tc.interval, func(t *testing.T) {
			setup(t)
			t.Setenv("AEGIS_DEPLOY_HEALTH_ATTEMPTS", tc.attempts)
			t.Setenv("AEGIS_DEPLOY_HEALTH_INTERVAL", tc.interval)
			c, err := Load()
			if (err != nil) != tc.invalid {
				t.Fatalf("err = %v, want invalid=%v", err, tc.invalid)
			}
			if tc.invalid {
				return
			}
			if c.DeployHealthAttempts != tc.wantCount || c.DeployHealthInterval != tc.wantEvery {
				t.Fatalf("attempts=%d interval=%v, want %d/%v", c.DeployHealthAttempts, c.DeployHealthInterval, tc.wantCount, tc.wantEvery)
			}
		})
	}
}

func TestApprovalConfig(t *testing.T) {
	t.Run("unset defaults to production", func(t *testing.T) {
		setup(t)
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(c.ApprovalEnvironments, []string{"production"}) || c.ApprovalTTL != 24*time.Hour {
			t.Fatalf("approval defaults = %v ttl=%v", c.ApprovalEnvironments, c.ApprovalTTL)
		}
	})
	for _, tc := range []struct {
		name    string
		value   string
		want    []string
		invalid bool
	}{
		{"empty disables approvals", "", []string{}, false},
		{"comma only disables approvals", ",", []string{}, false},
		{"spaces and commas disable approvals", "  ,  ", []string{}, false},
		{"single staging", "staging", []string{"staging"}, false},
		{"trim and dedupe", " development , production ,production", []string{"development", "production"}, false},
		{"unknown environment", "qa", nil, true},
		{"wrong case", "Production", nil, true},
		{"one unknown among valid", "staging,qa", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup(t)
			t.Setenv("AEGIS_DEPLOY_APPROVAL_ENVIRONMENTS", tc.value)
			c, err := Load()
			if (err != nil) != tc.invalid {
				t.Fatalf("environments=%v err=%v", c.ApprovalEnvironments, err)
			}
			if tc.invalid {
				return
			}
			if !slices.Equal(c.ApprovalEnvironments, tc.want) {
				t.Fatalf("environments=%v want=%v", c.ApprovalEnvironments, tc.want)
			}
			// P1-02: an explicitly set variable must never look unset. A nil
			// result makes deployments.New fall back to ["production"], so an
			// explicitly empty value must be a non-nil, zero-length slice.
			if c.ApprovalEnvironments == nil {
				t.Fatalf("explicit AEGIS_DEPLOY_APPROVAL_ENVIRONMENTS=%q produced nil; want non-nil empty slice", tc.value)
			}
		})
	}
	for _, tc := range []struct {
		value   string
		want    time.Duration
		invalid bool
	}{
		{"", 24 * time.Hour, false},
		{"1h", time.Hour, false},
		{"168h", 168 * time.Hour, false},
		{"30m", 0, true},
		{"169h", 0, true},
		{"0s", 0, true},
		{"soon", 0, true},
	} {
		t.Run("ttl="+tc.value, func(t *testing.T) {
			setup(t)
			t.Setenv("AEGIS_APPROVAL_TTL", tc.value)
			c, err := Load()
			if (err != nil) != tc.invalid || (!tc.invalid && c.ApprovalTTL != tc.want) {
				t.Fatalf("ttl=%v err=%v", c.ApprovalTTL, err)
			}
		})
	}
}

func TestRequireReviewEnvironmentsConfig(t *testing.T) {
	// The review gate is opt-in: both an unset variable and an explicitly
	// empty value must leave it disabled, unlike the approval default.
	t.Run("unset disables the gate", func(t *testing.T) {
		setup(t)
		unsetEnv(t, "AEGIS_DEPLOY_REQUIRE_REVIEW_ENVIRONMENTS")
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if len(c.RequireReviewEnvironments) != 0 {
			t.Fatalf("review environments = %v, want none", c.RequireReviewEnvironments)
		}
	})
	for _, tc := range []struct {
		name    string
		value   string
		want    []string
		invalid bool
	}{
		{"explicit empty disables the gate", "", []string{}, false},
		{"comma only disables the gate", ",", []string{}, false},
		{"spaces and commas disable the gate", "  ,  ", []string{}, false},
		{"single production", "production", []string{"production"}, false},
		{"all three", "production,staging,development", []string{"production", "staging", "development"}, false},
		{"trim and dedupe", " development , production ,production", []string{"development", "production"}, false},
		{"unknown environment", "qa", nil, true},
		{"wrong case", "Production", nil, true},
		{"one unknown among valid", "staging,qa", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup(t)
			t.Setenv("AEGIS_DEPLOY_REQUIRE_REVIEW_ENVIRONMENTS", tc.value)
			c, err := Load()
			if (err != nil) != tc.invalid {
				t.Fatalf("environments=%v err=%v", c.RequireReviewEnvironments, err)
			}
			if tc.invalid {
				return
			}
			if !slices.Equal(c.RequireReviewEnvironments, tc.want) {
				t.Fatalf("environments=%v want=%v", c.RequireReviewEnvironments, tc.want)
			}
			if c.RequireReviewEnvironments == nil {
				t.Fatalf("explicit AEGIS_DEPLOY_REQUIRE_REVIEW_ENVIRONMENTS=%q produced nil; want non-nil empty slice", tc.value)
			}
		})
	}
	// The gate is independent of the approval policy: enabling one must not
	// change the other's parsed value.
	t.Run("independent of approval environments", func(t *testing.T) {
		setup(t)
		t.Setenv("AEGIS_DEPLOY_APPROVAL_ENVIRONMENTS", "")
		t.Setenv("AEGIS_DEPLOY_REQUIRE_REVIEW_ENVIRONMENTS", "production")
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if len(c.ApprovalEnvironments) != 0 || !slices.Equal(c.RequireReviewEnvironments, []string{"production"}) {
			t.Fatalf("approval=%v review=%v", c.ApprovalEnvironments, c.RequireReviewEnvironments)
		}
	})
}

func TestApprovalWebhookConfig(t *testing.T) {
	t.Run("https with signing secret", func(t *testing.T) {
		setup(t)
		t.Setenv("AEGIS_APPROVAL_WEBHOOK_URL", "https://hooks.example.com/aegis/approvals")
		t.Setenv("AEGIS_APPROVAL_WEBHOOK_SECRET", "0123456789abcdef")
		t.Setenv("AEGIS_APPROVAL_WEBHOOK_TIMEOUT", "7s")
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.ApprovalWebhookURL != "https://hooks.example.com/aegis/approvals" ||
			string(c.ApprovalWebhookSecret) != "0123456789abcdef" || c.ApprovalWebhookTimeout != 7*time.Second {
			t.Fatalf("webhook config = %+v", c)
		}
	})
	t.Run("loopback http is allowed", func(t *testing.T) {
		setup(t)
		t.Setenv("AEGIS_APPROVAL_WEBHOOK_URL", "http://127.0.0.1:9099/hook")
		c, err := Load()
		if err != nil || c.ApprovalWebhookURL != "http://127.0.0.1:9099/hook" || c.ApprovalWebhookTimeout != 5*time.Second {
			t.Fatalf("loopback webhook = %+v err=%v", c, err)
		}
	})
	for _, tc := range []struct {
		name, url, secret, timeout string
	}{
		{"plain http remote", "http://hooks.example.com/aegis", "", ""},
		{"unsupported scheme", "ftp://hooks.example.com/aegis", "", ""},
		{"userinfo", "https://user:pass@hooks.example.com/aegis", "", ""},
		{"empty host", "https:///aegis", "", ""},
		{"fragment", "https://hooks.example.com/aegis#x", "", ""},
		{"secret without url", "", "0123456789abcdef", ""},
		{"short secret", "https://hooks.example.com/aegis", "short", ""},
		{"timeout too long", "https://hooks.example.com/aegis", "", "31s"},
		{"invalid timeout", "https://hooks.example.com/aegis", "", "soon"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup(t)
			t.Setenv("AEGIS_APPROVAL_WEBHOOK_URL", tc.url)
			t.Setenv("AEGIS_APPROVAL_WEBHOOK_SECRET", tc.secret)
			t.Setenv("AEGIS_APPROVAL_WEBHOOK_TIMEOUT", tc.timeout)
			if _, err := Load(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

// TestReviewWebhookSecretConfig covers the inbound external-result signing key:
// unset keeps that endpoint closed (503) while everything else still loads; a
// configured value must be 16-256 bytes. It is independent of the outbound
// approval webhook configuration.
func TestReviewWebhookSecretConfig(t *testing.T) {
	t.Run("unset disables external results", func(t *testing.T) {
		setup(t)
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if len(c.ReviewWebhookSecret) != 0 {
			t.Fatalf("review webhook secret = %q, want unset", c.ReviewWebhookSecret)
		}
	})
	t.Run("valid secret", func(t *testing.T) {
		setup(t)
		t.Setenv("AEGIS_REVIEW_WEBHOOK_SECRET", "0123456789abcdef")
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if string(c.ReviewWebhookSecret) != "0123456789abcdef" {
			t.Fatalf("review webhook secret = %q", c.ReviewWebhookSecret)
		}
	})
	t.Run("maximum length", func(t *testing.T) {
		setup(t)
		t.Setenv("AEGIS_REVIEW_WEBHOOK_SECRET", strings.Repeat("k", 256))
		c, err := Load()
		if err != nil || len(c.ReviewWebhookSecret) != 256 {
			t.Fatalf("review webhook secret length = %d err=%v", len(c.ReviewWebhookSecret), err)
		}
	})
	for _, tc := range []struct{ name, secret string }{
		{"too short", "0123456789abcde"},
		{"too long", strings.Repeat("k", 257)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup(t)
			t.Setenv("AEGIS_REVIEW_WEBHOOK_SECRET", tc.secret)
			if _, err := Load(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

// TestIngestWebhookSecretConfig covers the scan/security-event signing key: it is
// independent from the review webhook secret, unset keeps the ingest endpoints
// closed (503) while everything else loads, and a configured value must be
// 16-256 bytes.
func TestIngestWebhookSecretConfig(t *testing.T) {
	t.Run("unset closes ingest", func(t *testing.T) {
		setup(t)
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if len(c.IngestWebhookSecret) != 0 || len(c.ReviewWebhookSecret) != 0 {
			t.Fatalf("ingest secret = %q, review secret = %q, want both unset", c.IngestWebhookSecret, c.ReviewWebhookSecret)
		}
	})
	t.Run("valid secret", func(t *testing.T) {
		setup(t)
		t.Setenv("AEGIS_INGEST_WEBHOOK_SECRET", "0123456789abcdef")
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if string(c.IngestWebhookSecret) != "0123456789abcdef" || len(c.ReviewWebhookSecret) != 0 {
			t.Fatalf("ingest secret = %q review secret = %q", c.IngestWebhookSecret, c.ReviewWebhookSecret)
		}
	})
	t.Run("maximum length", func(t *testing.T) {
		setup(t)
		t.Setenv("AEGIS_INGEST_WEBHOOK_SECRET", strings.Repeat("k", 256))
		c, err := Load()
		if err != nil || len(c.IngestWebhookSecret) != 256 {
			t.Fatalf("ingest secret length = %d err=%v", len(c.IngestWebhookSecret), err)
		}
	})
	for _, tc := range []struct{ name, secret string }{
		{"too short", "0123456789abcde"},
		{"too long", strings.Repeat("k", 257)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup(t)
			t.Setenv("AEGIS_INGEST_WEBHOOK_SECRET", tc.secret)
			if _, err := Load(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

// TestOIDCConfig covers the opt-in OIDC single sign-on configuration: unset
// issuer keeps the feature fully disabled (and any other OIDC variable then
// fails closed), while an enabled deployment gets validated URLs, scopes and
// the documented defaults.
func TestOIDCConfig(t *testing.T) {
	t.Run("unset disables the feature", func(t *testing.T) {
		setup(t)
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.OIDC.Enabled() || c.OIDC.Issuer != "" || len(c.OIDC.Scopes) != 0 {
			t.Fatalf("disabled OIDC config = %+v", c.OIDC)
		}
	})
	t.Run("enabled defaults", func(t *testing.T) {
		setup(t)
		t.Setenv("AEGIS_OIDC_ISSUER", "https://idp.example.com/tenant/")
		t.Setenv("AEGIS_OIDC_CLIENT_ID", "aegisflow")
		t.Setenv("AEGIS_OIDC_REDIRECT_URL", "https://aegis.example.com/api/v1/auth/oidc/callback")
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if !c.OIDC.Enabled() || c.OIDC.Issuer != "https://idp.example.com/tenant" {
			t.Fatalf("issuer = %q", c.OIDC.Issuer)
		}
		if c.OIDC.Label != "企业单点登录" || c.OIDC.AutoProvision || c.OIDC.DiscoveryTimeout != 5*time.Second {
			t.Fatalf("defaults = %+v", c.OIDC)
		}
		if !slices.Equal(c.OIDC.Scopes, []string{"openid", "profile", "email"}) {
			t.Fatalf("scopes = %v", c.OIDC.Scopes)
		}
	})
	t.Run("loopback http and explicit options", func(t *testing.T) {
		setup(t)
		t.Setenv("AEGIS_OIDC_ISSUER", "http://127.0.0.1:9099")
		t.Setenv("AEGIS_OIDC_CLIENT_ID", "aegisflow-e2e")
		t.Setenv("AEGIS_OIDC_CLIENT_SECRET", "test-client-secret")
		t.Setenv("AEGIS_OIDC_REDIRECT_URL", "http://localhost:5173/api/v1/auth/oidc/callback")
		t.Setenv("AEGIS_OIDC_SCOPES", "openid profile email groups")
		t.Setenv("AEGIS_OIDC_AUTO_PROVISION", "true")
		t.Setenv("AEGIS_OIDC_LABEL", "公司 SSO")
		t.Setenv("AEGIS_OIDC_DISCOVERY_TIMEOUT", "10s")
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.OIDC.ClientSecret != "test-client-secret" || !c.OIDC.AutoProvision || c.OIDC.Label != "公司 SSO" || c.OIDC.DiscoveryTimeout != 10*time.Second {
			t.Fatalf("explicit options = %+v", c.OIDC)
		}
		if !slices.Contains(c.OIDC.Scopes, "groups") || len(c.OIDC.Scopes) != 4 {
			t.Fatalf("scopes = %v", c.OIDC.Scopes)
		}
	})
	t.Run("duplicate scopes are collapsed", func(t *testing.T) {
		setup(t)
		t.Setenv("AEGIS_OIDC_ISSUER", "https://idp.example.com")
		t.Setenv("AEGIS_OIDC_CLIENT_ID", "aegisflow")
		t.Setenv("AEGIS_OIDC_REDIRECT_URL", "https://aegis.example.com/api/v1/auth/oidc/callback")
		t.Setenv("AEGIS_OIDC_SCOPES", "openid openid profile")
		c, err := Load()
		if err != nil || !slices.Equal(c.OIDC.Scopes, []string{"openid", "profile"}) {
			t.Fatalf("scopes = %v err=%v", c.OIDC.Scopes, err)
		}
	})
	// Partial configuration without the issuer must fail closed instead of
	// silently starting a server the operator believes has SSO enabled.
	for _, tc := range []struct{ name, key, value string }{
		{"client id without issuer", "AEGIS_OIDC_CLIENT_ID", "aegisflow"},
		{"secret without issuer", "AEGIS_OIDC_CLIENT_SECRET", "secret"},
		{"redirect without issuer", "AEGIS_OIDC_REDIRECT_URL", "https://aegis.example.com/cb"},
		{"label without issuer", "AEGIS_OIDC_LABEL", "SSO"},
		{"scopes without issuer", "AEGIS_OIDC_SCOPES", "openid profile"},
		{"auto provision without issuer", "AEGIS_OIDC_AUTO_PROVISION", "true"},
		{"timeout without issuer", "AEGIS_OIDC_DISCOVERY_TIMEOUT", "5s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup(t)
			t.Setenv(tc.key, tc.value)
			if _, err := Load(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
	// Enabled deployments with unsafe values must also fail startup.
	for _, tc := range []struct{ name, key, value string }{
		{"issuer plain http", "AEGIS_OIDC_ISSUER", "http://idp.example.com"},
		{"issuer userinfo", "AEGIS_OIDC_ISSUER", "https://user:pass@idp.example.com"},
		{"issuer fragment", "AEGIS_OIDC_ISSUER", "https://idp.example.com/#fragment"},
		{"issuer query", "AEGIS_OIDC_ISSUER", "https://idp.example.com?x=1"},
		{"issuer ftp", "AEGIS_OIDC_ISSUER", "ftp://idp.example.com"},
		{"scopes missing openid", "AEGIS_OIDC_SCOPES", "profile email"},
		{"invalid auto provision", "AEGIS_OIDC_AUTO_PROVISION", "yes"},
		{"discovery timeout too large", "AEGIS_OIDC_DISCOVERY_TIMEOUT", "31s"},
		{"client id too long", "AEGIS_OIDC_CLIENT_ID", strings.Repeat("a", 257)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup(t)
			t.Setenv("AEGIS_OIDC_ISSUER", "https://idp.example.com")
			t.Setenv("AEGIS_OIDC_CLIENT_ID", "aegisflow")
			t.Setenv("AEGIS_OIDC_REDIRECT_URL", "https://aegis.example.com/api/v1/auth/oidc/callback")
			t.Setenv(tc.key, tc.value)
			if _, err := Load(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
	t.Run("redirect URL unsafe forms rejected", func(t *testing.T) {
		for _, redirect := range []string{
			"http://aegis.example.com/cb",
			"https://user:pass@aegis.example.com/cb",
			"https://aegis.example.com/cb#fragment",
			"https://aegis.example.com/cb?query=1",
			"relative/cb",
		} {
			setup(t)
			t.Setenv("AEGIS_OIDC_ISSUER", "https://idp.example.com")
			t.Setenv("AEGIS_OIDC_CLIENT_ID", "aegisflow")
			t.Setenv("AEGIS_OIDC_REDIRECT_URL", redirect)
			if _, err := Load(); err == nil {
				t.Fatalf("redirect %q accepted", redirect)
			}
		}
	})
}

// TestDefaultTenantSlugConfig pins the AEGIS_DEFAULT_TENANT_SLUG contract: the
// documented slug shape (1-32 lowercase letters, digits and inner hyphens) and
// the "default" fallback when the variable is unset.
func TestDefaultTenantSlugConfig(t *testing.T) {
	for _, tc := range []struct {
		value   string
		want    string
		invalid bool
	}{
		{"", "default", false},
		{"default", "default", false},
		{"acme", "acme", false},
		{"acme-corp", "acme-corp", false},
		{"a", "a", false},
		{"team-01", "team-01", false},
		{strings.Repeat("a", 32), strings.Repeat("a", 32), false},
		{"-acme", "", true},
		{"acme-", "", true},
		{"Acme", "", true},
		{"acme corp", "", true},
		{"acme_corp", "", true},
		{"中", "", true},
		{strings.Repeat("a", 33), "", true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			setup(t)
			t.Setenv("AEGIS_DEFAULT_TENANT_SLUG", tc.value)
			c, err := Load()
			if tc.invalid {
				if err == nil {
					t.Fatalf("slug %q accepted", tc.value)
				}
				return
			}
			if err != nil {
				t.Fatalf("slug %q rejected: %v", tc.value, err)
			}
			if c.DefaultTenantSlug != tc.want {
				t.Fatalf("DefaultTenantSlug = %q, want %q", c.DefaultTenantSlug, tc.want)
			}
		})
	}
}
