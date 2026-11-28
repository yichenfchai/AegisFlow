package config

import (
	"strings"
	"testing"
	"time"
)

// TestLDAPConfig covers the opt-in LDAP directory authentication configuration:
// an unset URL keeps the feature fully disabled (and any other AEGIS_LDAP_*
// variable then fails closed), while an enabled deployment gets validated URLs,
// TLS rules, DNs, filter, attributes and the documented defaults.
func TestLDAPConfig(t *testing.T) {
	t.Run("unset disables the feature", func(t *testing.T) {
		setup(t)
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.LDAP.Enabled() || c.LDAP.URL != "" || c.LDAP.Timeout != 0 {
			t.Fatalf("disabled LDAP config = %+v", c.LDAP)
		}
	})
	t.Run("enabled defaults", func(t *testing.T) {
		setup(t)
		t.Setenv("AEGIS_LDAP_URL", "ldaps://directory.example.com")
		t.Setenv("AEGIS_LDAP_BASE_DN", "dc=example,dc=com")
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if !c.LDAP.Enabled() || c.LDAP.URL != "ldaps://directory.example.com" || c.LDAP.StartTLS || c.LDAP.AutoProvision {
			t.Fatalf("enabled config = %+v", c.LDAP)
		}
		if c.LDAP.UserFilter != "(uid={username})" || c.LDAP.UsernameAttribute != "uid" ||
			c.LDAP.EmailAttribute != "mail" || c.LDAP.DisplayNameAttribute != "cn" {
			t.Fatalf("defaults = %+v", c.LDAP)
		}
		if c.LDAP.Label != "LDAP 登录" || c.LDAP.Timeout != 5*time.Second {
			t.Fatalf("default label/timeout = %q / %v", c.LDAP.Label, c.LDAP.Timeout)
		}
		if c.LDAP.BindDN != "" || c.LDAP.BindPassword != "" {
			t.Fatalf("unexpected anonymous-search config: %+v", c.LDAP)
		}
	})
	t.Run("full custom configuration", func(t *testing.T) {
		setup(t)
		t.Setenv("AEGIS_LDAP_URL", "ldap://10.0.0.5:3389/")
		t.Setenv("AEGIS_LDAP_STARTTLS", "true")
		t.Setenv("AEGIS_LDAP_BIND_DN", "cn=svc,ou=services,dc=example,dc=com")
		t.Setenv("AEGIS_LDAP_BIND_PASSWORD", "service-secret")
		t.Setenv("AEGIS_LDAP_BASE_DN", "ou=people,dc=example,dc=com")
		t.Setenv("AEGIS_LDAP_USER_FILTER", "(&(objectClass=person)(sAMAccountName={username}))")
		t.Setenv("AEGIS_LDAP_USERNAME_ATTRIBUTE", "sAMAccountName")
		t.Setenv("AEGIS_LDAP_EMAIL_ATTRIBUTE", "mail;binary")
		t.Setenv("AEGIS_LDAP_DISPLAY_NAME_ATTRIBUTE", "displayName")
		t.Setenv("AEGIS_LDAP_AUTO_PROVISION", "true")
		t.Setenv("AEGIS_LDAP_LABEL", "企业目录")
		t.Setenv("AEGIS_LDAP_TIMEOUT", "12s")
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.LDAP.URL != "ldap://10.0.0.5:3389" || !c.LDAP.StartTLS || !c.LDAP.AutoProvision {
			t.Fatalf("custom config = %+v", c.LDAP)
		}
		if c.LDAP.BindDN != "cn=svc,ou=services,dc=example,dc=com" || c.LDAP.BindPassword != "service-secret" {
			t.Fatalf("service account = %q", c.LDAP.BindDN)
		}
		if c.LDAP.UserFilter != "(&(objectClass=person)(sAMAccountName={username}))" ||
			c.LDAP.UsernameAttribute != "sAMAccountName" || c.LDAP.EmailAttribute != "mail;binary" ||
			c.LDAP.DisplayNameAttribute != "displayName" {
			t.Fatalf("filter/attributes = %+v", c.LDAP)
		}
		if c.LDAP.Label != "企业目录" || c.LDAP.Timeout != 12*time.Second {
			t.Fatalf("label/timeout = %q / %v", c.LDAP.Label, c.LDAP.Timeout)
		}
	})
	// Auto-provisioning never picks a tenant from the request in the no-slug
	// path; it writes to the deployment's default tenant, which startup
	// guarantees. The configuration must therefore keep the two settings
	// independent: enabling auto-provision does not change the default slug.
	t.Run("auto provision keeps the default tenant", func(t *testing.T) {
		setup(t)
		t.Setenv("AEGIS_LDAP_URL", "ldaps://directory.example.com")
		t.Setenv("AEGIS_LDAP_BASE_DN", "dc=example,dc=com")
		t.Setenv("AEGIS_LDAP_AUTO_PROVISION", "true")
		t.Setenv("AEGIS_DEFAULT_TENANT_SLUG", "acme")
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if !c.LDAP.AutoProvision || c.DefaultTenantSlug != "acme" {
			t.Fatalf("auto provision = %v default tenant = %q", c.LDAP.AutoProvision, c.DefaultTenantSlug)
		}
	})
	t.Run("partial configuration fails closed", func(t *testing.T) {
		for _, tc := range []struct{ name, key, value string }{
			{"starttls without url", "AEGIS_LDAP_STARTTLS", "true"},
			{"bind dn without url", "AEGIS_LDAP_BIND_DN", "cn=svc,dc=example,dc=com"},
			{"bind password without url", "AEGIS_LDAP_BIND_PASSWORD", "secret"},
			{"base dn without url", "AEGIS_LDAP_BASE_DN", "dc=example,dc=com"},
			{"filter without url", "AEGIS_LDAP_USER_FILTER", "(uid={username})"},
			{"username attribute without url", "AEGIS_LDAP_USERNAME_ATTRIBUTE", "uid"},
			{"email attribute without url", "AEGIS_LDAP_EMAIL_ATTRIBUTE", "mail"},
			{"display attribute without url", "AEGIS_LDAP_DISPLAY_NAME_ATTRIBUTE", "cn"},
			{"auto provision without url", "AEGIS_LDAP_AUTO_PROVISION", "true"},
			{"label without url", "AEGIS_LDAP_LABEL", "LDAP"},
			{"timeout without url", "AEGIS_LDAP_TIMEOUT", "5s"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				setup(t)
				t.Setenv(tc.key, tc.value)
				if _, err := Load(); err == nil {
					t.Fatal("expected validation error")
				}
			})
		}
	})
	t.Run("unsafe URLs rejected", func(t *testing.T) {
		for _, tc := range []struct{ name, value string }{
			{"empty host", "ldap://"},
			{"ftp scheme", "ftp://directory.example.com"},
			{"http scheme", "https://directory.example.com"},
			{"userinfo", "ldaps://user:pass@directory.example.com"},
			{"fragment", "ldaps://directory.example.com/#fragment"},
			{"query", "ldaps://directory.example.com?x=1"},
			{"path", "ldaps://directory.example.com/dc=example,dc=com"},
			{"non numeric port", "ldaps://directory.example.com:ldap"},
			{"port out of range", "ldaps://directory.example.com:70000"},
			{"whitespace", "ldaps://directory.example.com/ bad"},
			{"plain http remote", "ldap://directory.example.com"},
			{"too long", "ldaps://" + strings.Repeat("a", 2050)},
		} {
			t.Run(tc.name, func(t *testing.T) {
				setup(t)
				t.Setenv("AEGIS_LDAP_URL", tc.value)
				t.Setenv("AEGIS_LDAP_BASE_DN", "dc=example,dc=com")
				if _, err := Load(); err == nil {
					t.Fatalf("URL %q accepted", tc.value)
				}
			})
		}
	})
	t.Run("transport security rules", func(t *testing.T) {
		for _, tc := range []struct {
			name, url, startTLS string
			invalid             bool
		}{
			{"loopback plain allowed", "ldap://127.0.0.1:3389", "", false},
			{"localhost plain allowed", "ldap://localhost:3389", "", false},
			{"remote plain needs starttls", "ldap://directory.example.com", "", true},
			{"remote plain with starttls", "ldap://directory.example.com", "true", false},
			{"ldaps with starttls", "ldaps://directory.example.com", "true", true},
			{"ldaps without starttls", "ldaps://directory.example.com", "false", false},
			{"invalid starttls boolean", "ldaps://directory.example.com", "yes", true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				setup(t)
				t.Setenv("AEGIS_LDAP_URL", tc.url)
				t.Setenv("AEGIS_LDAP_BASE_DN", "dc=example,dc=com")
				t.Setenv("AEGIS_LDAP_STARTTLS", tc.startTLS)
				if _, err := Load(); (err != nil) != tc.invalid {
					t.Fatalf("url=%q starttls=%q err=%v invalid=%v", tc.url, tc.startTLS, err, tc.invalid)
				}
			})
		}
	})
	t.Run("service bind rules", func(t *testing.T) {
		for _, tc := range []struct {
			name, dn, password string
			invalid            bool
		}{
			{"anonymous search", "", "", false},
			{"valid pair", "cn=svc,dc=example,dc=com", "secret", false},
			{"dn without password", "cn=svc,dc=example,dc=com", "", true},
			{"password without dn", "", "secret", true},
			{"invalid dn", "not a dn", "secret", true},
			{"control character in password", "cn=svc,dc=example,dc=com", "sec\x01ret", true},
			{"oversized password", "cn=svc,dc=example,dc=com", strings.Repeat("k", 1025), true},
			{"oversized dn", "cn=" + strings.Repeat("a", 1100) + ",dc=example,dc=com", "secret", true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				setup(t)
				t.Setenv("AEGIS_LDAP_URL", "ldaps://directory.example.com")
				t.Setenv("AEGIS_LDAP_BASE_DN", "dc=example,dc=com")
				t.Setenv("AEGIS_LDAP_BIND_DN", tc.dn)
				t.Setenv("AEGIS_LDAP_BIND_PASSWORD", tc.password)
				if _, err := Load(); (err != nil) != tc.invalid {
					t.Fatalf("dn=%q err=%v invalid=%v", tc.dn, err, tc.invalid)
				}
			})
		}
	})
	t.Run("base DN required", func(t *testing.T) {
		for _, base := range []string{"", "   ", "not a dn", "dc=" + strings.Repeat("a", 1100)} {
			setup(t)
			t.Setenv("AEGIS_LDAP_URL", "ldaps://directory.example.com")
			t.Setenv("AEGIS_LDAP_BASE_DN", base)
			if _, err := Load(); err == nil {
				t.Fatalf("base DN %q accepted", base)
			}
		}
	})
	t.Run("user filter placeholder", func(t *testing.T) {
		for _, filter := range []string{
			"(uid=alice)",                             // no placeholder
			"(uid={username})(uid={username})",        // two placeholders
			"(uid={username}",                         // unbalanced
			"(uid=" + strings.Repeat("{username}", 1), // unbalanced value
		} {
			setup(t)
			t.Setenv("AEGIS_LDAP_URL", "ldaps://directory.example.com")
			t.Setenv("AEGIS_LDAP_BASE_DN", "dc=example,dc=com")
			t.Setenv("AEGIS_LDAP_USER_FILTER", filter)
			if _, err := Load(); err == nil {
				t.Fatalf("filter %q accepted", filter)
			}
		}
		for _, filter := range []string{
			"(uid={username})",
			"(sAMAccountName={username})",
			"(&(objectClass=person)(mail={username}))",
			"(|(uid={username})(cn=aegisflow))",
		} {
			setup(t)
			t.Setenv("AEGIS_LDAP_URL", "ldaps://directory.example.com")
			t.Setenv("AEGIS_LDAP_BASE_DN", "dc=example,dc=com")
			t.Setenv("AEGIS_LDAP_USER_FILTER", filter)
			c, err := Load()
			if err != nil || c.LDAP.UserFilter != filter {
				t.Fatalf("filter %q err=%v", filter, err)
			}
		}
	})
	t.Run("attributes validated", func(t *testing.T) {
		for _, tc := range []struct{ key, value string }{
			{"AEGIS_LDAP_USERNAME_ATTRIBUTE", "1uid"},
			{"AEGIS_LDAP_USERNAME_ATTRIBUTE", "uid name"},
			{"AEGIS_LDAP_EMAIL_ATTRIBUTE", "-mail"},
			{"AEGIS_LDAP_DISPLAY_NAME_ATTRIBUTE", "cn;"},
			{"AEGIS_LDAP_DISPLAY_NAME_ATTRIBUTE", strings.Repeat("a", 65)},
		} {
			setup(t)
			t.Setenv("AEGIS_LDAP_URL", "ldaps://directory.example.com")
			t.Setenv("AEGIS_LDAP_BASE_DN", "dc=example,dc=com")
			t.Setenv(tc.key, tc.value)
			if _, err := Load(); err == nil {
				t.Fatalf("%s=%q accepted", tc.key, tc.value)
			}
		}
	})
	t.Run("auto provision and label", func(t *testing.T) {
		setup(t)
		t.Setenv("AEGIS_LDAP_URL", "ldaps://directory.example.com")
		t.Setenv("AEGIS_LDAP_BASE_DN", "dc=example,dc=com")
		t.Setenv("AEGIS_LDAP_AUTO_PROVISION", "yes")
		if _, err := Load(); err == nil {
			t.Fatal("invalid boolean accepted")
		}
		setup(t)
		t.Setenv("AEGIS_LDAP_URL", "ldaps://directory.example.com")
		t.Setenv("AEGIS_LDAP_BASE_DN", "dc=example,dc=com")
		t.Setenv("AEGIS_LDAP_LABEL", strings.Repeat("a", 129))
		if _, err := Load(); err == nil {
			t.Fatal("oversized label accepted")
		}
	})
	t.Run("timeout bounding", func(t *testing.T) {
		for _, tc := range []struct {
			value   string
			want    time.Duration
			invalid bool
		}{
			{"", 5 * time.Second, false},
			{"1s", time.Second, false},
			{"30s", 30 * time.Second, false},
			{"31s", 0, true},
			{"0s", 0, true},
			{"500ms", 0, true},
			{"soon", 0, true},
		} {
			t.Run("value="+tc.value, func(t *testing.T) {
				setup(t)
				t.Setenv("AEGIS_LDAP_URL", "ldaps://directory.example.com")
				t.Setenv("AEGIS_LDAP_BASE_DN", "dc=example,dc=com")
				t.Setenv("AEGIS_LDAP_TIMEOUT", tc.value)
				c, err := Load()
				if (err != nil) != tc.invalid || (!tc.invalid && c.LDAP.Timeout != tc.want) {
					t.Fatalf("timeout=%v err=%v", c.LDAP.Timeout, err)
				}
			})
		}
	})
}
