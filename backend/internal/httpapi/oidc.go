package httpapi

import (
	"net/http"
	"net/url"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yichenfchai/AegisFlow/backend/internal/oidc"
)

const (
	oidcStateCookieName = "aegis_oidc_state"
	// The state cookie is scoped to the OIDC routes so it is never sent to the
	// password-login endpoints; its lifetime mirrors oidc.StateTTL exactly.
	oidcStateCookiePath   = "/api/v1/auth/oidc"
	oidcStateCookieMaxAge = 600
	// oidcCallbackPath is the one route where a top-level cross-site GET
	// navigation is expected: that is how every OIDC provider returns the
	// browser to the relying party.
	oidcCallbackPath = "/api/v1/auth/oidc/callback"
)

// isOIDCCallbackNavigation is the single documented exception to the global
// Sec-Fetch-Site: cross-site rejection. It only permits a GET to the callback
// path; the request still has to present the one-time state cookie and a
// matching one-time state row before anything happens, so this does not relax
// CSRF protection for cookie-authorized mutations.
func isOIDCCallbackNavigation(request *http.Request) bool {
	return request.Method == http.MethodGet && request.URL.Path == oidcCallbackPath
}

// oidcConfig is a public probe for the login page. When the feature is
// unconfigured it reports enabled=false with an empty label and never reveals
// the issuer, client id or redirect URL.
func (s *Server) oidcConfig(c *gin.Context) {
	if s.oidc == nil {
		s.respond(c, http.StatusOK, gin.H{"enabled": false, "label": ""})
		return
	}
	s.respond(c, http.StatusOK, gin.H{"enabled": true, "label": s.oidc.Label()})
}

// oidcStart begins the authorization-code + PKCE flow. The route shares the
// password-login Redis quota (fail closed) and stays closed while OIDC is
// unconfigured; disabled is documented as 503 rather than 404 so an operator
// can tell "not configured" from "wrong path" without a configuration leak.
func (s *Server) oidcStart(c *gin.Context) {
	if s.oidc == nil {
		s.fail(c, http.StatusServiceUnavailable, "dependencies_unavailable", "single sign-on is not configured")
		return
	}
	if !s.loginAttempt(c) {
		return
	}
	// Only a same-origin relative path is accepted; anything else becomes "/".
	redirectTo := oidc.SafeRedirectTo(c.Query("redirectTo"))
	// OIDC is pinned to the deployment's default tenant in phase F1: the login
	// state records it, and the callback resolves/provisions accounts only
	// inside that tenant. Cross-tenant SSO ambiguity is handled by the later
	// frontend batch that adds an explicit workspace selector.
	tenant, err := s.auth.DefaultTenant(c.Request.Context())
	if err != nil {
		s.logger.ErrorContext(c.Request.Context(), "OIDC start default tenant unavailable", "requestId", requestID(c), "error", err)
		s.fail(c, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	authorizationURL, state, err := s.oidc.BeginLogin(c.Request.Context(), tenant.ID, redirectTo)
	if err != nil {
		s.logger.ErrorContext(c.Request.Context(), "OIDC start failed", "requestId", requestID(c), "error", err)
		s.fail(c, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	s.setOIDCStateCookie(c, state)
	c.Redirect(http.StatusFound, authorizationURL)
}

// oidcCallback completes the flow. Every failure is a 302 to
// /login?sso_error=<fixed code>; the one-time state cookie is always cleared,
// and no provider error body, token or claim reaches the browser or the logs.
// Success sets the same refresh cookie as a password login and redirects to the
// validated in-site path.
func (s *Server) oidcCallback(c *gin.Context) {
	if s.oidc == nil {
		s.fail(c, http.StatusServiceUnavailable, "dependencies_unavailable", "single sign-on is not configured")
		return
	}
	if !s.loginAttempt(c) {
		return
	}
	cookieState := ""
	if cookie, err := c.Request.Cookie(oidcStateCookieName); err == nil {
		cookieState = cookie.Value
	}
	// The login state is single-use server-side, so clearing the cookie is
	// unconditional: a retry must start from /start again.
	s.clearOIDCStateCookie(c)
	result, err := s.oidc.Complete(c.Request.Context(), oidc.CallbackInput{
		State:       c.Query("state"),
		CookieState: cookieState,
		Code:        c.Query("code"),
		RequestID:   requestID(c),
	})
	if err != nil {
		code := oidc.CodeOf(err)
		s.logger.InfoContext(c.Request.Context(), "auth.oidc", "requestId", requestID(c), "success", false, "code", string(code))
		c.Redirect(http.StatusFound, "/login?sso_error="+url.QueryEscape(string(code)))
		return
	}
	s.setRefreshCookie(c, result.Tokens)
	s.logger.InfoContext(c.Request.Context(), "auth.oidc", "requestId", requestID(c), "success", true)
	c.Redirect(http.StatusFound, result.RedirectTo)
}

// setOIDCStateCookie stores the raw state in a one-time HttpOnly cookie. Lax is
// required for the top-level GET navigation back from the provider; the state
// is only useful together with its server-side row.
func (s *Server) setOIDCStateCookie(c *gin.Context, state string) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name: oidcStateCookieName, Value: state, Path: oidcStateCookiePath,
		HttpOnly: true, Secure: s.cookieSecure, SameSite: http.SameSiteLaxMode,
		MaxAge: oidcStateCookieMaxAge,
	})
}

func (s *Server) clearOIDCStateCookie(c *gin.Context) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name: oidcStateCookieName, Value: "", Path: oidcStateCookiePath,
		HttpOnly: true, Secure: s.cookieSecure, SameSite: http.SameSiteLaxMode,
		MaxAge: -1, Expires: time.Unix(1, 0).UTC(),
	})
}
