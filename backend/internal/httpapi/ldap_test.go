package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
	"github.com/yichenfchai/AegisFlow/backend/internal/ldap"
	"github.com/yichenfchai/AegisFlow/backend/internal/ldaptest"
	"github.com/yichenfchai/AegisFlow/backend/internal/tenants"
)

const ldapTestPassword = "directory-password-2026"

// ldapOptionsFor builds the production LDAP client configuration against one
// embedded directory. Tests override individual fields as needed.
func ldapOptionsFor(server *ldaptest.Server) ldap.Options {
	return ldap.Options{
		URL:                  server.URL(),
		BaseDN:               "dc=example,dc=com",
		UserFilter:           "(uid={username})",
		UsernameAttribute:    "uid",
		EmailAttribute:       "mail",
		DisplayNameAttribute: "cn",
		Label:                "LDAP 登录",
		Timeout:              5 * time.Second,
		Logger:               slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func mustLDAPClient(t *testing.T, options ldap.Options) *ldap.Client {
	t.Helper()
	client, err := ldap.New(options)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// ldapTestServer starts a normal HTTP test server backed by real PostgreSQL and
// registers the embedded directory with the auth service.
func ldapTestServer(t *testing.T, autoProvision bool, options ldap.Options) (http.Handler, *auth.Service, *sql.DB) {
	t.Helper()
	handler, service, db := testServerWithDB(t, Options{})
	service.WithLDAPDirectory(mustLDAPClient(t, options), autoProvision)
	return handler, service, db
}

func loginLDAP(t *testing.T, h http.Handler, username, password, tenantSlug string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"username":` + quoteJSON(username) + `,"password":` + quoteJSON(password)
	if tenantSlug != "" {
		body += `,"tenantSlug":` + quoteJSON(tenantSlug)
	}
	body += `}`
	return request(h, "POST", "/api/v1/auth/ldap/login", body, "")
}

func quoteJSON(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func provisionerActor() auth.Actor {
	return auth.Actor{TenantID: tenants.DefaultID, Username: "provisioner", RequestID: "req-setup"}
}

// TestLDAPConfigProbeAndLoginClosed pins the unconfigured behavior: the probe
// answers enabled=false without leaking configuration and the login route stays
// registered but closed with 503 (documented choice, same as OIDC routes).
func TestLDAPConfigProbeAndLoginClosed(t *testing.T) {
	h, _ := testServer(t)
	w := request(h, "GET", "/api/v1/auth/ldap/config", "", "")
	if w.Code != 200 {
		t.Fatalf("config status = %d", w.Code)
	}
	var config struct {
		Data struct {
			Enabled bool   `json:"enabled"`
			Label   string `json:"label"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &config); err != nil {
		t.Fatal(err)
	}
	if config.Data.Enabled || config.Data.Label != "" {
		t.Fatalf("disabled config = %+v", config.Data)
	}
	if w = loginLDAP(t, h, "alice", ldapTestPassword, ""); w.Code != 503 || !strings.Contains(w.Body.String(), "dependencies_unavailable") || !strings.Contains(w.Body.String(), "ldap is not configured") {
		t.Fatalf("disabled login = %d %s", w.Code, w.Body.String())
	}

	// Enabled deployment: the probe reports the label but still no endpoint,
	// base DN or filter.
	server := ldaptest.Start(t)
	server.AddUser("alice", "alice@example.com", "Alice", ldapTestPassword)
	enabled, _, _ := ldapTestServer(t, false, ldapOptionsFor(server))
	w = request(enabled, "GET", "/api/v1/auth/ldap/config", "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"enabled":true`) || !strings.Contains(w.Body.String(), "LDAP 登录") {
		t.Fatalf("enabled config = %d %s", w.Code, w.Body.String())
	}
	for _, leak := range []string{server.URL(), "dc=example", "uid="} {
		if strings.Contains(w.Body.String(), leak) {
			t.Fatalf("enabled config probe leaks %q: %s", leak, w.Body.String())
		}
	}
}

// TestLDAPLoginSuccess covers the full success path: directory bind, account
// linkage, session cookie attributes, refresh, tenant metadata and audit detail.
func TestLDAPLoginSuccess(t *testing.T) {
	server := ldaptest.Start(t)
	server.AddUser("alice", "alice@example.com", "Alice Example", ldapTestPassword)
	h, service, db := ldapTestServer(t, false, ldapOptionsFor(server))
	ctx := context.Background()
	member, err := service.CreateUser(ctx, provisionerActor(), "alice-local", "local-password-2026", "Alice", "member")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE auth_users SET email='alice@example.com' WHERE id=$1`, member.ID); err != nil {
		t.Fatal(err)
	}
	w := loginLDAP(t, h, "alice", ldapTestPassword, "")
	access, cookie := responseTokens(t, w)
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != refreshCookiePath {
		t.Fatalf("refresh cookie = %+v", cookie)
	}
	// LDAP login has no remember choice: the session cookie is non-persistent.
	if cookie.MaxAge != 0 || !cookie.Expires.IsZero() {
		t.Fatalf("LDAP cookie must be a session cookie: %+v", cookie)
	}
	var body struct {
		Data auth.Tokens `json:"data"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Data.User.ID != member.ID || body.Data.User.Tenant == nil || body.Data.User.Tenant.Slug != tenants.DefaultSlug {
		t.Fatalf("LDAP user payload = %+v", body.Data.User)
	}
	if w := request(h, "GET", "/api/v1/auth/me", "", access); w.Code != 200 || !strings.Contains(w.Body.String(), `"slug":"default"`) {
		t.Fatalf("/auth/me after LDAP login = %d %s", w.Code, w.Body.String())
	}
	if w := cookieRequest(h, "POST", "/api/v1/auth/refresh", `{}`, cookie); w.Code != 200 {
		t.Fatalf("LDAP refresh = %d %s", w.Code, w.Body.String())
	}
	if w := cookieRequest(h, "POST", "/api/v1/auth/logout", `{}`, cookie); w.Code != 200 {
		t.Fatalf("LDAP logout = %d", w.Code)
	}
	if w := request(h, "GET", "/api/v1/auth/me", "", access); w.Code != 401 {
		t.Fatalf("session survived logout: %d", w.Code)
	}
}

// TestLDAPLoginFailures covers the fixed failure contract: credential and
// directory-data problems are one generic 401, directory dependency problems a
// 503 with a generic message, and local account policy decides the rest.
func TestLDAPLoginFailures(t *testing.T) {
	server := ldaptest.Start(t)
	server.AddUser("alice", "alice@example.com", "Alice", ldapTestPassword)
	server.AddUser("unlinked-user", "unlinked@example.com", "Unlinked", ldapTestPassword)
	// An ambiguous directory entry (same uid under two OUs) must not be guessed.
	for _, dn := range []string{"uid=dup,ou=people,dc=example,dc=com", "uid=dup,ou=contractors,dc=example,dc=com"} {
		server.AddEntry(ldaptest.Entry{
			DN:           dn,
			Attributes:   map[string][]string{"uid": {"dup"}, "mail": {"dup@example.com"}, "cn": {"Dup"}},
			UserPassword: []string{ldapTestPassword},
		})
	}
	h, service, db := ldapTestServer(t, false, ldapOptionsFor(server))
	ctx := context.Background()
	for _, tc := range []struct{ name, username, password string }{
		{"wrong password", "alice", "wrong-directory-password"},
		{"unknown directory user", "nobody", ldapTestPassword},
		{"filter injection", `*)(uid=*`, ldapTestPassword},
		{"ambiguous directory match", "dup", ldapTestPassword},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := loginLDAP(t, h, tc.username, tc.password, "")
			if w.Code != 401 {
				t.Fatalf("status = %d %s", w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "uid=") || strings.Contains(w.Body.String(), "dc=example") {
				t.Fatalf("directory detail leaked: %s", w.Body.String())
			}
		})
	}
	// Unknown local account with auto-provisioning disabled: 403 account_not_found.
	if w := loginLDAP(t, h, "unlinked-user", ldapTestPassword, ""); w.Code != 403 || !strings.Contains(w.Body.String(), "account_not_found") {
		t.Fatalf("unlinked account = %d %s", w.Code, w.Body.String())
	}
	// Disabled linked account: the directory password is proven first, then the
	// local disabled state answers 403 account_disabled.
	member, err := service.CreateUser(ctx, provisionerActor(), "disabled-local", "local-password-2026", "Disabled", "member")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE auth_users SET email='alice@example.com' WHERE id=$1`, member.ID); err != nil {
		t.Fatal(err)
	}
	if err = service.SetUserDisabled(ctx, provisionerActor(), member.ID, true); err != nil {
		t.Fatal(err)
	}
	if w := loginLDAP(t, h, "alice", ldapTestPassword, ""); w.Code != 403 || !strings.Contains(w.Body.String(), "account_disabled") {
		t.Fatalf("disabled account = %d %s", w.Code, w.Body.String())
	}

	// Directory unreachable: 503 with a generic message; the endpoint is not
	// exposed to the client.
	stopped := ldaptest.Start(t)
	closedOptions := ldapOptionsFor(stopped)
	stopped.Stop()
	unreachable, _, _ := ldapTestServer(t, false, closedOptions)
	if w := loginLDAP(t, unreachable, "alice", ldapTestPassword, ""); w.Code != 503 {
		t.Fatalf("unreachable directory = %d %s", w.Code, w.Body.String())
	} else if strings.Contains(w.Body.String(), "127.0.0.1") || strings.Contains(w.Body.String(), "ldap") {
		t.Fatalf("unreachable directory body leaks internals: %s", w.Body.String())
	}

	// Failed service-account bind: an operator problem, answered 503 instead of
	// turning every login into a normal wrong-password 401.
	serviceBound := ldaptest.Start(t, ldaptest.WithRequireBind())
	serviceBound.AddEntry(ldaptest.Entry{DN: "cn=svc,dc=example,dc=com", Attributes: map[string][]string{"cn": {"svc"}}, UserPassword: []string{"service-secret"}})
	serviceBound.AddUser("svc-user", "svc-user@example.com", "Svc", ldapTestPassword)
	badBind := ldapOptionsFor(serviceBound)
	badBind.BindDN = "cn=svc,dc=example,dc=com"
	badBind.BindPassword = "wrong-service-password"
	badBindServer, _, _ := ldapTestServer(t, false, badBind)
	if w := loginLDAP(t, badBindServer, "svc-user", ldapTestPassword, ""); w.Code != 503 {
		t.Fatalf("failed service bind = %d %s", w.Code, w.Body.String())
	}
}

// TestLDAPServiceAccountBindAndTransport covers a successful service bind plus
// LDAPS and StartTLS end to end through the HTTP endpoint.
func TestLDAPServiceAccountBindAndTransport(t *testing.T) {
	serverTLS, clientTLS := ldaptest.TLSConfigs(t)
	server := ldaptest.Start(t, ldaptest.WithTLS(serverTLS), ldaptest.WithRequireBind())
	server.AddEntry(ldaptest.Entry{DN: "cn=svc,dc=example,dc=com", Attributes: map[string][]string{"cn": {"svc"}}, UserPassword: []string{"service-secret"}})
	server.AddUser("alice", "alice@example.com", "Alice", ldapTestPassword)
	options := ldapOptionsFor(server)
	options.BindDN = "cn=svc,dc=example,dc=com"
	options.BindPassword = "service-secret"
	options.TLSConfig = clientTLS
	h, service, db := ldapTestServer(t, false, options)
	if _, err := service.CreateUser(context.Background(), provisionerActor(), "alice-local", "local-password-2026", "Alice", "member"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE auth_users SET email='alice@example.com' WHERE username='alice-local'`); err != nil {
		t.Fatal(err)
	}
	w := loginLDAP(t, h, "alice", ldapTestPassword, "")
	_, _ = responseTokens(t, w)

	// StartTLS: plain URL upgraded before the service bind.
	startTLSServer := ldaptest.Start(t, ldaptest.WithStartTLS(serverTLS), ldaptest.WithRequireBind())
	startTLSServer.AddEntry(ldaptest.Entry{DN: "cn=svc,dc=example,dc=com", Attributes: map[string][]string{"cn": {"svc"}}, UserPassword: []string{"service-secret"}})
	startTLSServer.AddUser("bob", "bob@example.com", "Bob", ldapTestPassword)
	startTLSOptions := ldapOptionsFor(startTLSServer)
	startTLSOptions.StartTLS = true
	startTLSOptions.BindDN = "cn=svc,dc=example,dc=com"
	startTLSOptions.BindPassword = "service-secret"
	startTLSOptions.TLSConfig = clientTLS
	startTLSHandler, startTLSService, startTLSDB := ldapTestServer(t, false, startTLSOptions)
	if _, err := startTLSService.CreateUser(context.Background(), provisionerActor(), "bob-local", "local-password-2026", "Bob", "member"); err != nil {
		t.Fatal(err)
	}
	if _, err := startTLSDB.Exec(`UPDATE auth_users SET email='bob@example.com' WHERE username='bob-local'`); err != nil {
		t.Fatal(err)
	}
	if w := loginLDAP(t, startTLSHandler, "bob", ldapTestPassword, ""); w.Code != 200 {
		t.Fatalf("starttls login = %d %s", w.Code, w.Body.String())
	}
}

// TestLDAPLoginTenantResolution covers the reused F1 tenant semantics: ambiguous
// identifiers require tenantSlug, an explicit slug scopes the lookup and an
// unknown tenant does not reveal whether it exists.
func TestLDAPLoginTenantResolution(t *testing.T) {
	server := ldaptest.Start(t)
	server.AddUser("shared", "shared@example.com", "Shared", ldapTestPassword)
	h, service, db := ldapTestServer(t, false, ldapOptionsFor(server))
	ctx := context.Background()
	otherTenant, err := tenants.Create(ctx, db, "tenant-b", "Tenant B")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.CreateUser(ctx, provisionerActor(), "shared", "local-password-2026", "Shared A", "member"); err != nil {
		t.Fatal(err)
	}
	if _, err = service.CreateUser(ctx, auth.Actor{TenantID: otherTenant.ID, Username: "provisioner", RequestID: "req-setup"}, "shared", "local-password-2026", "Shared B", "admin"); err != nil {
		t.Fatal(err)
	}
	if w := loginLDAP(t, h, "shared", ldapTestPassword, ""); w.Code != 400 || !strings.Contains(w.Body.String(), "tenant_required") {
		t.Fatalf("ambiguous login = %d %s", w.Code, w.Body.String())
	}
	w := loginLDAP(t, h, "shared", ldapTestPassword, "tenant-b")
	var body struct {
		Data auth.Tokens `json:"data"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Data.User.Tenant == nil || body.Data.User.Tenant.Slug != "tenant-b" || body.Data.User.DisplayName != "Shared B" {
		t.Fatalf("tenant-scoped login = %+v", body.Data.User)
	}
	if w = loginLDAP(t, h, "shared", ldapTestPassword, "unknown-tenant"); w.Code != 403 || !strings.Contains(w.Body.String(), "account_not_found") {
		t.Fatalf("unknown tenant = %d %s", w.Code, w.Body.String())
	}
}

// TestLDAPAutoProvision covers the member-only auto-provision policy end to end,
// including the trusted-email requirement and the default-tenant placement.
func TestLDAPAutoProvision(t *testing.T) {
	server := ldaptest.Start(t)
	server.AddUser("new-person", "new-person@example.com", "New Person", ldapTestPassword)
	server.AddEntry(ldaptest.Entry{
		DN:           "uid=no-email,ou=people,dc=example,dc=com",
		Attributes:   map[string][]string{"uid": {"no-email"}, "cn": {"No Email"}},
		UserPassword: []string{ldapTestPassword},
	})
	h, _, db := ldapTestServer(t, true, ldapOptionsFor(server))
	w := loginLDAP(t, h, "new-person", ldapTestPassword, "")
	_, _ = responseTokens(t, w)
	var body struct {
		Data auth.Tokens `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Data.User.Role != "member" || body.Data.User.Username != "new-person" || body.Data.User.Tenant == nil || body.Data.User.Tenant.Slug != tenants.DefaultSlug {
		t.Fatalf("auto-provisioned user = %+v", body.Data.User)
	}
	var email, role string
	if err := db.QueryRow(`SELECT email,role FROM auth_users WHERE id=$1`, body.Data.User.ID).Scan(&email, &role); err != nil {
		t.Fatal(err)
	}
	if email != "new-person@example.com" || role != "member" {
		t.Fatalf("provisioned row = %q %q", email, role)
	}
	var created, loginDetail string
	if err := db.QueryRow(`SELECT detail FROM auth_audit WHERE action=$1 AND object_id=$2`, auth.ActionUserCreate, body.Data.User.ID).Scan(&created); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT detail FROM auth_audit WHERE action=$1 AND actor_id=$2`, auth.ActionLogin, body.Data.User.ID).Scan(&loginDetail); err != nil {
		t.Fatal(err)
	}
	if created != "method=ldap" || loginDetail != "method=ldap tenant=default" {
		t.Fatalf("audit details = %q / %q", created, loginDetail)
	}
	// Retried first login reuses the same account instead of creating another.
	w = loginLDAP(t, h, "new-person", ldapTestPassword, "")
	var again struct {
		Data auth.Tokens `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &again); err != nil {
		t.Fatal(err)
	}
	if again.Data.User.ID != body.Data.User.ID {
		t.Fatalf("retried provisioning created a new account: %s vs %s", again.Data.User.ID, body.Data.User.ID)
	}
	// A directory identity without an email cannot be provisioned.
	if w = loginLDAP(t, h, "no-email", ldapTestPassword, ""); w.Code != 403 || !strings.Contains(w.Body.String(), "account_not_found") || !strings.Contains(w.Body.String(), "email") {
		t.Fatalf("missing-email provisioning = %d %s", w.Code, w.Body.String())
	}
}

// TestLDAPAutoProvisionIntoExplicitTenant verifies the requested workspace wins
// when auto-provisioning, and that the created account can never be an admin.
func TestLDAPAutoProvisionIntoExplicitTenant(t *testing.T) {
	server := ldaptest.Start(t)
	server.AddUser("tenant-b-person", "tenant-b@example.com", "Tenant B", ldapTestPassword)
	h, _, db := ldapTestServer(t, true, ldapOptionsFor(server))
	ctx := context.Background()
	otherTenant, err := tenants.Create(ctx, db, "tenant-b", "Tenant B")
	if err != nil {
		t.Fatal(err)
	}
	w := loginLDAP(t, h, "tenant-b-person", ldapTestPassword, "tenant-b")
	var body struct {
		Data auth.Tokens `json:"data"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Data.User.Role != "member" || body.Data.User.Tenant == nil || body.Data.User.Tenant.Slug != "tenant-b" {
		t.Fatalf("explicit-tenant provisioning = %+v", body.Data.User)
	}
	var tenantID, role string
	if err = db.QueryRow(`SELECT tenant_id,role FROM auth_users WHERE id=$1`, body.Data.User.ID).Scan(&tenantID, &role); err != nil {
		t.Fatal(err)
	}
	if tenantID != otherTenant.ID || role != "member" {
		t.Fatalf("provisioned row tenant=%q role=%q", tenantID, role)
	}
}

// TestLDAPStrictJSONAndValidation covers the shared strict-JSON and field
// validation rules, including the Origin requirement and the shared login quota.
func TestLDAPStrictJSONAndValidation(t *testing.T) {
	server := ldaptest.Start(t)
	server.AddUser("alice", "alice@example.com", "Alice", ldapTestPassword)
	h, _, _ := ldapTestServer(t, false, ldapOptionsFor(server))
	for _, tc := range []struct{ name, body string }{
		{"unknown field", `{"username":"alice","password":"x","remember":true}`},
		{"null tenantSlug", `{"username":"alice","password":"x","tenantSlug":null}`},
		{"empty username", `{"username":"   ","password":"x"}`},
		{"oversized username", `{"username":"` + strings.Repeat("a", 257) + `","password":"x"}`},
		{"empty password", `{"username":"alice","password":""}`},
		{"oversized password", `{"username":"alice","password":"` + strings.Repeat("p", 1025) + `"}`},
		{"invalid tenantSlug", `{"username":"alice","password":"x","tenantSlug":"Not A Slug"}`},
		{"empty tenantSlug", `{"username":"alice","password":"x","tenantSlug":""}`},
		{"array body", `[]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := request(h, "POST", "/api/v1/auth/ldap/login", tc.body, "")
			if w.Code != 400 {
				t.Fatalf("status = %d %s", w.Code, w.Body.String())
			}
		})
	}
	// Auth POSTs keep the identical Origin/CSRF rule as password login.
	r := httptest.NewRequest("POST", "/api/v1/auth/ldap/login", strings.NewReader(`{"username":"alice","password":"x"}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 || !strings.Contains(w.Body.String(), "origin_denied") {
		t.Fatalf("missing origin = %d %s", w.Code, w.Body.String())
	}
}

// TestLDAPRateLimit proves the LDAP login route consumes the shared login quota
// and answers 429 with Retry-After after the limit is reached.
func TestLDAPRateLimit(t *testing.T) {
	server := ldaptest.Start(t)
	server.AddUser("alice", "alice@example.com", "Alice", ldapTestPassword)
	h, _, _ := ldapTestServer(t, false, ldapOptionsFor(server))
	for i := 0; i < 10; i++ {
		if w := request(h, "POST", "/api/v1/auth/ldap/login", `{}`, ""); w.Code != 400 {
			t.Fatalf("attempt %d = %d %s", i, w.Code, w.Body.String())
		}
	}
	w := request(h, "POST", "/api/v1/auth/ldap/login", `{}`, "")
	if w.Code != 429 || w.Header().Get("Retry-After") == "" || !strings.Contains(w.Body.String(), "rate_limited") {
		t.Fatalf("rate limit = %d %s", w.Code, w.Body.String())
	}
}

// TestLDAPAuditNeverContainsDirectorySecrets asserts the audit trail records
// only the fixed method/tenant detail, never the directory password, DN, email
// or filter.
func TestLDAPAuditNeverContainsDirectorySecrets(t *testing.T) {
	server := ldaptest.Start(t)
	server.AddUser("audit-user", "audit-user@example.com", "Audit User", ldapTestPassword)
	h, _, db := ldapTestServer(t, true, ldapOptionsFor(server))
	if w := loginLDAP(t, h, "audit-user", ldapTestPassword, ""); w.Code != 200 {
		t.Fatalf("login = %d %s", w.Code, w.Body.String())
	}
	rows, err := db.Query(`SELECT detail FROM auth_audit WHERE action IN ($1,$2) ORDER BY id`, auth.ActionLogin, auth.ActionUserCreate)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var detail string
		if err = rows.Scan(&detail); err != nil {
			t.Fatal(err)
		}
		seen++
		for _, forbidden := range []string{ldapTestPassword, "uid=audit-user", "dc=example", "audit-user@example.com", "cn=Audit"} {
			if strings.Contains(detail, forbidden) {
				t.Fatalf("audit detail %q leaks %q", detail, forbidden)
			}
		}
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if seen != 2 {
		t.Fatalf("audit rows = %d, want login + user.create", seen)
	}
}
