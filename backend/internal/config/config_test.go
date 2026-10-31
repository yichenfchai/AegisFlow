package config

import (
	"strings"
	"testing"
	"time"
)

func setup(t *testing.T) {
	t.Helper()
	for _, key := range []string{"AEGIS_HTTP_ADDR", "AEGIS_DATABASE_PATH", "AEGIS_BOOTSTRAP_USERNAME", "AEGIS_BOOTSTRAP_PASSWORD", "AEGIS_ACCESS_TTL", "AEGIS_REFRESH_TTL", "AEGIS_ALLOWED_ORIGINS", "AEGIS_COOKIE_SECURE", "AEGIS_BUILD_SANDBOX", "AEGIS_BUILD_IMAGE", "AEGIS_BUILD_CPU", "AEGIS_BUILD_MEMORY", "AEGIS_BUILD_PIDS", "AEGIS_BUILD_USER", "AEGIS_BUILD_DISK", "AEGIS_BUILD_CLONE", "AEGIS_BUILD_ALLOW_LOCAL_REPO", "AEGIS_AUDIT_RETENTION"} {
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
