package config

import (
	"strings"
	"testing"
	"time"
)

func setup(t *testing.T) {
	t.Helper()
	for _, key := range []string{"AEGIS_HTTP_ADDR", "AEGIS_DATABASE_PATH", "AEGIS_BOOTSTRAP_USERNAME", "AEGIS_BOOTSTRAP_PASSWORD", "AEGIS_ACCESS_TTL", "AEGIS_REFRESH_TTL", "AEGIS_ALLOWED_ORIGINS", "AEGIS_COOKIE_SECURE", "AEGIS_BUILD_SANDBOX"} {
		t.Setenv(key, "")
	}
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
		{"user", "", true},
		{"container", "", true},
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
			// An unimplemented mode must say so: silently accepting it would
			// promise isolation the executor does not provide.
			if tc.invalid && tc.value == "container" && !strings.Contains(err.Error(), "not implemented") {
				t.Fatalf("error should explain that the mode is not implemented: %v", err)
			}
		})
	}
}
