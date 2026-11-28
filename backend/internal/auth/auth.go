// Package auth implements persistent local users and revocable JWT sessions.
// Callers must enforce admin authorization before invoking CreateUser/ListUsers.
// Every user and session belongs to exactly one tenant; the tenant is carried by
// the JWT `tid` claim and re-verified against the database on every request.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/yichenfchai/AegisFlow/backend/internal/tenants"
)

var (
	ErrUnauthorized    = errors.New("unauthorized")
	ErrForbidden       = errors.New("forbidden")
	ErrConflict        = errors.New("conflict")
	ErrValidation      = errors.New("validation failed")
	ErrNotFound        = errors.New("not found")
	ErrAccountDisabled = errors.New("account disabled")
	// ErrTenantRequired reports an ambiguous login: the identifier exists in
	// more than one tenant, so the client must retry with tenantSlug. It is a
	// client-correctable 400, not an authentication failure.
	ErrTenantRequired = errors.New("tenant required")
)

const (
	Issuer   = "aegisflow"
	Audience = "aegisflow-api"
)

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{2,63}$`)

// IDs are base64url tokens from randomToken; reject other shapes before queries.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)

func ValidID(id string) bool { return idPattern.MatchString(id) }

// Actor identifies who performed an auditable operation. TenantID is the
// caller's tenant and bounds every read/write of this actor; callers must never
// derive it (or Role) from client-supplied input. RequestID links the audit row
// to the structured request log.
type Actor struct {
	ID        string
	TenantID  string
	Username  string
	Role      string
	RequestID string
}

// User is a local account. A user belongs to exactly one tenant: TenantID is
// part of the user's identity but is not part of the public JSON yet (the
// tenant metadata surface is delivered with the frontend batch).
type User struct {
	ID       string `json:"id"`
	TenantID string `json:"-"`
	Username string `json:"username"`
	// DisplayName is the human-facing name.
	DisplayName string `json:"displayName"`
	// Role is the tenant-scoped role. It bounds what the actor may see and do.
	Role string `json:"role"`
	// Disabled accounts cannot log in and have no active sessions.
	Disabled bool `json:"disabled"`
	// Zero means unknown for rows created before the timestamp migration.
	CreatedAt int64 `json:"createdAt"`
}

type Tokens struct {
	User             User   `json:"user"`
	AccessToken      string `json:"accessToken"`
	RefreshToken     string `json:"-"`         // Only the HTTP cookie transport may expose this value.
	ExpiresIn        int64  `json:"expiresIn"` // Seconds until the access token expires.
	Remember         bool   `json:"-"`
	SessionExpiresAt int64  `json:"-"` // Absolute Unix deadline, never extended by rotation.
}

type Service struct {
	db         *sql.DB
	secret     []byte
	accessTTL  time.Duration
	refreshTTL time.Duration
	// defaultTenantSlug is the deployment's default tenant slug
	// (AEGIS_DEFAULT_TENANT_SLUG). It selects where system/identity operations
	// land and is overridden by EnsureDefaultTenant at startup.
	defaultTenantSlug string
	// testHookAfterPasswordCheck is nil in production. Tests use it to pause a
	// login after the password comparison and before the session transaction so
	// the login/revocation race is reproducible deterministically.
	testHookAfterPasswordCheck func()
}

// afterPasswordCheck runs the test-only hook; production services keep it nil.
func (s *Service) afterPasswordCheck() {
	if s.testHookAfterPasswordCheck != nil {
		s.testHookAfterPasswordCheck()
	}
}

type accessClaims struct {
	SessionID string `json:"sid"`
	// TenantID is the tenant the session was opened for. A token without it is
	// treated as unauthenticated; InspectToken also requires the session and
	// user rows to carry the same tenant.
	TenantID string `json:"tid"`
	jwt.RegisteredClaims
}

func New(db *sql.DB, secret string, accessTTL, refreshTTL time.Duration) *Service {
	return &Service{db: db, secret: []byte(secret), accessTTL: accessTTL, refreshTTL: refreshTTL, defaultTenantSlug: tenants.DefaultSlug}
}

// tenantSlug returns the configured default tenant slug.
func (s *Service) tenantSlug() string {
	if s.defaultTenantSlug == "" {
		return tenants.DefaultSlug
	}
	return s.defaultTenantSlug
}

// EnsureDefaultTenant verifies (and, for a renamed seed, performs) the startup
// default-tenant guarantee and remembers the configured slug for later system
// operations. It must run after Migrate and before Bootstrap.
func (s *Service) EnsureDefaultTenant(ctx context.Context, slug string) (*tenants.Tenant, error) {
	if err := s.valid(); err != nil {
		return nil, err
	}
	tenant, err := tenants.EnsureDefault(ctx, s.db, slug)
	if err != nil {
		return nil, err
	}
	s.defaultTenantSlug = slug
	return tenant, nil
}

// DefaultTenant resolves the deployment's default tenant row.
func (s *Service) DefaultTenant(ctx context.Context) (*tenants.Tenant, error) {
	if err := s.valid(); err != nil {
		return nil, err
	}
	return tenants.BySlug(ctx, s.db, s.tenantSlug())
}

func (s *Service) valid() error {
	if s.db == nil || len(s.secret) == 0 || s.accessTTL < time.Second || s.refreshTTL < time.Second {
		return fmt.Errorf("%w: database, secret and positive token TTLs are required", ErrValidation)
	}
	return nil
}

// requireTenantID validates a tenant scope supplied by an authenticated caller.
// Operations without a tenant fail closed instead of silently falling back to
// another tenant's rows.
func requireTenantID(tenantID string) error {
	if !ValidID(tenantID) {
		return fmt.Errorf("%w: tenant is required", ErrValidation)
	}
	return nil
}

// actorTenantID extracts the tenant scope of an audited actor.
func actorTenantID(actor Actor) (string, error) {
	if err := requireTenantID(actor.TenantID); err != nil {
		return "", err
	}
	return actor.TenantID, nil
}

func validateUser(username, password, displayName, role string) error {
	if !usernamePattern.MatchString(username) {
		return fmt.Errorf("%w: username must be 3-64 ASCII letters, digits, dots, underscores or hyphens and begin with a letter or digit", ErrValidation)
	}
	if err := validatePassword(password); err != nil {
		return err
	}
	if role != "admin" && role != "member" && role != "viewer" {
		return fmt.Errorf("%w: unknown role", ErrValidation)
	}
	if !utf8.ValidString(displayName) || len(displayName) > 256 {
		return fmt.Errorf("%w: display name must be valid UTF-8 and at most 256 bytes", ErrValidation)
	}
	return nil
}

// Bootstrap atomically creates the first user only; existing installations are
// unchanged. The administrator is created inside the deployment's default
// tenant, because no other tenant can exist in phase F1.
func (s *Service) Bootstrap(ctx context.Context, username, password string) error {
	if err := s.valid(); err != nil {
		return err
	}
	if err := validateUser(username, password, username, "admin"); err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	id, err := randomToken(18)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = advisoryLock(ctx, tx, "bootstrap"); err != nil {
		return err
	}
	tenant, err := tenants.BySlug(ctx, tx, s.tenantSlug())
	if err != nil {
		return fmt.Errorf("bootstrap: default tenant %q is unavailable (run migrations and startup tenant check first): %w", s.tenantSlug(), err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO auth_users(id,tenant_id,username,password_hash,display_name,role,disabled,created_at)
	 SELECT $1,$2,$3,$4,$5,'admin',0,$6 WHERE NOT EXISTS(SELECT 1 FROM auth_users)`,
		id, tenant.ID, username, string(hash), username, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	return tx.Commit()
}

// CreateUser does not authorize the caller; the HTTP layer must require an
// admin. The new account always belongs to the actor's tenant.
func (s *Service) CreateUser(ctx context.Context, actor Actor, username, password, displayName, role string) (*User, error) {
	if err := s.valid(); err != nil {
		return nil, err
	}
	tenantID, err := actorTenantID(actor)
	if err != nil {
		return nil, err
	}
	if displayName == "" {
		displayName = username
	}
	if err := validateUser(username, password, displayName, role); err != nil {
		return nil, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	id, err := randomToken(18)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	result, err := tx.ExecContext(ctx, `INSERT INTO auth_users(id,tenant_id,username,password_hash,display_name,role,disabled,created_at)
	 VALUES($1,$2,$3,$4,$5,$6,0,$7) ON CONFLICT (tenant_id, lower(username)) DO NOTHING`,
		id, tenantID, username, string(hash), displayName, role, now)
	if err != nil {
		return nil, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, fmt.Errorf("%w: username already exists", ErrConflict)
	}
	if err = s.writeAudit(ctx, tx, actor, ActionUserCreate, "user", id, ResultSuccess, username); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &User{ID: id, TenantID: tenantID, Username: username, DisplayName: displayName, Role: role, CreatedAt: now}, nil
}

func scanUser(row interface{ Scan(dest ...any) error }) (*User, error) {
	var u User
	var disabled int
	if err := row.Scan(&u.TenantID, &u.ID, &u.Username, &u.DisplayName, &u.Role, &disabled, &u.CreatedAt); err != nil {
		return nil, err
	}
	u.Disabled = disabled != 0
	return &u, nil
}

const userColumns = `tenant_id,id,username,display_name,role,disabled,created_at`

// qualifiedUserColumns is userColumns for queries that join auth_users with
// another table; every column is qualified so shared names (id) stay unambiguous.
const qualifiedUserColumns = `u.tenant_id,u.id,u.username,u.display_name,u.role,u.disabled,u.created_at`

// ListUsers returns the users of one tenant. It exists for non-HTTP callers;
// the admin HTTP route uses ListUsersPage.
func (s *Service) ListUsers(ctx context.Context, tenantID string) ([]User, error) {
	if err := s.valid(); err != nil {
		return nil, err
	}
	if err := requireTenantID(tenantID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+userColumns+` FROM auth_users WHERE tenant_id=$1 ORDER BY lower(username),id`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	users := make([]User, 0)
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, *u)
	}
	return users, rows.Err()
}

// Login keeps the original Go API for non-HTTP callers; sessions default to
// non-persistent and the login identifier must be unique across tenants.
func (s *Service) Login(ctx context.Context, username, password string) (*Tokens, error) {
	return s.LoginWithRemember(ctx, username, password, false, "")
}

// dummyPasswordHash is a valid bcrypt hash of an unrelated password. Comparing
// against it makes unknown-user failures pay the same cost as a bad password.
const dummyPasswordHash = "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"

// comparePassword runs the bcrypt comparison for a login attempt. bcrypt refuses
// inputs longer than 72 bytes without hashing them, so a length check outside
// this helper would make long-password failures measurably cheaper than normal
// ones and leak which usernames exist. The truncation below is only used for the
// comparison: Login still rejects every >72-byte password after it.
func comparePassword(hash, password string) bool {
	if len(password) > 72 {
		password = password[:72]
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// loginCandidate is one row resolved by the login lookup (before the password
// comparison and before the session transaction).
type loginCandidate struct {
	user         User
	hash         string
	disabled     int
	tenantSlug   string
	tenantStatus string
}

// lookupLoginUser resolves the login identifier to a candidate:
//   - with a tenant slug, only that tenant is searched; an unknown tenant and an
//     unknown username are indistinguishable (ErrUnauthorized);
//   - without a slug, the identifier is searched across tenants. Zero matches is
//     ErrUnauthorized, more than one is ErrTenantRequired (the caller must pick
//     the workspace), one match logs in.
func (s *Service) lookupLoginUser(ctx context.Context, tenantSlug, username string) (*loginCandidate, error) {
	const columns = `u.tenant_id,u.id,u.username,u.display_name,u.role,u.password_hash,u.disabled,u.created_at,t.slug,t.status`
	if tenantSlug != "" {
		var c loginCandidate
		err := s.db.QueryRowContext(ctx, `SELECT `+columns+` FROM auth_users u
			JOIN tenants t ON t.id=u.tenant_id
			WHERE t.slug=$1 AND lower(u.username)=lower($2)`, tenantSlug, username).
			Scan(&c.user.TenantID, &c.user.ID, &c.user.Username, &c.user.DisplayName, &c.user.Role, &c.hash, &c.disabled, &c.user.CreatedAt, &c.tenantSlug, &c.tenantStatus)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrUnauthorized
		}
		if err != nil {
			return nil, err
		}
		return &c, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+columns+` FROM auth_users u
		JOIN tenants t ON t.id=u.tenant_id
		WHERE lower(u.username)=lower($1) ORDER BY u.tenant_id,u.id LIMIT 2`, username)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var candidates []loginCandidate
	for rows.Next() {
		var c loginCandidate
		if err = rows.Scan(&c.user.TenantID, &c.user.ID, &c.user.Username, &c.user.DisplayName, &c.user.Role, &c.hash, &c.disabled, &c.user.CreatedAt, &c.tenantSlug, &c.tenantStatus); err != nil {
			return nil, err
		}
		candidates = append(candidates, c)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	switch len(candidates) {
	case 0:
		return nil, ErrUnauthorized
	case 1:
		c := candidates[0]
		return &c, nil
	default:
		// Deliberately no password comparison: the response (400 tenant_required)
		// is chosen before credentials are checked, which is what lets a client
		// re-submit with the tenant slug.
		return nil, ErrTenantRequired
	}
}

// LoginWithRemember keeps the original no-tenant-slug contract for callers that
// are not the HTTP login handler.
func (s *Service) LoginWithRemember(ctx context.Context, username, password string, remember bool, requestID string) (*Tokens, error) {
	return s.LoginWithTenant(ctx, "", username, password, remember, requestID)
}

// LoginWithTenant resolves the tenant, verifies the credentials and opens a
// revocable session bound to that tenant. A non-empty tenantSlug scopes the
// lookup; an empty one accepts only an identifier unique across tenants.
func (s *Service) LoginWithTenant(ctx context.Context, tenantSlug, username, password string, remember bool, requestID string) (*Tokens, error) {
	if err := s.valid(); err != nil {
		return nil, err
	}
	if tenantSlug != "" && !tenants.ValidSlug(tenantSlug) {
		return nil, fmt.Errorf("%w: tenantSlug must be 1-32 lowercase letters, digits and inner hyphens", ErrValidation)
	}
	candidate, err := s.lookupLoginUser(ctx, tenantSlug, username)
	if err != nil {
		if errors.Is(err, ErrUnauthorized) {
			// A fixed valid bcrypt hash keeps unknown-user/unknown-tenant failures
			// comparable to bad passwords.
			comparePassword(dummyPasswordHash, password)
			s.afterPasswordCheck()
		}
		return nil, err
	}
	u := candidate.user
	if len(password) > 72 {
		// bcrypt refuses inputs over 72 bytes without hashing them. Without this
		// branch a long password would skip bcrypt entirely and return measurably
		// faster than an ordinary failed login, leaking which usernames exist.
		// Compare against the fixed dummy hash so the known and unknown user paths
		// pay the same work, then reject the oversized credential.
		comparePassword(dummyPasswordHash, password)
		s.afterPasswordCheck()
		return nil, ErrUnauthorized
	}
	matched := comparePassword(candidate.hash, password)
	s.afterPasswordCheck()
	if !matched {
		return nil, ErrUnauthorized
	}
	// Only report the disabled state once the caller proved knowledge of the password.
	if candidate.disabled != 0 {
		return nil, ErrAccountDisabled
	}
	// A suspended tenant cannot authenticate; report the same bare contract as a
	// bad credential so tenant state is not disclosed without authorization.
	if candidate.tenantStatus != tenants.StatusActive {
		return nil, ErrUnauthorized
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
	tokens, err := s.issue(u, sid, refresh, expires, remember)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Re-check the user and tenant rows under share locks before creating the
	// session. ChangePassword, ResetPassword and UpdateUser update auth_users
	// before they revoke sessions, so their row lock conflicts with this one:
	// either the revocation committed first (and the stale password/disabled/
	// tenant state read above is rejected here) or this login commits first and
	// the revocation revokes the session it just created. Without the lock a
	// login that started before the revocation could insert a session after it,
	// leaving a session alive that the "revoke immediately" contract says must
	// not exist.
	var lockedHash, lockedTenantID, lockedTenantStatus string
	var lockedDisabled int
	err = tx.QueryRowContext(ctx, `SELECT u.password_hash,u.disabled,u.tenant_id,t.status FROM auth_users u
		JOIN tenants t ON t.id=u.tenant_id WHERE u.id=$1 FOR SHARE OF u, t`, u.ID).
		Scan(&lockedHash, &lockedDisabled, &lockedTenantID, &lockedTenantStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUnauthorized
	}
	if err != nil {
		return nil, err
	}
	if lockedHash != candidate.hash {
		// The password changed while bcrypt was running; verify the credential
		// against the committed hash instead of trusting the stale comparison.
		if !comparePassword(lockedHash, password) {
			return nil, ErrUnauthorized
		}
	}
	if lockedDisabled != 0 {
		return nil, ErrAccountDisabled
	}
	if lockedTenantID != u.TenantID || lockedTenantStatus != tenants.StatusActive {
		return nil, ErrUnauthorized
	}
	rememberInt := 0
	if remember {
		rememberInt = 1
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO auth_sessions(id,tenant_id,user_id,expires_at,remember,created_at) VALUES($1,$2,$3,$4,$5,$6)`, sid, u.TenantID, u.ID, expires, rememberInt, now); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO auth_refresh_tokens(token_hash,session_id) VALUES($1,$2)`, hashToken(refresh), sid); err != nil {
		return nil, err
	}
	// The tenant slug is recorded for forensic clarity; it is not a secret.
	detail := "tenant=" + candidate.tenantSlug
	if err = s.writeAudit(ctx, tx, Actor{ID: u.ID, TenantID: u.TenantID, Username: u.Username, RequestID: requestID}, ActionLogin, "user", u.ID, ResultSuccess, detail); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return tokens, nil
}

func (s *Service) Refresh(ctx context.Context, refreshToken string) (*Tokens, error) {
	if err := s.valid(); err != nil {
		return nil, err
	}
	if !validRefresh(refreshToken) {
		return nil, ErrUnauthorized
	}
	hash := hashToken(refreshToken)
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Resolve the immutable session id without locking the token. Every operation
	// then locks session -> token, serializing all generations across processes.
	sid, err := lockRefreshSession(ctx, tx, hash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUnauthorized
	}
	if err != nil {
		return nil, err
	}
	var u User
	var expires int64
	var revoked, remember, consumed, disabled int
	var sessionTenantID, tenantStatus string
	if err = tx.QueryRowContext(ctx, `SELECT u.tenant_id,u.id,u.username,u.display_name,u.role,u.created_at,s.tenant_id,s.expires_at,s.revoked,s.remember,u.disabled,t.status
 FROM auth_sessions s JOIN auth_users u ON u.id=s.user_id JOIN tenants t ON t.id=u.tenant_id WHERE s.id=$1`, sid).
		Scan(&u.TenantID, &u.ID, &u.Username, &u.DisplayName, &u.Role, &u.CreatedAt, &sessionTenantID, &expires, &revoked, &remember, &disabled, &tenantStatus); err != nil {
		return nil, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT consumed FROM auth_refresh_tokens WHERE token_hash=$1 AND session_id=$2 FOR UPDATE`, hash, sid).Scan(&consumed); err != nil {
		return nil, err
	}
	if consumed != 0 {
		// Commit the replay revocation, including when a concurrent rotation won.
		if _, err = tx.ExecContext(ctx, `UPDATE auth_sessions SET revoked=1 WHERE id=$1`, sid); err != nil {
			return nil, err
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		return nil, ErrUnauthorized
	}
	// Disabled accounts must not rotate refresh tokens either: a disable revokes
	// sessions, but checking here also rejects a session that raced the revoke.
	if revoked != 0 || disabled != 0 || expires <= time.Now().Unix() {
		return nil, ErrUnauthorized
	}
	// Tenant isolation: the session row must carry the user's current tenant and
	// the tenant must be active. A mismatch (for example a forged or stale row)
	// or a suspended tenant revokes the session permanently, not just the token.
	if sessionTenantID != u.TenantID || tenantStatus != tenants.StatusActive {
		if _, err = tx.ExecContext(ctx, `UPDATE auth_sessions SET revoked=1 WHERE id=$1`, sid); err != nil {
			return nil, err
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		return nil, ErrUnauthorized
	}
	if _, err = tx.ExecContext(ctx, `UPDATE auth_refresh_tokens SET consumed=1 WHERE token_hash=$1`, hash); err != nil {
		return nil, err
	}
	next, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	tokens, err := s.issue(u, sid, next, expires, remember != 0)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO auth_refresh_tokens(token_hash,session_id) VALUES($1,$2)`, hashToken(next), sid); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return tokens, nil
}

// Logout is idempotent, including for unknown tokens. Historical refresh tokens
// can revoke their session, so logout still works after rotation.
func (s *Service) Logout(ctx context.Context, refreshToken string) error {
	if err := s.valid(); err != nil {
		return err
	}
	if !validRefresh(refreshToken) {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	sid, err := lockRefreshSession(ctx, tx, hashToken(refreshToken))
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE auth_sessions SET revoked=1 WHERE id=$1`, sid); err != nil {
		return err
	}
	return tx.Commit()
}

// Token-to-session associations are immutable and history is never deleted.
// Lock only the session here; locking a joined token first would allow deadlocks
// between an old-token replay, a new-token refresh, and logout.
func lockRefreshSession(ctx context.Context, tx *sql.Tx, hash string) (string, error) {
	var sid string
	if err := tx.QueryRowContext(ctx, `SELECT session_id FROM auth_refresh_tokens WHERE token_hash=$1`, hash).Scan(&sid); err != nil {
		return "", err
	}
	err := tx.QueryRowContext(ctx, `SELECT id FROM auth_sessions WHERE id=$1 FOR UPDATE`, sid).Scan(&sid)
	return sid, err
}

func (s *Service) Authenticate(ctx context.Context, accessToken string) (*User, error) {
	user, _, err := s.InspectToken(ctx, accessToken)
	return user, err
}

// InspectToken additionally returns the session id so callers can identify the
// current session when listing or selectively revoking sessions. The JWT must
// carry a tenant claim, and the session, user and tenant rows must all agree on
// it and be active; anything else is unauthenticated.
func (s *Service) InspectToken(ctx context.Context, accessToken string) (*User, string, error) {
	if err := s.valid(); err != nil {
		return nil, "", err
	}
	claims := &accessClaims{}
	token, err := jwt.ParseWithClaims(accessToken, claims, func(token *jwt.Token) (any, error) {
		if token.Method != jwt.SigningMethodHS256 {
			return nil, ErrUnauthorized
		}
		return s.secret, nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer(Issuer), jwt.WithAudience(Audience), jwt.WithExpirationRequired(), jwt.WithIssuedAt())
	if err != nil || !token.Valid || claims.Subject == "" || claims.SessionID == "" || claims.TenantID == "" {
		return nil, "", ErrUnauthorized
	}
	var u User
	err = s.db.QueryRowContext(ctx, `SELECT u.tenant_id,u.id,u.username,u.display_name,u.role,u.created_at FROM auth_sessions s
	 JOIN auth_users u ON u.id=s.user_id JOIN tenants t ON t.id=u.tenant_id
	 WHERE s.id=$1 AND s.user_id=$2 AND s.tenant_id=$3 AND u.tenant_id=$3
	   AND t.status='active' AND s.revoked=0 AND s.expires_at>$4 AND u.disabled=0`,
		claims.SessionID, claims.Subject, claims.TenantID, time.Now().Unix()).
		Scan(&u.TenantID, &u.ID, &u.Username, &u.DisplayName, &u.Role, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrUnauthorized
	}
	if err != nil {
		return nil, "", err
	}
	return &u, claims.SessionID, nil
}

func (s *Service) issue(u User, sid, refresh string, sessionExpires int64, remember bool) (*Tokens, error) {
	now := time.Now().Unix()
	expires := time.Now().Add(s.accessTTL).Unix()
	if expires > sessionExpires {
		expires = sessionExpires
	}
	if expires <= now {
		return nil, ErrUnauthorized
	}
	claims := accessClaims{
		SessionID: sid,
		TenantID:  u.TenantID,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    Issuer,
			Subject:   u.ID,
			Audience:  jwt.ClaimStrings{Audience},
			IssuedAt:  jwt.NewNumericDate(time.Unix(now, 0)),
			ExpiresAt: jwt.NewNumericDate(time.Unix(expires, 0)),
		},
	}
	access, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.secret)
	if err != nil {
		return nil, err
	}
	return &Tokens{User: u, AccessToken: access, RefreshToken: refresh, ExpiresIn: expires - now, Remember: remember, SessionExpiresAt: sessionExpires}, nil
}

func randomToken(size int) (string, error) {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
func validRefresh(token string) bool {
	if len(token) != 43 || strings.TrimSpace(token) != token {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(token)
	return err == nil && len(b) == 32 && base64.RawURLEncoding.EncodeToString(b) == token
}
