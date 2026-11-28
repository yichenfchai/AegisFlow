package auth

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yichenfchai/AegisFlow/backend/internal/ldap"
	"github.com/yichenfchai/AegisFlow/backend/internal/tenants"
)

// fakeDirectory is a programmable LDAP directory surface for account-resolution
// tests. The real wire-level directory behavior is covered by internal/ldap and
// the httpapi integration tests.
type fakeDirectory struct {
	identity *ldap.Identity
	err      error
	label    string
	calls    int
	username string
	password string
}

func (f *fakeDirectory) Authenticate(_ context.Context, username, password string) (*ldap.Identity, error) {
	f.calls++
	f.username = username
	f.password = password
	if f.err != nil {
		return nil, f.err
	}
	return f.identity, nil
}

func (f *fakeDirectory) Label() string { return f.label }

func fakeIdentity(username, email, displayName string) *fakeDirectory {
	return &fakeDirectory{identity: &ldap.Identity{Username: username, Email: email, DisplayName: displayName}, label: "LDAP 登录"}
}

func enableLDAP(t *testing.T, s *Service, directory *fakeDirectory, autoProvision bool) *Service {
	t.Helper()
	return s.WithLDAPDirectory(directory, autoProvision)
}

// TestLoginLDAPLinksExistingAccount covers the ordinary linked-account path:
// the directory only proves the password, the local role and account state stay
// authoritative and the session is the same machinery as a password login.
func TestLoginLDAPLinksExistingAccount(t *testing.T) {
	s, db := testService(t)
	ctx := context.Background()
	member, err := s.CreateUser(ctx, testActor, "ldap-user", testPassword, "", "member")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE auth_users SET email='Ldap.User@Example.com' WHERE id=$1`, member.ID); err != nil {
		t.Fatal(err)
	}
	directory := fakeIdentity("whatever", "ldap.user@example.com", "LDAP User")
	s = enableLDAP(t, s, directory, false)
	user, err := s.findLDAPLocalUser(ctx, "", "ignored", "ldap.user@example.com")
	if err != nil || user.ID != member.ID {
		t.Fatalf("case-insensitive email lookup = %+v, %v", user, err)
	}
	tokens, err := s.LoginLDAP(ctx, "", "ldap-user", "directory-password", "req-ldap")
	if err != nil {
		t.Fatal(err)
	}
	if tokens.RefreshToken == "" || tokens.Remember || tokens.ExpiresIn <= 0 || tokens.User.Role != "member" {
		t.Fatalf("LDAP tokens = %+v", tokens)
	}
	if tokens.User.Tenant == nil || tokens.User.Tenant.Slug != tenants.DefaultSlug {
		t.Fatalf("LDAP tokens carry no tenant metadata: %+v", tokens.User.Tenant)
	}
	if directory.username != "ldap-user" || directory.password != "directory-password" {
		t.Fatalf("directory received %q/%q", directory.username, directory.password)
	}
	if _, err = s.Authenticate(ctx, tokens.AccessToken); err != nil {
		t.Fatalf("LDAP access token unusable: %v", err)
	}
	if _, err = s.Refresh(ctx, tokens.RefreshToken); err != nil {
		t.Fatalf("LDAP refresh failed: %v", err)
	}
	var detail string
	if err = db.QueryRow(`SELECT detail FROM auth_audit WHERE action=$1 AND actor_id=$2 AND result=$3 ORDER BY id DESC LIMIT 1`,
		ActionLogin, member.ID, ResultSuccess).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if detail != "method=ldap tenant=default" {
		t.Fatalf("login audit detail = %q", detail)
	}

	// A disabled linked account is refused after the directory proves the
	// password, and no session is created.
	if err = s.SetUserDisabled(ctx, testActor, member.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err = s.LoginLDAP(ctx, "", "ldap-user", "directory-password", "req-ldap-2"); !errors.Is(err, ErrAccountDisabled) {
		t.Fatalf("disabled account LDAP error = %v", err)
	}
}

// TestLoginLDAPErrors pins the failure mapping: directory credential problems
// are a generic 401, dependency problems a 503-style error, and the local
// account policy decides the rest.
func TestLoginLDAPErrors(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	unconfigured := s
	if _, err := unconfigured.LoginLDAP(ctx, "", "user", "pass", "req"); !errors.Is(err, ErrDependencyUnavailable) {
		t.Fatalf("unconfigured LDAP error = %v", err)
	}
	for _, directoryErr := range []error{ldap.ErrInvalidCredentials, ldap.ErrAmbiguous, ldap.ErrInvalidEntry} {
		s = enableLDAP(t, s, &fakeDirectory{err: directoryErr, label: "LDAP"}, false)
		if _, err := s.LoginLDAP(ctx, "", "user", "pass", "req"); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("directory error %v mapped to %v", directoryErr, err)
		}
	}
	s = enableLDAP(t, s, &fakeDirectory{err: ldap.ErrUnavailable, label: "LDAP"}, false)
	if _, err := s.LoginLDAP(ctx, "", "user", "pass", "req"); !errors.Is(err, ErrDependencyUnavailable) {
		t.Fatalf("unavailable directory error = %v", err)
	}
	// Unknown local account with auto-provisioning disabled.
	s = enableLDAP(t, s, fakeIdentity("ghost", "ghost@example.com", "Ghost"), false)
	if _, err := s.LoginLDAP(ctx, "", "ghost", "pass", "req"); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("unknown account error = %v", err)
	}
	// Invalid tenant slug is a client validation error, before any session.
	if _, err := s.LoginLDAP(ctx, "Not A Slug", "ghost", "pass", "req"); !errors.Is(err, ErrValidation) {
		t.Fatalf("invalid slug error = %v", err)
	}
}

// TestLoginLDAPTenantResolution covers the F1 semantics reused by directory
// login: an identifier unique across tenants logs in, a duplicate requires a
// tenant slug (or auto-provisions only inside the named tenant), and an unknown
// tenant never creates an account.
func TestLoginLDAPTenantResolution(t *testing.T) {
	s, db := testService(t)
	ctx := context.Background()
	insertTenant(t, db, otherTenantID, "tenant-b", tenants.StatusActive)
	insertTenantUser(t, db, tenants.DefaultID, "shared-name", "x", "member", "a-shared@example.com")
	insertTenantUser(t, db, otherTenantID, "shared-name", "x", "member", "b-shared@example.com")
	insertTenantUser(t, db, tenants.DefaultID, "bob", "x", "member", "bob@example.com")

	directory := fakeIdentity("shared-name", "", "Shared")
	s = enableLDAP(t, s, directory, false)
	if _, err := s.LoginLDAP(ctx, "", "shared-name", "directory-password", "req"); !errors.Is(err, ErrTenantRequired) {
		t.Fatalf("ambiguous login error = %v", err)
	}
	// The email is empty, so the lookup matches by username only; with a tenant
	// slug the directory identity logs into that tenant.
	tokens, err := s.LoginLDAP(ctx, "tenant-b", "shared-name", "directory-password", "req")
	if err != nil {
		t.Fatal(err)
	}
	if tokens.User.Tenant == nil || tokens.User.Tenant.Slug != "tenant-b" {
		t.Fatalf("tenant-scoped login = %+v", tokens.User.Tenant)
	}
	// An unknown tenant is indistinguishable from an unlinked account.
	if _, err = s.LoginLDAP(ctx, "unknown-tenant", "shared-name", "directory-password", "req"); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("unknown tenant error = %v", err)
	}

	// A directory identity whose email matches exactly one tenant logs in
	// without a slug.
	directoryByEmail := fakeIdentity("bob", "BOB@example.com", "Bob")
	s = enableLDAP(t, s, directoryByEmail, false)
	tokens, err = s.LoginLDAP(ctx, "", "bob", "directory-password", "req")
	if err != nil {
		t.Fatalf("email-linked cross-tenant login: %v", err)
	}
	if tokens.User.Tenant == nil || tokens.User.Tenant.Slug != tenants.DefaultSlug {
		t.Fatalf("email-linked login tenant = %+v", tokens.User.Tenant)
	}

	// A suspended tenant cannot authenticate even with the right directory
	// password: the session path rejects it with the account_disabled outcome.
	if _, err = tenants.SetStatus(ctx, db, "tenant-b", tenants.StatusSuspended); err != nil {
		t.Fatal(err)
	}
	s = enableLDAP(t, s, fakeIdentity("shared-name", "", "Shared"), false)
	if _, err = s.LoginLDAP(ctx, "tenant-b", "shared-name", "directory-password", "req"); !errors.Is(err, ErrAccountDisabled) {
		t.Fatalf("suspended tenant error = %v", err)
	}
}

// TestLoginLDAPAutoProvision covers the member-only auto-provision policy:
// default and explicit tenants, trusted email requirement, unique username
// collision handling and the "unusable random password" property.
func TestLoginLDAPAutoProvision(t *testing.T) {
	s, db := testService(t)
	ctx := context.Background()
	insertTenant(t, db, otherTenantID, "tenant-b", tenants.StatusActive)

	// No email: auto-provisioning cannot create a trustworthy account.
	noEmail := fakeIdentity("no-email", "", "No Email")
	s = enableLDAP(t, s, noEmail, true)
	if _, err := s.LoginLDAP(ctx, "", "no-email", "directory-password", "req"); !errors.Is(err, ErrDirectoryEmailMissing) {
		t.Fatalf("missing directory email error = %v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM auth_users WHERE lower(username)='no-email'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("account created without email: count=%d err=%v", count, err)
	}

	// New identity: a member account is created in the default tenant with the
	// directory email, and the login audit records method=ldap tenant=<slug>.
	newUser := fakeIdentity("new-person", "new-person@example.com", "New Person")
	s = enableLDAP(t, s, newUser, true)
	tokens, err := s.LoginLDAP(ctx, "", "new-person", "directory-password", "req")
	if err != nil {
		t.Fatal(err)
	}
	if tokens.User.Role != "member" || tokens.User.Username != "new-person" {
		t.Fatalf("provisioned user = %+v", tokens.User)
	}
	if tokens.User.Tenant == nil || tokens.User.Tenant.Slug != tenants.DefaultSlug {
		t.Fatalf("provisioned tenant = %+v", tokens.User.Tenant)
	}
	var email, hash, createdDetail, loginDetail string
	if err = db.QueryRow(`SELECT email,password_hash FROM auth_users WHERE id=$1`, tokens.User.ID).Scan(&email, &hash); err != nil {
		t.Fatal(err)
	}
	if email != "new-person@example.com" || !strings.HasPrefix(hash, "$2") {
		t.Fatalf("provisioned row = email %q hash %q", email, hash)
	}
	if err = db.QueryRow(`SELECT detail FROM auth_audit WHERE action=$1 AND object_id=$2`, ActionUserCreate, tokens.User.ID).Scan(&createdDetail); err != nil {
		t.Fatal(err)
	}
	if createdDetail != "method=ldap" {
		t.Fatalf("user.create audit detail = %q", createdDetail)
	}
	if err = db.QueryRow(`SELECT detail FROM auth_audit WHERE action=$1 AND actor_id=$2 AND result=$3`, ActionLogin, tokens.User.ID, ResultSuccess).Scan(&loginDetail); err != nil {
		t.Fatal(err)
	}
	if loginDetail != "method=ldap tenant=default" {
		t.Fatalf("login audit detail = %q", loginDetail)
	}
	// The random local password is unusable, so only the directory can sign in.
	if _, err = s.Login(ctx, tokens.User.Username, "directory-password"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("provisioned account accepted a password login: %v", err)
	}
	// A retried first login for the same directory identity reuses the account.
	again, err := s.LoginLDAP(ctx, "", "new-person", "directory-password", "req-2")
	if err != nil || again.User.ID != tokens.User.ID {
		t.Fatalf("retried first login = %+v, %v", again, err)
	}

	// An explicit tenant slug provisions into that tenant.
	newUserB := fakeIdentity("tenant-b-person", "tenant-b@example.com", "Tenant B Person")
	s = enableLDAP(t, s, newUserB, true)
	tokensB, err := s.LoginLDAP(ctx, "tenant-b", "tenant-b-person", "directory-password", "req")
	if err != nil {
		t.Fatal(err)
	}
	if tokensB.User.Tenant == nil || tokensB.User.Tenant.Slug != "tenant-b" || tokensB.User.Role != "member" {
		t.Fatalf("tenant-scoped provisioning = %+v", tokensB.User)
	}
	if err = db.QueryRow(`SELECT COUNT(*) FROM auth_users WHERE tenant_id=$1 AND lower(email)='tenant-b@example.com'`, otherTenantID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("tenant-b account count = %d err=%v", count, err)
	}
}

// TestLoginLDAPExistingAccountWithoutEmail proves the documented fallback: a
// directory identity without a usable email can still log into an existing
// account matched by username.
func TestLoginLDAPExistingAccountWithoutEmail(t *testing.T) {
	s, db := testService(t)
	ctx := context.Background()
	member, err := s.CreateUser(ctx, testActor, "by-username", testPassword, "", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	s = enableLDAP(t, s, fakeIdentity("BY-USERNAME", "", ""), false)
	tokens, err := s.LoginLDAP(ctx, "", "by-username", "directory-password", "req")
	if err != nil {
		t.Fatalf("username-only lookup: %v", err)
	}
	if tokens.User.ID != member.ID || tokens.User.Role != "viewer" {
		t.Fatalf("linked user = %+v", tokens.User)
	}
	// The directory-provided display name is only used for newly provisioned
	// accounts; an existing account keeps its local display name.
	var display string
	if err = db.QueryRow(`SELECT display_name FROM auth_users WHERE id=$1`, member.ID).Scan(&display); err != nil || display != "by-username" {
		t.Fatalf("existing display name = %q err=%v", display, err)
	}
}

// TestLoginLDAPAmbiguousLocalAccounts refuses to guess when the directory
// identity matches two different local accounts inside the requested tenant.
func TestLoginLDAPAmbiguousLocalAccounts(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, testActor, "alice", testPassword, "", "member"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser(ctx, testActor, "alice-email", testPassword, "", "member"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE auth_users SET email='alice@example.com' WHERE username='alice-email'`); err != nil {
		t.Fatal(err)
	}
	s = enableLDAP(t, s, fakeIdentity("alice", "alice@example.com", "Alice"), false)
	if _, err := s.LoginLDAP(ctx, "", "alice", "directory-password", "req"); !errors.Is(err, ErrTenantRequired) {
		t.Fatalf("cross-tenant ambiguity error = %v", err)
	}
	if _, err := s.LoginLDAP(ctx, "default", "alice", "directory-password", "req"); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("in-tenant ambiguity error = %v", err)
	}
}
