package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
	"github.com/yichenfchai/AegisFlow/backend/internal/tenants"
)

const (
	// ldapUsernameMaxBytes and ldapPasswordMaxBytes mirror the ldap client
	// bounds; the HTTP layer rejects oversized input before any directory
	// connection is opened.
	ldapUsernameMaxBytes = 256
	ldapPasswordMaxBytes = 1024
)

// ldapConfig is a public probe for the login page. When the feature is
// unconfigured it reports enabled=false with an empty label and never reveals
// the directory URL, base DN or filter.
func (s *Server) ldapConfig(c *gin.Context) {
	if !s.auth.LDAPEnabled() {
		s.respond(c, http.StatusOK, gin.H{"enabled": false, "label": ""})
		return
	}
	s.respond(c, http.StatusOK, gin.H{"enabled": true, "label": s.auth.LDAPLabel()})
}

// ldapLogin authenticates a user against the configured directory. The route
// stays registered when unconfigured but answers 503 (like the OIDC routes), so
// an operator can tell "not configured" from "wrong path". It counts against
// the same shared Redis login quota as password login (fail closed, no local
// fallback). The body is strict JSON: username and password are required,
// tenantSlug is the same optional workspace selector password login accepts.
func (s *Server) ldapLogin(c *gin.Context) {
	if !s.auth.LDAPEnabled() {
		s.fail(c, http.StatusServiceUnavailable, "dependencies_unavailable", "ldap is not configured")
		return
	}
	if !s.loginAttempt(c) {
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
		// TenantSlug is optional: absent resolves the identity across tenants
		// (an ambiguous identity returns 400 tenant_required); null and
		// non-string values are rejected by the strict decoder.
		TenantSlug *string `json:"tenantSlug"`
	}
	if !s.decode(c, &input) {
		return
	}
	username := strings.TrimSpace(input.Username)
	if len(username) == 0 || len(username) > ldapUsernameMaxBytes || !utf8.ValidString(username) ||
		strings.IndexFunc(username, unicode.IsControl) >= 0 {
		s.fail(c, 400, "validation_failed", "username must be 1-256 bytes without control characters")
		return
	}
	if len(input.Password) == 0 || len(input.Password) > ldapPasswordMaxBytes {
		s.fail(c, 400, "validation_failed", "password must be 1-1024 bytes")
		return
	}
	tenantSlug := ""
	if input.TenantSlug != nil {
		if !tenants.ValidSlug(*input.TenantSlug) {
			s.fail(c, 400, "validation_failed", "tenantSlug must be 1-32 lowercase letters, digits and inner hyphens")
			return
		}
		tenantSlug = *input.TenantSlug
	}
	ctx := c.Request.Context()
	tokens, err := s.auth.LoginLDAP(ctx, tenantSlug, username, input.Password, requestID(c))
	// Only the fixed failure category reaches the log: the directory endpoint,
	// directory diagnostics, DN, password and filter value never do.
	s.logger.InfoContext(ctx, "auth.ldap", "requestId", requestID(c), "success", err == nil, "category", ldapFailureCategory(err))
	if err != nil {
		s.authError(c, err)
		return
	}
	s.setRefreshCookie(c, tokens)
	s.respond(c, 200, tokens)
}

// ldapFailureCategory maps a login error to a fixed, log-safe label. An unknown
// error is reported as internal_error rather than formatted into the log.
func ldapFailureCategory(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, auth.ErrUnauthorized):
		return "invalid_credentials"
	case errors.Is(err, auth.ErrTenantRequired):
		return "tenant_required"
	case errors.Is(err, auth.ErrAccountNotFound):
		return "account_not_found"
	case errors.Is(err, auth.ErrAccountDisabled):
		return "account_disabled"
	case errors.Is(err, auth.ErrDirectoryEmailMissing):
		return "directory_email_missing"
	case errors.Is(err, auth.ErrDependencyUnavailable):
		return "directory_unavailable"
	case errors.Is(err, auth.ErrValidation):
		return "validation_failed"
	default:
		return "internal_error"
	}
}
