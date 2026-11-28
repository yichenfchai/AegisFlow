package main

import (
	"bytes"
	"context"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
	"github.com/yichenfchai/AegisFlow/backend/internal/dbmigrate"
	"github.com/yichenfchai/AegisFlow/backend/internal/tenants"
	"github.com/yichenfchai/AegisFlow/backend/internal/testutil"
)

// cliEnvironment opens one migrated test schema and points AEGIS_DATABASE_URL at
// it, so the real CLI connection path (config.DatabaseURL + pgx) is exercised.
// The returned handle reads the same schema for assertions.
func cliEnvironment(t *testing.T) *sql.DB {
	t.Helper()
	db := testutil.OpenPostgres(t)
	if err := dbmigrate.Apply(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	var schema string
	if err := db.QueryRow(`SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	raw := os.Getenv("AEGIS_TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("real PostgreSQL integration test skipped: set AEGIS_TEST_DATABASE_URL")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	t.Setenv("AEGIS_DATABASE_URL", parsed.String())
	// A stale path variable must not silently change the shared validation.
	t.Setenv("AEGIS_DATABASE_PATH", "")
	return db
}

// runCLI invokes the real command and returns its exit code and output.
func runCLI(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// createdPassword extracts the one-time password line printed by a successful
// create/create-admin invocation.
func createdPassword(t *testing.T, stdout string) string {
	t.Helper()
	const marker = "initial password (shown only once; store it securely): "
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, marker) {
			return strings.TrimSpace(strings.TrimPrefix(line, marker))
		}
	}
	t.Fatalf("no one-time password line in output:\n%s", stdout)
	return ""
}

// loginAs authenticates through the real auth service against the test schema.
func loginAs(t *testing.T, db *sql.DB, slug, username, password string) (*auth.Tokens, error) {
	t.Helper()
	service := auth.New(db, strings.Repeat("k", 32), time.Minute, time.Hour)
	return service.LoginWithTenant(context.Background(), slug, username, password, false, "req-cli-test")
}

func tenantCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM tenants`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestCreateWithGeneratedAdminPasswordAndLogin(t *testing.T) {
	db := cliEnvironment(t)
	code, stdout, stderr := runCLI("create", "--slug", "acme", "--name", "Acme Corp", "--admin-email", "ops@acme.test")
	if code != 0 {
		t.Fatalf("create exit = %d, stderr=%s", code, stderr)
	}
	password := createdPassword(t, stdout)
	if len(password) < 12 || len(password) > 72 {
		t.Fatalf("generated password length = %d", len(password))
	}
	if !strings.Contains(stdout, "created tenant: id=") || !strings.Contains(stdout, "slug=acme") {
		t.Fatalf("create output = %s", stdout)
	}
	if !strings.Contains(stdout, "created administrator: tenant=acme username=ops") {
		t.Fatalf("admin output = %s", stdout)
	}
	// The password is printed exactly once: the same value must not appear twice.
	if strings.Count(stdout, password) != 1 {
		t.Fatalf("password printed %d times", strings.Count(stdout, password))
	}
	if strings.Contains(stderr, password) {
		t.Fatal("password leaked to stderr")
	}

	tenant, err := tenants.BySlug(context.Background(), db, "acme")
	if err != nil || tenant.Name != "Acme Corp" || tenant.Status != tenants.StatusActive {
		t.Fatalf("stored tenant = %+v, %v", tenant, err)
	}
	tokens, err := loginAs(t, db, "acme", "ops", password)
	if err != nil {
		t.Fatalf("login with generated password: %v", err)
	}
	if tokens.User.TenantID != tenant.ID || tokens.User.Role != "admin" {
		t.Fatalf("logged-in user = %+v", tokens.User)
	}
	// The audit trail proves the account creation happened inside the new tenant,
	// while never containing the password.
	rows, err := db.Query(`SELECT tenant_id,action,detail FROM auth_audit WHERE object_type='user' AND action='user.create'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var audited int
	for rows.Next() {
		var tenantID, action, detail string
		if err := rows.Scan(&tenantID, &action, &detail); err != nil {
			t.Fatal(err)
		}
		audited++
		if tenantID != tenant.ID {
			t.Fatalf("user.create audit tenant = %q, want %q", tenantID, tenant.ID)
		}
		if strings.Contains(detail, password) || strings.Contains(detail, "ops@acme.test") {
			t.Fatalf("audit detail contains credential/email data: %q", detail)
		}
	}
	if audited != 1 {
		t.Fatalf("user.create audit rows = %d", audited)
	}
}

func TestCreateRejectsDuplicateAndInvalidSlug(t *testing.T) {
	db := cliEnvironment(t)
	if code, _, stderr := runCLI("create", "--slug", "acme", "--name", "Acme"); code != 0 {
		t.Fatalf("first create exit = %d stderr=%s", code, stderr)
	}
	code, stdout, stderr := runCLI("create", "--slug", "acme", "--name", "Other")
	if code == 0 {
		t.Fatalf("duplicate slug accepted: %s", stdout)
	}
	if !strings.Contains(stderr, "already exists") {
		t.Fatalf("duplicate slug stderr = %q", stderr)
	}
	if got := tenantCount(t, db); got != 2 {
		t.Fatalf("tenant count after duplicate = %d, want the seed plus acme", got)
	}
	code, _, stderr = runCLI("create", "--slug", "Bad Slug", "--name", "Bad")
	if code == 0 || !strings.Contains(stderr, "invalid tenant slug") {
		t.Fatalf("invalid slug = exit %d stderr %q", code, stderr)
	}
	code, _, stderr = runCLI("create", "--slug", "short-name", "--name", "x")
	if code != 0 {
		t.Fatalf("short display name rejected: exit %d stderr %s", code, stderr)
	}
	code, _, stderr = runCLI("create", "--slug", "bad-password", "--name", "Bad Password", "--admin-email", "a@b.test", "--admin-password", "short")
	if code == 0 || !strings.Contains(stderr, "12-72") {
		t.Fatalf("short password = exit %d stderr %q", code, stderr)
	}
	if got := tenantCount(t, db); got != 3 {
		t.Fatalf("tenant count = %d, short-name tenant missing or invalid create leaked", got)
	}
}

func TestCreateAdminForExistingAndMissingTenant(t *testing.T) {
	db := cliEnvironment(t)
	if code, _, stderr := runCLI("create", "--slug", "acme", "--name", "Acme"); code != 0 {
		t.Fatalf("create exit = %d stderr=%s", code, stderr)
	}
	code, stdout, stderr := runCLI("create-admin", "--tenant", "acme", "--email", "dev@acme.test", "--password", "explicit-password-01")
	if code != 0 {
		t.Fatalf("create-admin exit = %d stderr=%s", code, stderr)
	}
	if strings.Contains(stdout, "initial password") || strings.Contains(stdout, "explicit-password-01") || strings.Contains(stderr, "explicit-password-01") {
		t.Fatalf("explicit password echoed: stdout=%s stderr=%s", stdout, stderr)
	}
	if !strings.Contains(stdout, "username=dev") {
		t.Fatalf("derived username output = %s", stdout)
	}
	if _, err := loginAs(t, db, "acme", "dev", "explicit-password-01"); err != nil {
		t.Fatalf("login as explicit-password admin: %v", err)
	}
	// A second administrator with the same email gets a suffix, not a conflict.
	code, stdout, stderr = runCLI("create-admin", "--tenant", "acme", "--email", "dev@acme.test")
	if code != 0 {
		t.Fatalf("second create-admin exit = %d stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "username=dev-2") {
		t.Fatalf("suffixed username output = %s", stdout)
	}
	// A tenant that does not exist is a clean non-zero failure.
	code, _, stderr = runCLI("create-admin", "--tenant", "missing", "--email", "x@y.test")
	if code == 0 || !strings.Contains(stderr, `tenant "missing" does not exist`) {
		t.Fatalf("missing tenant = exit %d stderr %q", code, stderr)
	}
}

func TestListShowsIdentityStatusAndUserCountWithoutSecrets(t *testing.T) {
	db := cliEnvironment(t)
	if code, _, stderr := runCLI("create", "--slug", "acme", "--name", "Acme", "--admin-email", "ops@acme.test"); code != 0 {
		t.Fatalf("create exit = %d stderr=%s", code, stderr)
	}
	code, stdout, stderr := runCLI("list")
	if code != 0 {
		t.Fatalf("list exit = %d stderr=%s", code, stderr)
	}
	for _, want := range []string{"ID", "SLUG", "NAME", "STATUS", "USERS", "CREATED_AT", "default", "acme", "active"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("list output missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "password") || strings.Contains(stdout, "hash") || strings.Contains(stdout, "cipher") {
		t.Fatalf("list leaked credential material:\n%s", stdout)
	}
	// Identity columns come from the database, not from request parameters.
	var id string
	if err := db.QueryRow(`SELECT id FROM tenants WHERE slug='acme'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, id) {
		t.Fatalf("list output missing tenant id %s:\n%s", id, stdout)
	}
}

func TestDisableAndEnableGateLogin(t *testing.T) {
	db := cliEnvironment(t)
	if code, _, stderr := runCLI("create", "--slug", "acme", "--name", "Acme", "--admin-email", "ops@acme.test"); code != 0 {
		t.Fatalf("create exit = %d stderr=%s", code, stderr)
	}
	// Recover the generated password so the login checks use a real credential.
	code, stdout, stderr := runCLI("create-admin", "--tenant", "acme", "--email", "second@acme.test")
	if code != 0 {
		t.Fatalf("create-admin exit = %d stderr=%s", code, stderr)
	}
	password := createdPassword(t, stdout)
	if _, err := loginAs(t, db, "acme", "second", password); err != nil {
		t.Fatalf("login before disable: %v", err)
	}
	if code, stdout, stderr := runCLI("disable", "--tenant", "acme"); code != 0 {
		t.Fatalf("disable exit = %d stderr=%s stdout=%s", code, stderr, stdout)
	}
	if _, err := loginAs(t, db, "acme", "second", password); err == nil {
		t.Fatal("suspended tenant login succeeded")
	}
	code, stdout, stderr = runCLI("list")
	if code != 0 || !strings.Contains(stdout, "suspended") {
		t.Fatalf("list after disable = exit %d stdout=%s stderr=%s", code, stdout, stderr)
	}
	if code, stdout, stderr := runCLI("enable", "--tenant", "acme"); code != 0 {
		t.Fatalf("enable exit = %d stderr=%s stdout=%s", code, stderr, stdout)
	}
	if _, err := loginAs(t, db, "acme", "second", password); err != nil {
		t.Fatalf("login after enable: %v", err)
	}
	if code, _, stderr := runCLI("disable", "--tenant", "missing"); code == 0 || !strings.Contains(stderr, "does not exist") {
		t.Fatalf("disable missing tenant = exit %d stderr %q", code, stderr)
	}
}

func TestUsageErrors(t *testing.T) {
	cliEnvironment(t)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"unknown command", []string{"frobnicate"}, "unknown command"},
		{"create without flags", []string{"create"}, "--slug and --name are required"},
		{"create missing name", []string{"create", "--slug", "acme"}, "--slug and --name are required"},
		{"create admin password without email", []string{"create", "--slug", "acme", "--name", "Acme", "--admin-password", "a-strong-password"}, "requires --admin-email"},
		{"create-admin missing flags", []string{"create-admin"}, "--tenant and --email are required"},
		{"disable missing tenant", []string{"disable"}, "--tenant is required"},
		{"list extra arg", []string{"list", "extra"}, "unexpected argument"},
	} {
		code, stdout, stderr := runCLI(tc.args...)
		if code != exitUsage {
			t.Fatalf("%s: exit = %d, want %d (stdout=%s stderr=%s)", tc.name, code, exitUsage, stdout, stderr)
		}
		if !strings.Contains(stderr, tc.want) {
			t.Fatalf("%s: stderr = %q, want substring %q", tc.name, stderr, tc.want)
		}
	}
	if code, stdout, _ := runCLI("help"); code != exitOK || !strings.Contains(stdout, "tenant create") {
		t.Fatalf("help = exit %d stdout %q", code, stdout)
	}
}
