// OIDC single sign-on account integration (P5 remaining modules, batch D).
// The OIDC package verifies the external id_token and owns the protocol; this
// file owns the local account side: verified-email lookup, session issuance
// through the same code path as a password login, and the member-only
// auto-provision path. No OIDC claim, token or provider error is ever written
// to the audit trail or a JSON response.
package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/bcrypt"

	"github.com/yichenfchai/AegisFlow/backend/internal/tenants"
)

// MaxEmailLength bounds the verified email used for account linkage.
const MaxEmailLength = 320

// oidcFailureCodes is the fixed, documented set of callback failure codes.
// Only these values may reach the audit detail or the login-page query string.
var oidcFailureCodes = map[string]bool{
	"state_expired":         true,
	"token_exchange_failed": true,
	"invalid_token":         true,
	"email_unverified":      true,
	"account_not_found":     true,
	"account_disabled":      true,
	"server_error":          true,
}

// NormalizeEmail trims and validates a verified email used for account
// linkage. The original case is kept; the lookup is case-insensitive through
// lower(email) and the unique index is built on the same expression.
func NormalizeEmail(raw string) (string, error) {
	email := strings.TrimSpace(raw)
	if email == "" || len(email) > MaxEmailLength || !utf8.ValidString(email) ||
		!strings.Contains(email, "@") || strings.IndexFunc(email, unicode.IsControl) >= 0 ||
		strings.ContainsAny(email, " \t") {
		return "", fmt.Errorf("%w: verified email must be 1-320 bytes, single line and contain @", ErrValidation)
	}
	return email, nil
}

// FindUserByEmail resolves the local account linked to a verified OIDC email
// inside one tenant. Only a non-empty email can match; an account without a
// stored email is never assumed to be the same person. Unknown email returns
// ErrNotFound. The same email may belong to different users in different
// tenants, which is exactly why the lookup is tenant-scoped.
func (s *Service) FindUserByEmail(ctx context.Context, tenantID, email string) (*User, error) {
	if err := s.valid(); err != nil {
		return nil, err
	}
	if err := requireTenantID(tenantID); err != nil {
		return nil, err
	}
	normalized, err := NormalizeEmail(email)
	if err != nil {
		return nil, err
	}
	user, err := scanUser(s.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM auth_users
		WHERE tenant_id=$1 AND email <> '' AND lower(email)=lower($2)`, tenantID, normalized))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return user, nil
}

// LoginOIDC issues the same revocable session and refresh cookie as a password
// login for an account whose identity was proven by the OIDC provider. The
// audit row reuses auth.login with detail "method=oidc".
func (s *Service) LoginOIDC(ctx context.Context, user User, requestID string) (*Tokens, error) {
	return s.loginVerifiedUser(ctx, user, requestID, "method=oidc")
}

// loginVerifiedUser issues a revocable session for an account whose external
// identity (OIDC or LDAP) has already been verified. The audit row reuses
// auth.login with the caller-supplied fixed detail (never a credential, DN or
// claim). Sessions are non-persistent: neither SSO flow carries a remember
// choice. The user row is re-read under a share lock so a disable that raced
// the external callback cannot leave a live session; the tenant row is read
// under the same transaction so a suspended tenant cannot start a session.
func (s *Service) loginVerifiedUser(ctx context.Context, user User, requestID, detail string) (*Tokens, error) {
	if err := s.valid(); err != nil {
		return nil, err
	}
	if !ValidID(user.ID) {
		return nil, fmt.Errorf("%w: invalid user id", ErrValidation)
	}
	if err := requireTenantID(user.TenantID); err != nil {
		return nil, err
	}
	sid, err := randomToken(18)
	if err != nil {
		return nil, err
	}
	refresh, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	expires := now + int64(s.refreshTTL/time.Second)
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// The share lock mirrors password login: a concurrent disable/revoke either
	// commits first (and this callback sees disabled here) or runs after this
	// transaction, revoking the session it just created.
	var current User
	var disabled int
	var tenantStatus, tenantSlug, tenantName string
	err = tx.QueryRowContext(ctx, `SELECT `+qualifiedUserColumns+`,t.status,t.slug,t.name FROM auth_users u
		JOIN tenants t ON t.id=u.tenant_id WHERE u.id=$1 FOR SHARE OF u, t`, user.ID).
		Scan(&current.TenantID, &current.ID, &current.Username, &current.DisplayName, &current.Role, &disabled, &current.CreatedAt, &tenantStatus, &tenantSlug, &tenantName)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUnauthorized
	}
	if err != nil {
		return nil, err
	}
	current.Tenant = tenantInfoOf(current.TenantID, tenantSlug, tenantName)
	current.Disabled = disabled != 0
	if current.Disabled {
		return nil, ErrAccountDisabled
	}
	if current.TenantID != user.TenantID {
		return nil, ErrUnauthorized
	}
	// A suspended tenant cannot open an external-login session. The browser-
	// facing code set of both flows only distinguishes "cannot sign in", and
	// account_disabled is the closest documented outcome; the tenant state
	// itself is never disclosed.
	if tenantStatus != tenants.StatusActive {
		return nil, ErrAccountDisabled
	}
	tokens, err := s.issue(current, sid, refresh, expires, false)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO auth_sessions(id,tenant_id,user_id,expires_at,remember,created_at) VALUES($1,$2,$3,$4,0,$5)`, sid, current.TenantID, current.ID, expires, now); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO auth_refresh_tokens(token_hash,session_id) VALUES($1,$2)`, hashToken(refresh), sid); err != nil {
		return nil, err
	}
	if err = s.writeAudit(ctx, tx, Actor{ID: current.ID, TenantID: current.TenantID, Username: current.Username, RequestID: requestID}, ActionLogin, "user", current.ID, ResultSuccess, detail); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return tokens, nil
}

// RecordOIDCLoginFailure writes the failure half of the OIDC audit trail. Only
// a fixed error code is stored: provider error bodies, tokens and claims stay
// out of the database and logs. tenantID is the tenant recorded by the
// consumed login state (empty when the failure happened before it was read);
// the auth service attributes a missing tenant to the deployment's default
// tenant, which is the only tenant in phase F1. The caller already applied the
// shared login rate limiter, so failed callbacks cannot be used to grow the
// audit table without bound.
func (s *Service) RecordOIDCLoginFailure(ctx context.Context, tenantID, requestID, code string, user *User) error {
	if !oidcFailureCodes[code] {
		code = "server_error"
	}
	actor := Actor{TenantID: tenantID, RequestID: requestID}
	objectID := ""
	if user != nil {
		actor.ID = user.ID
		actor.TenantID = user.TenantID
		actor.Username = user.Username
		objectID = user.ID
	}
	return s.writeAuditStandalone(ctx, actor, ActionLogin, "user", objectID, ResultFailure, "method=oidc code="+code)
}

// ProvisionOIDCUser creates the local member account for a verified external
// identity inside the given tenant. It can never create an admin and never
// overrides an existing username: a collision gets a random suffix. The
// password is cryptographically random and discarded, so the account is only
// reachable through OIDC until an administrator resets or disables it. If a
// concurrent first login already linked the same email in this tenant, that
// existing account is returned instead of failing.
func (s *Service) ProvisionOIDCUser(ctx context.Context, tenantID, preferredUsername, displayName, email, requestID string) (*User, error) {
	normalized, err := NormalizeEmail(email)
	if err != nil {
		return nil, err
	}
	return s.provisionExternalUser(ctx, tenantID, preferredUsername, displayName, normalized, requestID, "oidc")
}

// provisionExternalUser is the single auto-provisioning path for verified
// external identities (OIDC and LDAP). The caller must already have normalized
// a trusted email. The created account is always a member, the password is a
// random unusable value and the audit row records only the fixed method. An
// existing account linked to the same email in this tenant (a concurrent first
// login) is reused instead of creating a duplicate identity.
func (s *Service) provisionExternalUser(ctx context.Context, tenantID, preferredUsername, displayName, email, requestID, method string) (*User, error) {
	if err := s.valid(); err != nil {
		return nil, err
	}
	if err := requireTenantID(tenantID); err != nil {
		return nil, err
	}
	var tenantStatus string
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM tenants WHERE id=$1`, tenantID).Scan(&tenantStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: tenant", ErrNotFound)
		}
		return nil, err
	}
	if tenantStatus != tenants.StatusActive {
		return nil, ErrAccountDisabled
	}
	base := oidcUsernameBase(preferredUsername, email)
	display := oidcDisplayName(displayName, base)
	// A valid bcrypt hash of a random, unused password keeps the row shape of a
	// normal account while making password login impossible for it.
	password, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	detail := "method=" + method
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		username := base
		if attempt > 0 {
			suffix, suffixErr := randomToken(3)
			if suffixErr != nil {
				return nil, suffixErr
			}
			username = oidcUsernameWithSuffix(base, suffix)
		}
		id, idErr := randomToken(18)
		if idErr != nil {
			return nil, idErr
		}
		now := time.Now().Unix()
		tx, txErr := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
		if txErr != nil {
			return nil, txErr
		}
		_, insertErr := tx.ExecContext(ctx, `INSERT INTO auth_users(id,tenant_id,username,password_hash,display_name,role,disabled,created_at,email)
		 VALUES($1,$2,$3,$4,$5,'member',0,$6,$7)`, id, tenantID, username, string(hash), display, now, email)
		if insertErr != nil {
			_ = tx.Rollback()
			if !isUniqueViolation(insertErr) {
				return nil, insertErr
			}
			// A concurrent callback may have just created the same email link:
			// reuse it instead of creating a duplicate identity.
			existing, findErr := s.FindUserByEmail(ctx, tenantID, email)
			if findErr == nil {
				return existing, nil
			}
			if !errors.Is(findErr, ErrNotFound) {
				return nil, findErr
			}
			lastErr = insertErr
			continue
		}
		// The account is created by the provider-backed identity itself, so the
		// actor stays empty and only the method is recorded (no email in audit).
		if auditErr := s.writeAudit(ctx, tx, Actor{TenantID: tenantID, RequestID: requestID}, ActionUserCreate, "user", id, ResultSuccess, detail); auditErr != nil {
			return nil, auditErr
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return nil, commitErr
		}
		return &User{ID: id, TenantID: tenantID, Username: username, DisplayName: display, Role: "member", CreatedAt: now}, nil
	}
	return nil, fmt.Errorf("provision external account: %w", lastErr)
}

// isUniqueViolation reports a PostgreSQL unique-constraint violation, which is
// the expected outcome of a concurrent username/email auto-provision.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// oidcUsernameBase derives the preferred local username from the provider's
// preferred_username claim, falling back to the verified email local part and
// finally to a fixed handle. The result always matches usernamePattern.
func oidcUsernameBase(preferredUsername, email string) string {
	if candidate := sanitizeOIDCUsername(preferredUsername); candidate != "" {
		return candidate
	}
	if at := strings.Index(email, "@"); at > 0 {
		if candidate := sanitizeOIDCUsername(email[:at]); candidate != "" {
			return candidate
		}
	}
	return "sso-user"
}

// oidcUsernameWithSuffix appends a short random suffix to a colliding base so
// auto-provisioning keeps working without overwriting anyone.
func oidcUsernameWithSuffix(base, suffix string) string {
	room := 64 - len(suffix) - 1
	if room < 3 {
		room = 3
	}
	if len(base) > room {
		base = base[:room]
	}
	return base + "-" + suffix
}

// sanitizeOIDCUsername keeps only the username alphabet, drops a leading
// separator and enforces the 3-64 byte username rules. Invalid input yields an
// empty string so the caller can pick another candidate.
func sanitizeOIDCUsername(raw string) string {
	var builder strings.Builder
	for _, r := range strings.TrimSpace(raw) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			builder.WriteRune(r)
		default:
			builder.WriteByte('-')
		}
	}
	candidate := strings.TrimLeft(builder.String(), ".-_")
	if len(candidate) > 64 {
		candidate = candidate[:64]
	}
	if len(candidate) < 3 || !usernamePattern.MatchString(candidate) {
		return ""
	}
	return candidate
}

// oidcDisplayName sanitizes the provider name claim, which is display-only and
// never used for authorization or account lookup.
func oidcDisplayName(raw, fallback string) string {
	name := strings.TrimSpace(raw)
	if utf8.ValidString(name) {
		name = strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return -1
			}
			return r
		}, name)
		for len(name) > 256 {
			_, size := utf8.DecodeLastRuneInString(name)
			name = name[:len(name)-size]
		}
	} else {
		name = ""
	}
	if strings.TrimSpace(name) == "" {
		return fallback
	}
	return name
}
