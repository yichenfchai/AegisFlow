package auth

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yichenfchai/AegisFlow/backend/internal/tenants"
)

// TestFindUserByEmailLinksOnlyStoredEmails verifies the OIDC account-linkage
// rule: lookup is case-insensitive, requires a real email value, and accounts
// without one are never assumed to be the IdP identity.
func TestFindUserByEmailLinksOnlyStoredEmails(t *testing.T) {
	s, db := testService(t)
	ctx := context.Background()
	if _, err := s.FindUserByEmail(ctx, tenants.DefaultID, "admin@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("account without email matched: %v", err)
	}
	if _, err := s.FindUserByEmail(ctx, tenants.DefaultID, "   "); !errors.Is(err, ErrValidation) {
		t.Fatalf("empty email accepted: %v", err)
	}
	member, err := s.CreateUser(ctx, testActor, "mail-user", testPassword, "", "member")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE auth_users SET email='Person@Example.com' WHERE id=$1`, member.ID); err != nil {
		t.Fatal(err)
	}
	found, err := s.FindUserByEmail(ctx, tenants.DefaultID, "person@example.COM")
	if err != nil || found.ID != member.ID {
		t.Fatalf("case-insensitive email lookup = %+v, %v", found, err)
	}
	var adminID string
	if err = db.QueryRow(`SELECT id FROM auth_users WHERE username='admin'`).Scan(&adminID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE auth_users SET email='PERSON@example.com' WHERE id=$1`, adminID); err == nil {
		t.Fatal("email uniqueness is not enforced case-insensitively")
	}
}

// TestProvisionOIDCUserCreatesMemberOnly covers the auto-provision boundary:
// member role only, collision-safe usernames, unusable random password, and
// reuse of the already-linked account for a concurrent/retried first login.
func TestProvisionOIDCUserCreatesMemberOnly(t *testing.T) {
	s, db := testService(t)
	ctx := context.Background()
	// "admin" collides with the bootstrap administrator, so a suffix is added
	// instead of touching the existing account.
	user, err := s.ProvisionOIDCUser(ctx, tenants.DefaultID, "admin", "SSO Admin Candidate", "admin@example.com", "req-provision")
	if err != nil {
		t.Fatal(err)
	}
	if user.Role != "member" || user.Disabled || user.Username == "admin" || !strings.HasPrefix(user.Username, "admin-") {
		t.Fatalf("unsafe provisioned user: %+v", user)
	}
	var email, hash string
	if err = db.QueryRow(`SELECT email,password_hash FROM auth_users WHERE id=$1`, user.ID).Scan(&email, &hash); err != nil {
		t.Fatal(err)
	}
	if email != "admin@example.com" || !strings.HasPrefix(hash, "$2") {
		t.Fatalf("provisioned row = email %q hash prefix %.4q", email, hash)
	}
	// The password is random and discarded, so password login cannot succeed.
	if _, err = s.Login(ctx, user.Username, "any-known-password"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("provisioned account accepted a password: %v", err)
	}
	// A retried first login for the same verified email reuses the account.
	again, err := s.ProvisionOIDCUser(ctx, tenants.DefaultID, "someone-else", "", "ADMIN@example.com", "req-provision-2")
	if err != nil || again.ID != user.ID {
		t.Fatalf("same email did not reuse account: %+v, %v", again, err)
	}
	var auditCount int
	if err = db.QueryRow(`SELECT COUNT(*) FROM auth_audit WHERE action=$1 AND object_id=$2`, ActionUserCreate, user.ID).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 {
		t.Fatalf("auto-provision audit rows = %d, want 1", auditCount)
	}
	var detail string
	if err = db.QueryRow(`SELECT detail FROM auth_audit WHERE action=$1 AND object_id=$2`, ActionUserCreate, user.ID).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if detail != "method=oidc" || strings.Contains(detail, "@") {
		t.Fatalf("audit detail leaks or mislabels the identity: %q", detail)
	}
}

// TestLoginOIDCIssuesNormalSession verifies that SSO sessions reuse exactly the
// password-login session machinery (access token, refresh rotation, live role)
// and record only method=oidc in the login audit.
func TestLoginOIDCIssuesNormalSession(t *testing.T) {
	s, db := testService(t)
	ctx := context.Background()
	member, err := s.CreateUser(ctx, testActor, "sso-member", testPassword, "", "member")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE auth_users SET email='sso-member@example.com' WHERE id=$1`, member.ID); err != nil {
		t.Fatal(err)
	}
	linked, err := s.FindUserByEmail(ctx, tenants.DefaultID, "sso-member@example.com")
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := s.LoginOIDC(ctx, *linked, "req-sso")
	if err != nil {
		t.Fatal(err)
	}
	if tokens.RefreshToken == "" || tokens.Remember || tokens.ExpiresIn <= 0 || tokens.User.Role != "member" {
		t.Fatalf("unexpected SSO tokens: %+v", tokens)
	}
	if user, authErr := s.Authenticate(ctx, tokens.AccessToken); authErr != nil || user.ID != member.ID {
		t.Fatalf("SSO access token unusable: %+v, %v", user, authErr)
	}
	if _, err = s.Refresh(ctx, tokens.RefreshToken); err != nil {
		t.Fatalf("SSO refresh failed: %v", err)
	}
	var detail string
	if err = db.QueryRow(`SELECT detail FROM auth_audit WHERE action=$1 AND actor_id=$2 AND result=$3`, ActionLogin, member.ID, ResultSuccess).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if detail != "method=oidc" {
		t.Fatalf("login audit detail = %q", detail)
	}
	// A disabled linked account is refused before any session is created.
	if err = s.SetUserDisabled(ctx, testActor, member.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err = s.LoginOIDC(ctx, *linked, "req-sso-2"); !errors.Is(err, ErrAccountDisabled) {
		t.Fatalf("disabled account SSO login error = %v", err)
	}
}

// TestRecordOIDCLoginFailureStoresOnlyFixedCodes keeps the failure audit free
// of provider text and coerces unknown codes to the documented fallback.
func TestRecordOIDCLoginFailureStoresOnlyFixedCodes(t *testing.T) {
	s, db := testService(t)
	ctx := context.Background()
	if err := s.RecordOIDCLoginFailure(ctx, "", "req-failure", "invalid_token", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordOIDCLoginFailure(ctx, "", "req-failure-2", "provider said: upstream secret", nil); err != nil {
		t.Fatal(err)
	}
	rows, err := db.QueryContext(ctx, `SELECT detail FROM auth_audit WHERE action=$1 AND result=$2 ORDER BY created_at, detail`, ActionLogin, ResultFailure)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var detail string
		if err = rows.Scan(&detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"method=oidc code=invalid_token": true, "method=oidc code=server_error": true}
	if len(details) != len(want) {
		t.Fatalf("failure audit details = %v", details)
	}
	for _, detail := range details {
		if !want[detail] {
			t.Fatalf("unexpected failure audit detail %q", detail)
		}
	}
}
