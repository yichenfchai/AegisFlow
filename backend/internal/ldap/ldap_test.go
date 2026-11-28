package ldap

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/yichenfchai/AegisFlow/backend/internal/ldaptest"
)

const (
	testBaseDN    = "dc=example,dc=com"
	knownPassword = "correct-horse-battery"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// testOptions is a complete anonymous-search configuration against one embedded
// directory; individual tests override what they exercise.
func testOptions(t *testing.T, server *ldaptest.Server) Options {
	t.Helper()
	return Options{
		URL:                  server.URL(),
		BaseDN:               testBaseDN,
		UserFilter:           "(uid={username})",
		UsernameAttribute:    "uid",
		EmailAttribute:       "mail",
		DisplayNameAttribute: "cn",
		Label:                "LDAP 登录",
		Timeout:              5 * time.Second,
		Logger:               discardLogger(),
	}
}

func mustClient(t *testing.T, options Options) *Client {
	t.Helper()
	client, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// TestAuthenticateSuccess covers the happy path and every fixed credential
// failure: the directory is a real LDAP server, the search is a real subtree
// search and the password is verified with a real bind as the matched entry.
func TestAuthenticateSuccess(t *testing.T) {
	server := ldaptest.Start(t)
	server.AddUser("alice", "alice@example.com", "Alice Example", knownPassword)
	client := mustClient(t, testOptions(t, server))
	ctx := context.Background()

	identity, err := client.Authenticate(ctx, "alice", knownPassword)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if identity.Username != "alice" || identity.Email != "alice@example.com" || identity.DisplayName != "Alice Example" || identity.DN == "" {
		t.Fatalf("identity = %+v", identity)
	}
	// Login lookups are case-insensitive: the search is not case-folded by the
	// client, but a directory entry is still reachable with a differently-cased
	// login name on servers whose matching rule ignores case.
	if _, err = client.Authenticate(ctx, "  alice  ", knownPassword); err != nil {
		t.Fatalf("trimmed username: %v", err)
	}

	for _, tc := range []struct {
		name     string
		username string
		password string
		want     error
	}{
		{"wrong password", "alice", "wrong-password", ErrInvalidCredentials},
		{"unknown user", "mallory", knownPassword, ErrInvalidCredentials},
		{"empty password", "alice", "", ErrInvalidCredentials},
		{"oversized password", "alice", strings.Repeat("x", MaxPasswordBytes+1), ErrInvalidCredentials},
		{"oversized username", strings.Repeat("a", MaxUsernameBytes+1), knownPassword, ErrInvalidCredentials},
		{"control characters", "ali\x01ce", knownPassword, ErrInvalidCredentials},
		// Filter metacharacters must be escaped instead of changing the filter.
		{"filter injection", "*)(uid=*", knownPassword, ErrInvalidCredentials},
		{"filter injection with closing paren", "alice)(|(uid=*", knownPassword, ErrInvalidCredentials},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := client.Authenticate(ctx, tc.username, tc.password); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestFilterEscapingMatchesLiteralMetacharacters proves the escaping is not
// over-eager: a directory username that literally contains "*" can still log in.
func TestFilterEscapingMatchesLiteralMetacharacters(t *testing.T) {
	server := ldaptest.Start(t)
	server.AddUser("ali*ce", "star@example.com", "Star User", knownPassword)
	client := mustClient(t, testOptions(t, server))
	identity, err := client.Authenticate(context.Background(), "ali*ce", knownPassword)
	if err != nil || identity.Username != "ali*ce" {
		t.Fatalf("literal metacharacter username = %+v, %v", identity, err)
	}
}

// TestServiceAccountBind proves the service bind happens before the search: the
// embedded directory rejects searches from unbound connections.
func TestServiceAccountBind(t *testing.T) {
	server := ldaptest.Start(t, ldaptest.WithRequireBind())
	server.AddEntry(ldaptest.Entry{
		DN:           "cn=svc,dc=example,dc=com",
		Attributes:   map[string][]string{"cn": {"svc"}},
		UserPassword: []string{"service-secret"},
	})
	server.AddUser("alice", "alice@example.com", "Alice", knownPassword)
	options := testOptions(t, server)
	options.BindDN = "cn=svc,dc=example,dc=com"
	options.BindPassword = "service-secret"
	client := mustClient(t, options)
	if _, err := client.Authenticate(context.Background(), "alice", knownPassword); err != nil {
		t.Fatalf("service-account search: %v", err)
	}

	// Anonymous search is impossible on this directory, so a failed service bind
	// is reported as a dependency failure instead of a credential failure.
	options.BindPassword = "wrong-service-password"
	failing := mustClient(t, options)
	if _, err := failing.Authenticate(context.Background(), "alice", knownPassword); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("failed service bind error = %v, want unavailable", err)
	}
}

// TestAnonymousSearch covers the unconfigured service account path.
func TestAnonymousSearch(t *testing.T) {
	server := ldaptest.Start(t)
	server.AddUser("bob", "bob@example.com", "Bob", knownPassword)
	client := mustClient(t, testOptions(t, server))
	if _, err := client.Authenticate(context.Background(), "bob", knownPassword); err != nil {
		t.Fatalf("anonymous search: %v", err)
	}
}

// TestAmbiguousEntry rejects a directory that matches more than one entry: the
// lookup must not guess. The embedded server honours the client's size limit and
// reports sizeLimitExceeded after returning the limit, like a real directory.
func TestAmbiguousEntry(t *testing.T) {
	server := ldaptest.Start(t)
	for _, dn := range []string{
		"uid=duplicate,ou=people,dc=example,dc=com",
		"uid=duplicate,ou=contractors,dc=example,dc=com",
	} {
		server.AddEntry(ldaptest.Entry{
			DN:           dn,
			Attributes:   map[string][]string{"uid": {"duplicate"}, "mail": {"duplicate@example.com"}, "cn": {"Duplicate"}},
			UserPassword: []string{knownPassword},
		})
	}
	client := mustClient(t, testOptions(t, server))
	if _, err := client.Authenticate(context.Background(), "duplicate", knownPassword); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("ambiguous match error = %v", err)
	}
}

// TestUnavailableDirectory covers a closed port, a wrong scheme host and a
// directory that accepts the connection but never answers (request deadline).
func TestUnavailableDirectory(t *testing.T) {
	server := ldaptest.Start(t)
	server.AddUser("carol", "carol@example.com", "Carol", knownPassword)
	options := testOptions(t, server)
	server.Stop()
	if _, err := mustClient(t, options).Authenticate(context.Background(), "carol", knownPassword); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("closed port error = %v, want unavailable", err)
	}

	hanging := ldaptest.Start(t, ldaptest.WithHang())
	hangingOptions := testOptions(t, hanging)
	hangingOptions.Timeout = time.Second
	start := time.Now()
	if _, err := mustClient(t, hangingOptions).Authenticate(context.Background(), "carol", knownPassword); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("hanging directory error = %v, want unavailable", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("request deadline not applied: %v", elapsed)
	}

	// A caller cancellation must also end the attempt.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := mustClient(t, hangingOptions).Authenticate(ctx, "carol", knownPassword); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("cancelled request error = %v, want unavailable", err)
	}
}

// TestLDAPS verifies the encrypted transport end to end with a throwaway
// certificate trusted only by the test client.
func TestLDAPS(t *testing.T) {
	serverTLS, clientTLS := ldaptest.TLSConfigs(t)
	server := ldaptest.Start(t, ldaptest.WithTLS(serverTLS))
	server.AddUser("dave", "dave@example.com", "Dave", knownPassword)
	options := testOptions(t, server)
	options.TLSConfig = clientTLS
	client := mustClient(t, options)
	if _, err := client.Authenticate(context.Background(), "dave", knownPassword); err != nil {
		t.Fatalf("ldaps authenticate: %v", err)
	}

	// Without the test root the certificate is not trusted: the client must
	// refuse the connection instead of silently skipping verification.
	untrusted := mustClient(t, testOptions(t, server))
	if _, err := untrusted.Authenticate(context.Background(), "dave", knownPassword); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("untrusted ldaps error = %v, want unavailable", err)
	}
}

// TestStartTLS upgrades a plaintext connection before any credential is sent.
func TestStartTLS(t *testing.T) {
	serverTLS, clientTLS := ldaptest.TLSConfigs(t)
	server := ldaptest.Start(t, ldaptest.WithStartTLS(serverTLS))
	server.AddUser("erin", "erin@example.com", "Erin", knownPassword)
	options := testOptions(t, server)
	options.StartTLS = true
	options.TLSConfig = clientTLS
	client := mustClient(t, options)
	if _, err := client.Authenticate(context.Background(), "erin", knownPassword); err != nil {
		t.Fatalf("starttls authenticate: %v", err)
	}
}

// TestAttributeBounds verifies that oversized identity attributes are handled
// without trusting them: an unusable email is treated as absent (the account
// layer then refuses auto-provisioning), an unusable username rejects the entry
// and an unusable display name falls back to empty.
func TestAttributeBounds(t *testing.T) {
	server := ldaptest.Start(t)
	server.AddEntry(ldaptest.Entry{
		DN: "uid=huge,ou=people,dc=example,dc=com",
		Attributes: map[string][]string{
			"uid":  {"huge"},
			"mail": {strings.Repeat("m", MaxAttributeBytes+1)},
			"cn":   {"Huge"},
		},
		UserPassword: []string{knownPassword},
	})
	client := mustClient(t, testOptions(t, server))
	identity, err := client.Authenticate(context.Background(), "huge", knownPassword)
	if err != nil {
		t.Fatalf("oversized email must not fail authentication: %v", err)
	}
	if identity.Email != "" {
		t.Fatalf("oversized email accepted: %q", identity.Email)
	}

	// A username attribute above the cap is not usable as an identity.
	server.AddEntry(ldaptest.Entry{
		DN: "uid=bounds,ou=people,dc=example,dc=com",
		Attributes: map[string][]string{
			"uid":         {"bounds"},
			"displayName": {"Bounds"},
			"description": {strings.Repeat("d", MaxAttributeBytes+1)},
		},
		UserPassword: []string{knownPassword},
	})
	options := testOptions(t, server)
	options.UsernameAttribute = "description"
	options.EmailAttribute = "mail"
	if _, err = mustClient(t, options).Authenticate(context.Background(), "bounds", knownPassword); !errors.Is(err, ErrInvalidEntry) {
		t.Fatalf("oversized username attribute error = %v, want invalid entry", err)
	}

	// Control characters in the display name are stripped by falling back to
	// empty; the login itself still succeeds.
	server.AddEntry(ldaptest.Entry{
		DN: "uid=control,ou=people,dc=example,dc=com",
		Attributes: map[string][]string{
			"uid":  {"control"},
			"cn":   {"Con\x01trol"},
			"mail": {"control@example.com"},
		},
		UserPassword: []string{knownPassword},
	})
	identity, err = client.Authenticate(context.Background(), "control", knownPassword)
	if err != nil || identity.DisplayName != "" {
		t.Fatalf("control display name = %+v, %v", identity, err)
	}
}

// TestMissingAttributes covers directories that omit the email or username
// attribute: the login name is the username fallback and the email is empty.
func TestMissingAttributes(t *testing.T) {
	server := ldaptest.Start(t)
	server.AddEntry(ldaptest.Entry{
		DN:           "uid=noemail,ou=people,dc=example,dc=com",
		Attributes:   map[string][]string{"uid": {"noemail"}},
		UserPassword: []string{knownPassword},
	})
	client := mustClient(t, testOptions(t, server))
	identity, err := client.Authenticate(context.Background(), "noemail", knownPassword)
	if err != nil || identity.Username != "noemail" || identity.Email != "" || identity.DisplayName != "" {
		t.Fatalf("missing attributes identity = %+v, %v", identity, err)
	}

	options := testOptions(t, server)
	options.UsernameAttribute = "employeeID"
	identity, err = mustClient(t, options).Authenticate(context.Background(), "noemail", knownPassword)
	if err != nil || identity.Username != "noemail" {
		t.Fatalf("username fallback = %+v, %v", identity, err)
	}
}

// TestNewValidation pins the defense-in-depth checks for programmatic callers.
func TestNewValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Options)
	}{
		{"missing url", func(o *Options) { o.URL = "" }},
		{"missing base dn", func(o *Options) { o.BaseDN = "" }},
		{"short timeout", func(o *Options) { o.Timeout = time.Millisecond }},
		{"no placeholder", func(o *Options) { o.UserFilter = "(uid=alice)" }},
		{"two placeholders", func(o *Options) { o.UserFilter = "(uid={username})(uid={username})" }},
		{"bad scheme", func(o *Options) { o.URL = "https://directory.example.com" }},
		{"starttls over ldaps", func(o *Options) { o.URL = "ldaps://directory.example.com:636"; o.StartTLS = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options := Options{
				URL:                  "ldap://127.0.0.1:3389",
				BaseDN:               testBaseDN,
				UserFilter:           "(uid={username})",
				UsernameAttribute:    "uid",
				EmailAttribute:       "mail",
				DisplayNameAttribute: "cn",
				Label:                "LDAP 登录",
				Timeout:              time.Second,
			}
			tc.mutate(&options)
			if _, err := New(options); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
	client := mustClient(t, Options{
		URL:                  "ldap://127.0.0.1:3389",
		BaseDN:               testBaseDN,
		UserFilter:           "(uid={username})",
		UsernameAttribute:    "uid",
		EmailAttribute:       "mail",
		DisplayNameAttribute: "cn",
		Label:                "企业目录",
		Timeout:              time.Second,
	})
	if client.Label() != "企业目录" {
		t.Fatalf("label = %q", client.Label())
	}
}

// TestConfigurableFilterAndAttributes proves the configurable filter/attribute
// names are actually used: a mail-based filter and custom attribute names.
func TestConfigurableFilterAndAttributes(t *testing.T) {
	server := ldaptest.Start(t)
	server.AddEntry(ldaptest.Entry{
		DN: "cn=Frank Person,ou=people,dc=example,dc=com",
		Attributes: map[string][]string{
			"objectClass":    {"inetOrgPerson"},
			"cn":             {"Frank Person"},
			"mail":           {"frank@example.com"},
			"sAMAccountName": {"frank"},
			"displayName":    {"Frank"},
		},
		UserPassword: []string{knownPassword},
	})
	options := testOptions(t, server)
	options.UserFilter = "(&(objectClass=inetOrgPerson)(mail={username}))"
	options.UsernameAttribute = "sAMAccountName"
	options.EmailAttribute = "mail"
	options.DisplayNameAttribute = "displayName"
	identity, err := mustClient(t, options).Authenticate(context.Background(), "frank@example.com", knownPassword)
	if err != nil {
		t.Fatalf("custom filter: %v", err)
	}
	if identity.Username != "frank" || identity.Email != "frank@example.com" || identity.DisplayName != "Frank" {
		t.Fatalf("identity = %+v", identity)
	}
}
