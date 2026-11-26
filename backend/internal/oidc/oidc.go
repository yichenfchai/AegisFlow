// Package oidc implements the OIDC single sign-on relying party flow (P5
// remaining modules, batch D): authorization code + PKCE (S256), a one-time
// database-backed login state, strict ID-token validation against the provider
// JWKS, verified-email account linkage and the member-only auto-provision
// policy. It does not depend on Gin: the HTTP layer owns cookies, redirects and
// the shared login rate limiter.
package oidc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
)

// StateTTL bounds how long one authorization request stays valid. It matches
// the Max-Age of the aegis_oidc_state cookie; an expired state row is treated
// exactly like a consumed one (state_expired) so a stale tab cannot log in.
const StateTTL = 10 * time.Minute

// ErrorCode is the fixed set of browser-facing callback failure codes. The
// login page maps them to Chinese text; provider error bodies, tokens and
// claims never leave the server.
type ErrorCode string

const (
	CodeStateExpired        ErrorCode = "state_expired"
	CodeTokenExchangeFailed ErrorCode = "token_exchange_failed"
	CodeInvalidToken        ErrorCode = "invalid_token"
	CodeEmailUnverified     ErrorCode = "email_unverified"
	CodeAccountNotFound     ErrorCode = "account_not_found"
	CodeAccountDisabled     ErrorCode = "account_disabled"
	CodeServerError         ErrorCode = "server_error"
)

// LoginError carries the stable failure code together with an internal cause.
// Error() intentionally exposes only the code: the HTTP layer redirects with it
// and may log the cause, while the browser learns nothing about the provider.
type LoginError struct {
	Code  ErrorCode
	cause error
}

func (e *LoginError) Error() string { return string(e.Code) }
func (e *LoginError) Unwrap() error { return e.cause }

func fail(code ErrorCode, cause error) error { return &LoginError{Code: code, cause: cause} }

// CodeOf extracts the browser-facing code of a failed callback. Any unknown
// failure maps to server_error, so an internal error can never leak details.
func CodeOf(err error) ErrorCode {
	var loginErr *LoginError
	if errors.As(err, &loginErr) {
		return loginErr.Code
	}
	return CodeServerError
}

// ValidErrorCode reports whether code belongs to the documented failure set.
func ValidErrorCode(code ErrorCode) bool {
	switch code {
	case CodeStateExpired, CodeTokenExchangeFailed, CodeInvalidToken,
		CodeEmailUnverified, CodeAccountNotFound, CodeAccountDisabled, CodeServerError:
		return true
	}
	return false
}

// Accounts is the local account surface the flow needs. *auth.Service
// implements it; tests may substitute a stub. Keeping it an interface mirrors
// the project rule that business modules do not depend on Gin or on concrete
// transport wiring.
type Accounts interface {
	FindUserByEmail(ctx context.Context, email string) (*auth.User, error)
	LoginOIDC(ctx context.Context, user auth.User, requestID string) (*auth.Tokens, error)
	ProvisionOIDCUser(ctx context.Context, preferredUsername, displayName, email, requestID string) (*auth.User, error)
	RecordOIDCLoginFailure(ctx context.Context, requestID, code string, user *auth.User) error
}

// Options is the validated deployment configuration plus test seams.
type Options struct {
	Issuer           string
	ClientID         string
	ClientSecret     string
	RedirectURL      string
	Scopes           []string
	AutoProvision    bool
	Label            string
	DiscoveryTimeout time.Duration
	// HTTPClient overrides the provider HTTP client (tests); nil builds a
	// dedicated client with the discovery timeout and no redirect following.
	HTTPClient *http.Client
	// Logger receives sanitized operational logs; nil uses slog.Default().
	Logger *slog.Logger
	// Now overrides the clock in tests; nil uses time.Now.
	Now func() time.Time
}

// Service ties the provider protocol, the one-time login state and the local
// account operations together. A nil *Service means OIDC is not configured.
type Service struct {
	provider      *Provider
	states        *StateStore
	accounts      Accounts
	autoProvision bool
	label         string
	logger        *slog.Logger
	now           func() time.Time
}

// CallbackResult is a successful callback: the issued session for the local
// account and the validated same-origin path to redirect to.
type CallbackResult struct {
	Tokens     *auth.Tokens
	RedirectTo string
}

// New performs provider discovery immediately so a misconfigured or
// unreachable provider fails server startup instead of the first login. The
// database is only used for the one-time login state table.
func New(ctx context.Context, db *sql.DB, accounts Accounts, options Options) (*Service, error) {
	if accounts == nil {
		return nil, errors.New("OIDC account service is required")
	}
	provider, err := newProvider(ctx, options)
	if err != nil {
		return nil, err
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &Service{
		provider:      provider,
		states:        &StateStore{db: db},
		accounts:      accounts,
		autoProvision: options.AutoProvision,
		label:         options.Label,
		logger:        logger,
		now:           now,
	}, nil
}

// Label is the login-page button text; never empty when the service exists.
func (s *Service) Label() string { return s.label }

// BeginLogin creates a one-time state row and returns the provider
// authorization URL plus the browser-visible state value (stored in the
// state cookie). redirectTo must already have passed SafeRedirectTo.
func (s *Service) BeginLogin(ctx context.Context, redirectTo string) (authorizationURL, state string, err error) {
	state, err = RandomToken(32)
	if err != nil {
		return "", "", err
	}
	nonce, err := RandomToken(32)
	if err != nil {
		return "", "", err
	}
	// RFC 7636 code_verifier is 43-128 unreserved characters; 32 random bytes
	// encode to exactly 43 base64url characters.
	verifier, err := RandomToken(32)
	if err != nil {
		return "", "", err
	}
	now := s.now()
	if err = s.states.Create(ctx, StateHash(state), nonce, verifier, SafeRedirectTo(redirectTo), now.Add(StateTTL).Unix(), now.Unix()); err != nil {
		return "", "", err
	}
	return s.provider.AuthorizationURL(state, nonce, PKCEChallenge(verifier)), state, nil
}

// CallbackInput is the raw callback material. CookieState is the value of the
// one-time aegis_oidc_state cookie; it must match the query state exactly.
type CallbackInput struct {
	State       string
	CookieState string
	Code        string
	RequestID   string
}

// Complete validates the callback, exchanges the code, verifies the ID token,
// resolves or provisions the local account and issues the session. Every
// failure is recorded as an audited auth.login failure carrying only its fixed
// code; the returned error is a *LoginError for the HTTP layer.
func (s *Service) Complete(ctx context.Context, input CallbackInput) (result *CallbackResult, err error) {
	var user *auth.User
	defer func() {
		if err == nil {
			return
		}
		s.recordFailure(ctx, input.RequestID, CodeOf(err), user)
	}()
	// Bind the callback to this browser before touching the state table: a
	// state that did not originate from this cookie (login CSRF / fixated
	// callback) is rejected with the same outward code as an expired state.
	if len(input.State) == 0 || len(input.CookieState) == 0 || len(input.State) > 256 || len(input.CookieState) > 256 ||
		subtle.ConstantTimeCompare([]byte(input.State), []byte(input.CookieState)) != 1 {
		return nil, fail(CodeStateExpired, errors.New("oidc state cookie missing or mismatched"))
	}
	state, err := s.states.Consume(ctx, StateHash(input.State), s.now().Unix())
	if err != nil {
		if errors.Is(err, ErrStateNotFound) {
			return nil, fail(CodeStateExpired, err)
		}
		return nil, fail(CodeServerError, err)
	}
	if len(input.Code) == 0 || len(input.Code) > 4096 {
		return nil, fail(CodeTokenExchangeFailed, errors.New("authorization code missing or too long"))
	}
	rawIDToken, err := s.provider.ExchangeCode(ctx, input.Code, state.CodeVerifier)
	if err != nil {
		return nil, fail(CodeTokenExchangeFailed, err)
	}
	identity, err := s.provider.VerifyIDToken(ctx, rawIDToken, state.Nonce)
	if err != nil {
		if errors.Is(err, ErrEmailUnverified) {
			return nil, fail(CodeEmailUnverified, err)
		}
		return nil, fail(CodeInvalidToken, err)
	}
	user, err = s.accounts.FindUserByEmail(ctx, identity.Email)
	if err == nil {
		if user.Disabled {
			return nil, fail(CodeAccountDisabled, errors.New("linked account is disabled"))
		}
		return s.issue(ctx, *user, state.RedirectTo, input.RequestID)
	}
	if !errors.Is(err, auth.ErrNotFound) {
		return nil, fail(CodeServerError, err)
	}
	if !s.autoProvision {
		return nil, fail(CodeAccountNotFound, errors.New("no local account is linked to the verified email"))
	}
	user, err = s.accounts.ProvisionOIDCUser(ctx, identity.PreferredUsername, identity.Name, identity.Email, input.RequestID)
	if err != nil {
		return nil, fail(CodeServerError, err)
	}
	return s.issue(ctx, *user, state.RedirectTo, input.RequestID)
}

// issue opens the session for an already-resolved account. The redirect target
// is re-sanitized defensively: the stored value was validated at start time.
func (s *Service) issue(ctx context.Context, user auth.User, redirectTo, requestID string) (*CallbackResult, error) {
	tokens, err := s.accounts.LoginOIDC(ctx, user, requestID)
	if err != nil {
		if errors.Is(err, auth.ErrAccountDisabled) {
			return nil, fail(CodeAccountDisabled, err)
		}
		return nil, fail(CodeServerError, err)
	}
	return &CallbackResult{Tokens: tokens, RedirectTo: SafeRedirectTo(redirectTo)}, nil
}

// recordFailure writes the audited failure row with a detached, bounded
// context so a cancelled browser request still leaves evidence. Only the fixed
// code is stored; the provider cause stays in the process logs.
func (s *Service) recordFailure(ctx context.Context, requestID string, code ErrorCode, user *auth.User) {
	if !ValidErrorCode(code) {
		code = CodeServerError
	}
	auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := s.accounts.RecordOIDCLoginFailure(auditCtx, requestID, string(code), user); err != nil {
		s.logger.ErrorContext(auditCtx, "OIDC login failure audit unavailable", "requestId", requestID, "code", string(code), "error", err)
	}
}

// RandomToken returns size crypto/rand bytes encoded as unpadded base64url.
func RandomToken(size int) (string, error) {
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// StateHash is the SHA-256 hex digest stored in oidc_login_states. Only the
// digest reaches the database; the raw state stays in the browser cookie.
func StateHash(state string) string {
	sum := sha256.Sum256([]byte(state))
	return hex.EncodeToString(sum[:])
}

// PKCEChallenge computes the S256 code challenge for a verifier.
func PKCEChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// SafeRedirectTo accepts only a same-origin relative path. Everything else
// falls back to "/": absolute URLs, scheme-relative "//host" forms,
// backslashes (browsers resolve "/\host" like "//host"), control characters,
// encoded scheme-relative paths and malformed values.
func SafeRedirectTo(raw string) string {
	if raw == "" || len(raw) > 2048 || !utf8.ValidString(raw) || strings.IndexFunc(raw, unicode.IsControl) >= 0 {
		return "/"
	}
	if !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") || strings.Contains(raw, "\\") {
		return "/"
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "" || parsed.Host != "" || parsed.User != nil || !strings.HasPrefix(parsed.Path, "/") {
		return "/"
	}
	decoded, err := url.PathUnescape(parsed.Path)
	if err != nil || strings.HasPrefix(decoded, "//") || strings.Contains(decoded, "\\") {
		return "/"
	}
	return raw
}
