package auth

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/yichenfchai/AegisFlow/backend/internal/tenants"
)

const otherTenantID = "tenant_other_0000000000000001"

// insertTenant adds a second tenant directly: phase F1 deliberately ships no
// tenant-creation API, so tests seed the row themselves.
func insertTenant(t *testing.T, db *sql.DB, id, slug, status string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO tenants(id,slug,name,status,created_at,updated_at) VALUES($1,$2,$3,$4,1,1)`, id, slug, slug, status); err != nil {
		t.Fatal(err)
	}
}

// insertTenantUser creates a user in an arbitrary tenant with a real bcrypt
// hash, so login tests can authenticate it.
func insertTenantUser(t *testing.T, db *sql.DB, tenantID, username, password, role, email string) string {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	id, err := randomToken(18)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO auth_users(id,tenant_id,username,password_hash,display_name,role,disabled,created_at,email)
		VALUES($1,$2,$3,$4,$3,$5,0,1,$6)`, id, tenantID, username, string(hash), role, email); err != nil {
		t.Fatal(err)
	}
	return id
}

func accessClaimsOf(t *testing.T, accessToken string) *accessClaims {
	t.Helper()
	parsed, err := jwt.ParseWithClaims(accessToken, &accessClaims{}, func(*jwt.Token) (any, error) {
		return []byte(testSecret), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	claims := parsed.Claims.(*accessClaims)
	if claims.TenantID == "" {
		t.Fatal("access token carries no tid claim")
	}
	return claims
}

func TestDefaultTenantStartupGuaranteeAndBootstrapPlacement(t *testing.T) {
	s, db := testService(t)
	ctx := context.Background()

	// The schema seeds the tenant; startup may rename the slug while keeping
	// the fixed id, so every existing row keeps its tenant.
	renamed, err := s.EnsureDefaultTenant(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if renamed.ID != tenants.DefaultID || renamed.Slug != "acme" {
		t.Fatalf("renamed default tenant = %+v", renamed)
	}
	defaultTenant, err := s.DefaultTenant(ctx)
	if err != nil || defaultTenant.ID != tenants.DefaultID || defaultTenant.Slug != "acme" {
		t.Fatalf("DefaultTenant = %+v, %v", defaultTenant, err)
	}

	// The bootstrap administrator lives in the deployment's default tenant.
	tokens := loginTest(t, s)
	if tokens.User.TenantID != tenants.DefaultID {
		t.Fatalf("bootstrap admin tenant = %q, want %q", tokens.User.TenantID, tenants.DefaultID)
	}
	var stored string
	if err = db.QueryRowContext(ctx, `SELECT tenant_id FROM auth_users WHERE id=$1`, tokens.User.ID).Scan(&stored); err != nil || stored != tenants.DefaultID {
		t.Fatalf("stored admin tenant = %q, %v", stored, err)
	}

	// Logins resolve the renamed slug and no longer the seeded one.
	withSlug, err := s.LoginWithTenant(ctx, "acme", "admin", testPassword, false, "req-renamed")
	if err != nil || withSlug.User.TenantID != tenants.DefaultID {
		t.Fatalf("login with renamed slug = %+v, %v", withSlug, err)
	}
	if _, err = s.LoginWithTenant(ctx, tenants.DefaultSlug, "admin", testPassword, false, ""); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("login with the replaced seed slug error = %v", err)
	}

	// A restart configured with the old slug must fail closed instead of
	// silently serving a deployment whose default tenant is unreachable.
	if _, err = s.EnsureDefaultTenant(ctx, tenants.DefaultSlug); err == nil {
		t.Fatal("startup accepted a missing default tenant after rename")
	}
}

func TestLoginTenantResolution(t *testing.T) {
	s, db := testService(t)
	ctx := context.Background()
	insertTenant(t, db, otherTenantID, "other", tenants.StatusActive)
	otherAdminID := insertTenantUser(t, db, otherTenantID, "admin", testPassword, "admin", "")
	// Same mailbox in two tenants is allowed; the unique index is per tenant.
	if _, err := db.Exec(`UPDATE auth_users SET email='shared@example.com' WHERE tenant_id=$1`, tenants.DefaultID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE auth_users SET email='SHARED@example.com' WHERE id=$1`, otherAdminID); err != nil {
		t.Fatal(err)
	}

	// Without a slug the duplicated username is ambiguous: the client must pick
	// a workspace. The password is not even checked on this path.
	if _, err := s.LoginWithTenant(ctx, "", "admin", testPassword, false, ""); !errors.Is(err, ErrTenantRequired) {
		t.Fatalf("ambiguous login error = %v", err)
	}
	// With a slug both tenants can be addressed independently.
	defaultLogin, err := s.LoginWithTenant(ctx, "default", "admin", testPassword, false, "")
	if err != nil || defaultLogin.User.TenantID != tenants.DefaultID {
		t.Fatalf("default-tenant login = %+v, %v", defaultLogin, err)
	}
	otherLogin, err := s.LoginWithTenant(ctx, "other", "admin", testPassword, false, "")
	if err != nil || otherLogin.User.TenantID != otherTenantID {
		t.Fatalf("other-tenant login = %+v, %v", otherLogin, err)
	}

	// Wrong password, unknown tenant, unknown user and invalid slug all fail:
	// the tenant/user/password failures stay indistinguishable (ErrUnauthorized).
	for _, tc := range []struct {
		name, slug, username, password string
		wantErr                        error
	}{
		{"wrong password", "default", "admin", "wrong-password-xx", ErrUnauthorized},
		{"wrong password in other tenant", "other", "admin", "wrong-password-xx", ErrUnauthorized},
		{"unknown tenant", "missing", "admin", testPassword, ErrUnauthorized},
		{"unknown user", "default", "no-such-user", testPassword, ErrUnauthorized},
		{"unknown user and tenant", "missing", "no-such-user", testPassword, ErrUnauthorized},
		{"invalid slug", "Bad Slug", "admin", testPassword, ErrValidation},
	} {
		if _, err := s.LoginWithTenant(ctx, tc.slug, tc.username, tc.password, false, ""); !errors.Is(err, tc.wantErr) {
			t.Fatalf("%s: error = %v, want %v", tc.name, err, tc.wantErr)
		}
	}

	// A username unique across tenants still logs in without a slug.
	insertTenantUser(t, db, otherTenantID, "solo-member", testPassword, "member", "")
	solo, err := s.LoginWithTenant(ctx, "", "solo-member", testPassword, false, "")
	if err != nil || solo.User.TenantID != otherTenantID {
		t.Fatalf("unique cross-tenant login = %+v, %v", solo, err)
	}

	// A suspended tenant cannot authenticate, with or without a slug.
	if _, err = db.Exec(`UPDATE tenants SET status='suspended' WHERE id=$1`, otherTenantID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.LoginWithTenant(ctx, "other", "admin", testPassword, false, ""); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("suspended tenant login with slug error = %v", err)
	}
	if _, err = s.LoginWithTenant(ctx, "", "solo-member", testPassword, false, ""); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("suspended tenant login without slug error = %v", err)
	}
}

func TestJWTRequiresMatchingTenantClaim(t *testing.T) {
	s, db := testService(t)
	insertTenant(t, db, otherTenantID, "other", tenants.StatusActive)
	tokens := loginTest(t, s)

	// The token carries tid = the session/user tenant.
	claims := accessClaimsOf(t, tokens.AccessToken)
	if claims.TenantID != tenants.DefaultID || claims.SessionID == "" || claims.Subject != tokens.User.ID {
		t.Fatalf("claims = tenant %q session %q subject %q", claims.TenantID, claims.SessionID, claims.Subject)
	}
	parts := strings.Split(tokens.AccessToken, ".")
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err = json.Unmarshal(payload, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["tid"] != tenants.DefaultID {
		t.Fatalf("JWT tid = %v, want %q", raw["tid"], tenants.DefaultID)
	}

	// A token that is correctly signed but claims another tenant is rejected:
	// the session, user and tenant rows must all agree.
	original := *claims
	otherTenant := original
	otherTenant.TenantID = otherTenantID
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, otherTenant).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatal(err)
	}
	assertUnauthorized(t, s, signed)
	missingTenant := original
	missingTenant.TenantID = ""
	unsignedTid, err := jwt.NewWithClaims(jwt.SigningMethodHS256, missingTenant).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatal(err)
	}
	assertUnauthorized(t, s, unsignedTid)
}

func TestRefreshRejectsSuspendedOrCrossTenantSession(t *testing.T) {
	s, db := testService(t)
	ctx := context.Background()
	insertTenant(t, db, otherTenantID, "other", tenants.StatusActive)

	// A session row whose tenant does not match the user's tenant is never
	// rotated: it is revoked and the caller is unauthenticated.
	crossTenant := loginTest(t, s)
	_, crossSessionID, err := s.InspectToken(ctx, crossTenant.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE auth_sessions SET tenant_id=$1 WHERE id=$2`, otherTenantID, crossSessionID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Refresh(ctx, crossTenant.RefreshToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("cross-tenant refresh error = %v", err)
	}
	var revoked int
	if err = db.QueryRow(`SELECT revoked FROM auth_sessions WHERE id=$1`, crossSessionID).Scan(&revoked); err != nil || revoked != 1 {
		t.Fatalf("cross-tenant session revoked = %d, %v", revoked, err)
	}

	// A suspended tenant blocks both refresh and existing access tokens, and
	// the refresh permanently revokes the session instead of only the token.
	suspended := loginTest(t, s)
	_, suspendedSessionID, err := s.InspectToken(ctx, suspended.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE tenants SET status='suspended' WHERE id=$1`, tenants.DefaultID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Refresh(ctx, suspended.RefreshToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("suspended-tenant refresh error = %v", err)
	}
	if err = db.QueryRow(`SELECT revoked FROM auth_sessions WHERE id=$1`, suspendedSessionID).Scan(&revoked); err != nil || revoked != 1 {
		t.Fatalf("suspended-tenant session revoked = %d, %v", revoked, err)
	}
	assertUnauthorized(t, s, suspended.AccessToken)
	if _, err = db.Exec(`UPDATE tenants SET status='active' WHERE id=$1`, tenants.DefaultID); err != nil {
		t.Fatal(err)
	}
	// Reactivating the tenant does not resurrect the revoked session.
	if _, err = s.Refresh(ctx, suspended.RefreshToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("refresh after reactivation = %v", err)
	}
}

func TestTenantScopedUserManagement(t *testing.T) {
	s, db := testService(t)
	ctx := context.Background()
	insertTenant(t, db, otherTenantID, "other", tenants.StatusActive)
	otherAdminID := insertTenantUser(t, db, otherTenantID, "other-admin", testPassword, "admin", "")

	actor := Actor{ID: "actor-default-000000001", TenantID: tenants.DefaultID, Username: "admin", RequestID: "req-tenant"}
	created, err := s.CreateUser(ctx, actor, "tenant-member", testPassword, "", "member")
	if err != nil {
		t.Fatal(err)
	}
	if created.TenantID != tenants.DefaultID {
		t.Fatalf("created user tenant = %q, want %q", created.TenantID, tenants.DefaultID)
	}

	// Listing never crosses the tenant boundary.
	defaultUsers, defaultTotal, err := s.ListUsersPage(ctx, tenants.DefaultID, 50, 0)
	if err != nil || defaultTotal != 2 || len(defaultUsers) != 2 {
		t.Fatalf("default user page = %d/%d, %v", defaultTotal, len(defaultUsers), err)
	}
	for _, user := range defaultUsers {
		if user.TenantID != tenants.DefaultID {
			t.Fatalf("default listing leaked tenant %q", user.TenantID)
		}
	}
	otherUsers, otherTotal, err := s.ListUsersPage(ctx, otherTenantID, 50, 0)
	if err != nil || otherTotal != 1 || len(otherUsers) != 1 || otherUsers[0].TenantID != otherTenantID {
		t.Fatalf("other tenant user page = %+v, %d, %v", otherUsers, otherTotal, err)
	}

	// Every administrator mutation resolves the target inside the actor's
	// tenant only: a cross-tenant user id is a 404, never a write.
	viewer := "viewer"
	if err = s.UpdateUser(ctx, actor, otherAdminID, &viewer, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant role change error = %v", err)
	}
	if err = s.SetUserDisabled(ctx, actor, otherAdminID, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant disable error = %v", err)
	}
	if err = s.ResetPassword(ctx, actor, otherAdminID, "reset-password-0001"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant password reset error = %v", err)
	}
	if err = s.ChangePassword(ctx, actor, otherAdminID, testPassword, "changed-password-0001", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant password change error = %v", err)
	}
	if err = s.RevokeSession(ctx, actor, otherAdminID, "missingsession0000001"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant session revoke error = %v", err)
	}

	// The last-admin guard is per tenant: the other tenant's administrator does
	// not keep the default tenant from locking itself out.
	admin := loginTest(t, s)
	if err = s.SetUserDisabled(ctx, actor, admin.User.ID, true); !errors.Is(err, ErrValidation) {
		t.Fatalf("disable last admin in default tenant error = %v", err)
	}

	// Audits are tenant scoped: the default tenant admin cannot see the other
	// tenant's rows and the other tenant has no audit rows yet.
	defaultEntries, defaultAuditTotal, err := s.ListAudit(ctx, tenants.DefaultID, AuditFilter{}, 100, 0)
	if err != nil || defaultAuditTotal == 0 || len(defaultEntries) == 0 {
		t.Fatalf("default audit page = %d/%d, %v", defaultAuditTotal, len(defaultEntries), err)
	}
	if _, otherAuditTotal, err := s.ListAudit(ctx, otherTenantID, AuditFilter{}, 100, 0); err != nil || otherAuditTotal != 0 {
		t.Fatalf("other tenant audit total = %d, %v", otherAuditTotal, err)
	}

	// An actor without a tenant fails closed instead of writing to a default.
	if _, err = s.CreateUser(ctx, Actor{}, "no-tenant", testPassword, "", "member"); !errors.Is(err, ErrValidation) {
		t.Fatalf("actor without tenant error = %v", err)
	}
}

func TestLoginAuditRecordsTenantSlug(t *testing.T) {
	s, db := testService(t)
	ctx := context.Background()
	loginTest(t, s)
	var detail string
	if err := db.QueryRowContext(ctx, `SELECT detail FROM auth_audit WHERE action=$1 AND result=$2 ORDER BY created_at DESC,id DESC LIMIT 1`, ActionLogin, ResultSuccess).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if detail != "tenant=default" {
		t.Fatalf("login audit detail = %q, want tenant=default", detail)
	}
}

func TestOIDCAccountLinkageIsTenantScoped(t *testing.T) {
	s, db := testService(t)
	ctx := context.Background()
	member, err := s.CreateUser(ctx, testActor, "sso-user", testPassword, "", "member")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE auth_users SET email='shared@example.com' WHERE id=$1`, member.ID); err != nil {
		t.Fatal(err)
	}
	insertTenant(t, db, otherTenantID, "other", tenants.StatusActive)

	// The verified email does not leak across tenants.
	if _, err = s.FindUserByEmail(ctx, otherTenantID, "shared@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant email lookup error = %v", err)
	}
	// Auto-provisioning creates a distinct account in the other tenant with the
	// same verified email: per-tenant email uniqueness allows it.
	provisioned, err := s.ProvisionOIDCUser(ctx, otherTenantID, "sso-user", "", "SHARED@example.com", "req-oidc")
	if err != nil {
		t.Fatal(err)
	}
	if provisioned.TenantID != otherTenantID || provisioned.ID == member.ID || provisioned.Role != "member" {
		t.Fatalf("provisioned user = %+v", provisioned)
	}
	if found, lookupErr := s.FindUserByEmail(ctx, tenants.DefaultID, "shared@example.com"); lookupErr != nil || found.ID != member.ID {
		t.Fatalf("default tenant link disturbed: %+v, %v", found, lookupErr)
	}
	// The SSO session is bound to the provisioned user's tenant.
	tokens, err := s.LoginOIDC(ctx, *provisioned, "req-oidc-login")
	if err != nil {
		t.Fatal(err)
	}
	if tokens.User.TenantID != otherTenantID {
		t.Fatalf("OIDC session tenant = %q, want %q", tokens.User.TenantID, otherTenantID)
	}
	if _, err = db.Exec(`UPDATE tenants SET status='suspended' WHERE id=$1`, otherTenantID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.LoginOIDC(ctx, *provisioned, "req-oidc-suspended"); !errors.Is(err, ErrAccountDisabled) {
		t.Fatalf("suspended tenant OIDC login error = %v", err)
	}
}
