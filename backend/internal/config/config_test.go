package config

import (
	"strings"
	"testing"
	"time"
)

func setup(t *testing.T) {
	t.Helper()
	for _, key := range []string{"AEGIS_HTTP_ADDR", "AEGIS_DATABASE_PATH", "AEGIS_BOOTSTRAP_USERNAME", "AEGIS_BOOTSTRAP_PASSWORD", "AEGIS_ACCESS_TTL", "AEGIS_REFRESH_TTL", "AEGIS_ALLOWED_ORIGINS"} {
		t.Setenv(key, "")
	}
	t.Setenv("AEGIS_JWT_SECRET", strings.Repeat("s", 32))
}
func TestLoad(t *testing.T) {
	setup(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.AccessTTL != 15*time.Minute || c.Address != "127.0.0.1:8080" {
		t.Fatalf("unexpected config: %+v", c)
	}
}
func TestRejectUnsafeConfig(t *testing.T) {
	for _, tc := range []struct{ key, value string }{{"AEGIS_JWT_SECRET", "short"}, {"AEGIS_ACCESS_TTL", "0s"}, {"AEGIS_ACCESS_TTL", "2h"}, {"AEGIS_REFRESH_TTL", "1m"}, {"AEGIS_BOOTSTRAP_USERNAME", "admin"}, {"AEGIS_ALLOWED_ORIGINS", "*"}, {"AEGIS_ALLOWED_ORIGINS", "https://example.com/path"}, {"AEGIS_HTTP_ADDR", "bad"}} {
		t.Run(tc.key+tc.value, func(t *testing.T) {
			setup(t)
			t.Setenv(tc.key, tc.value)
			if _, err := Load(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}
