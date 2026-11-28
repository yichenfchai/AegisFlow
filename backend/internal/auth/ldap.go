// LDAP directory authentication account integration (phase G). The internal
// ldap package proves the external identity; this file owns the local account
// side: tenant-scoped lookup by directory username/email, the member-only
// auto-provision policy and session issuance through the same transaction and
// cookie path as a password login. No password, bind credential or entry DN is
// ever written to the audit trail or a JSON response.
package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/yichenfchai/AegisFlow/backend/internal/ldap"
	"github.com/yichenfchai/AegisFlow/backend/internal/tenants"
)

// LDAPDirectory is the external directory surface the login flow needs.
// *ldap.Client implements it; tests may substitute a stub. Keeping it an
// interface mirrors the project rule that business modules do not depend on Gin
// or on concrete transport wiring.
type LDAPDirectory interface {
	// Authenticate proves username/password against the directory and returns
	// the canonical identity; failures use the ldap package sentinels.
	Authenticate(ctx context.Context, username, password string) (*ldap.Identity, error)
	Label() string
}

// WithLDAPDirectory enables directory authentication for this service. The
// zero value (no directory) keeps the feature completely disabled, which is the
// documented behavior for a deployment without AEGIS_LDAP_URL. autoProvision
// only ever creates member accounts in an explicit or default tenant.
func (s *Service) WithLDAPDirectory(directory LDAPDirectory, autoProvision bool) *Service {
	s.ldapDirectory = directory
	s.ldapAutoProvision = autoProvision
	return s
}

// LDAPEnabled reports whether the directory login feature is configured. The
// HTTP layer uses it to answer the public config probe and to keep the login
// endpoint closed with 503 when unconfigured.
func (s *Service) LDAPEnabled() bool { return s.ldapDirectory != nil }

// LDAPLabel is the login-page entry text; empty when the feature is disabled.
func (s *Service) LDAPLabel() string {
	if s.ldapDirectory == nil {
		return ""
	}
	return s.ldapDirectory.Label()
}

// LoginLDAP authenticates a user against the configured directory and issues a
// local session for the linked account. The flow is:
//
//  1. the directory proves the password and returns the canonical identity;
//  2. a local account is resolved inside the requested tenant, or across
//     tenants when no tenantSlug was sent (an ambiguous identifier is 400
//     tenant_required, exactly like password login);
//  3. when no account exists and auto-provisioning is enabled, a member account
//     is created in the requested tenant or the deployment's default tenant;
//  4. the session is issued through the shared verified-identity path, with an
//     auth.login audit detail of "method=ldap tenant=<slug>".
//
// The directory password is never stored or compared locally; an existing
// account's local password is unchanged and remains usable through the password
// login endpoint (documented boundary).
func (s *Service) LoginLDAP(ctx context.Context, tenantSlug, username, password, requestID string) (*Tokens, error) {
	if err := s.valid(); err != nil {
		return nil, err
	}
	if s.ldapDirectory == nil {
		return nil, ErrDependencyUnavailable
	}
	if tenantSlug != "" && !tenants.ValidSlug(tenantSlug) {
		return nil, fmt.Errorf("%w: tenantSlug must be 1-32 lowercase letters, digits and inner hyphens", ErrValidation)
	}
	identity, err := s.ldapDirectory.Authenticate(ctx, username, password)
	if err != nil {
		// Wrong password, unknown user, an ambiguous directory match and an
		// unusable entry all collapse into the same generic 401: the client
		// must not learn which case happened.
		if errors.Is(err, ldap.ErrInvalidCredentials) || errors.Is(err, ldap.ErrAmbiguous) || errors.Is(err, ldap.ErrInvalidEntry) {
			return nil, ErrUnauthorized
		}
		return nil, fmt.Errorf("%w: directory authentication failed", ErrDependencyUnavailable)
	}
	directoryUsername := strings.TrimSpace(identity.Username)
	if directoryUsername == "" {
		directoryUsername = strings.TrimSpace(username)
	}
	// The directory email is the trusted linkage source (the local unique index
	// already guarantees per-tenant uniqueness). An unusable email behaves like
	// an absent one: it links no account and never auto-provisions.
	email := ""
	if identity.Email != "" {
		if normalized, emailErr := NormalizeEmail(identity.Email); emailErr == nil {
			email = normalized
		}
	}
	user, err := s.findLDAPLocalUser(ctx, tenantSlug, directoryUsername, email)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if errors.Is(err, ErrNotFound) {
		if !s.ldapAutoProvision {
			return nil, ErrAccountNotFound
		}
		if email == "" {
			return nil, ErrDirectoryEmailMissing
		}
		tenant, tenantErr := s.ldapProvisionTenant(ctx, tenantSlug)
		if tenantErr != nil {
			return nil, tenantErr
		}
		user, err = s.provisionExternalUser(ctx, tenant.ID, directoryUsername, identity.DisplayName, email, requestID, "ldap")
		if err != nil {
			return nil, err
		}
		user.Tenant = tenantInfoOf(tenant.ID, tenant.Slug, tenant.Name)
	}
	if user.Disabled {
		// The directory password was already proven, so reporting the disabled
		// state here does not enable account enumeration.
		return nil, ErrAccountDisabled
	}
	tenantSlugForAudit := ""
	if user.Tenant != nil {
		tenantSlugForAudit = user.Tenant.Slug
	}
	if tenantSlugForAudit == "" {
		tenant, tenantErr := tenants.ByID(ctx, s.db, user.TenantID)
		if tenantErr != nil {
			return nil, tenantErr
		}
		tenantSlugForAudit = tenant.Slug
	}
	return s.loginVerifiedUser(ctx, *user, requestID, "method=ldap tenant="+tenantSlugForAudit)
}

// findLDAPLocalUser resolves the directory identity to one local account. With a
// tenant slug only that tenant is searched; without one the lookup spans
// tenants and reports ErrTenantRequired when more than one account matches (the
// client must then send a tenant slug, exactly like a password login). The email
// predicate is skipped when the directory returned no usable email.
func (s *Service) findLDAPLocalUser(ctx context.Context, tenantSlug, username, email string) (*User, error) {
	const columns = `u.tenant_id,u.id,u.username,u.display_name,u.role,u.disabled,u.created_at,t.slug,t.name`
	query := `SELECT ` + columns + ` FROM auth_users u JOIN tenants t ON t.id=u.tenant_id
		WHERE lower(u.username)=lower($1) OR ($2<>'' AND lower(u.email)=lower($2))
		ORDER BY u.tenant_id,u.id LIMIT 2`
	args := []any{username, email}
	if tenantSlug != "" {
		query = `SELECT ` + columns + ` FROM auth_users u JOIN tenants t ON t.id=u.tenant_id
			WHERE t.slug=$3 AND (lower(u.username)=lower($1) OR ($2<>'' AND lower(u.email)=lower($2)))
			ORDER BY u.tenant_id,u.id LIMIT 2`
		args = append(args, tenantSlug)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	users := make([]User, 0, 2)
	for rows.Next() {
		var user User
		var tenantSlug, tenantName string
		var disabled int
		if err = rows.Scan(&user.TenantID, &user.ID, &user.Username, &user.DisplayName, &user.Role, &disabled, &user.CreatedAt, &tenantSlug, &tenantName); err != nil {
			return nil, err
		}
		user.Disabled = disabled != 0
		user.Tenant = tenantInfoOf(user.TenantID, tenantSlug, tenantName)
		users = append(users, user)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	switch len(users) {
	case 0:
		return nil, ErrNotFound
	case 1:
		return &users[0], nil
	default:
		if tenantSlug == "" {
			return nil, ErrTenantRequired
		}
		// The directory identity matched two different local accounts inside one
		// tenant; refuse instead of guessing (for example an email and a
		// username that belong to different people).
		return nil, fmt.Errorf("%w: multiple local accounts match the directory identity", ErrAccountNotFound)
	}
}

// ldapProvisionTenant selects where an auto-provisioned account is created: the
// explicitly requested tenant, or the deployment's default tenant when the login
// carried no tenantSlug. A suspended tenant cannot receive a new account.
func (s *Service) ldapProvisionTenant(ctx context.Context, tenantSlug string) (*tenants.Tenant, error) {
	if tenantSlug == "" {
		tenant, err := s.DefaultTenant(ctx)
		if err != nil {
			if errors.Is(err, tenants.ErrNotFound) {
				return nil, fmt.Errorf("%w: default tenant is unavailable", ErrAccountNotFound)
			}
			return nil, err
		}
		if tenant.Status != tenants.StatusActive {
			return nil, ErrAccountDisabled
		}
		return tenant, nil
	}
	tenant, err := s.TenantBySlug(ctx, tenantSlug)
	if err != nil {
		if errors.Is(err, tenants.ErrNotFound) {
			// An unknown tenant must not be distinguishable from "no account":
			// both answer account_not_found and nothing is created.
			return nil, ErrAccountNotFound
		}
		if errors.Is(err, tenants.ErrInvalidSlug) {
			return nil, fmt.Errorf("%w: tenantSlug", ErrValidation)
		}
		return nil, err
	}
	if tenant.Status != tenants.StatusActive {
		return nil, ErrAccountDisabled
	}
	return tenant, nil
}
