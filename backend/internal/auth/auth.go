// Package auth implements persistent local users and revocable JWT sessions.
// Callers must enforce admin authorization before invoking CreateUser/ListUsers.
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
)

var (
	ErrUnauthorized    = errors.New("unauthorized")
	ErrForbidden       = errors.New("forbidden")
	ErrConflict        = errors.New("conflict")
	ErrValidation      = errors.New("validation failed")
	ErrNotFound        = errors.New("not found")
	ErrAccountDisabled = errors.New("account disabled")
)

const (
	Issuer   = "aegisflow"
	Audience = "aegisflow-api"
)

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{2,63}$`)

// IDs are base64url tokens from randomToken; reject other shapes before queries.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)

func ValidID(id string) bool { return idPattern.MatchString(id) }

// Actor identifies who performed an auditable operation. RequestID links the
// audit row to the structured request log.
type Actor struct {
	ID        string
	Username  string
	RequestID string
}

type User struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	Role        string `json:"role"`
	Disabled    bool   `json:"disabled"`
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
}

type accessClaims struct {
	SessionID string `json:"sid"`
	jwt.RegisteredClaims
}

func New(db *sql.DB, secret string, accessTTL, refreshTTL time.Duration) *Service {
	return &Service{db: db, secret: []byte(secret), accessTTL: accessTTL, refreshTTL: refreshTTL}
}

func (s *Service) valid() error {
	if s.db == nil || len(s.secret) == 0 || s.accessTTL < time.Second || s.refreshTTL < time.Second {
		return fmt.Errorf("%w: database, secret and positive token TTLs are required", ErrValidation)
	}
	return nil
}

// Migrate applies versioned PostgreSQL migrations under a transaction-scoped lock.
// Refresh history is retained so replay can revoke a session.
func (s *Service) Migrate(ctx context.Context) error {
	if err := s.valid(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = advisoryLock(ctx, tx, "migrate"); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
 version BIGINT PRIMARY KEY, applied_at BIGINT NOT NULL)`); err != nil {
		return fmt.Errorf("create migration ledger: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		return err
	}
	var version int64
	for rows.Next() {
		var found int64
		if err = rows.Scan(&found); err != nil {
			rows.Close()
			return err
		}
		if found != version+1 || found > int64(len(authMigrations)) {
			rows.Close()
			return fmt.Errorf("unsupported auth schema migration version %d", found)
		}
		version = found
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for i := int(version); i < len(authMigrations); i++ {
		for _, stmt := range authMigrations[i] {
			if _, err = tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("migrate auth version %d: %w", i+1, err)
			}
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations(version,applied_at) VALUES($1,$2)`, i+1, time.Now().Unix()); err != nil {
			return err
		}
	}
	return tx.Commit()
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

// Bootstrap atomically creates the first user only; existing installations are unchanged.
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
	_, err = tx.ExecContext(ctx, `INSERT INTO auth_users(id,username,password_hash,display_name,role,disabled,created_at)
	 SELECT $1,$2,$3,$4,'admin',0,$5 WHERE NOT EXISTS(SELECT 1 FROM auth_users)`, id, username, string(hash), username, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	return tx.Commit()
}

// CreateUser does not authorize the caller; the HTTP layer must require an admin.
func (s *Service) CreateUser(ctx context.Context, actor Actor, username, password, displayName, role string) (*User, error) {
	if err := s.valid(); err != nil {
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
	result, err := tx.ExecContext(ctx, `INSERT INTO auth_users(id,username,password_hash,display_name,role,disabled,created_at)
	 VALUES($1,$2,$3,$4,$5,0,$6) ON CONFLICT (lower(username)) DO NOTHING`, id, username, string(hash), displayName, role, now)
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
	return &User{ID: id, Username: username, DisplayName: displayName, Role: role, CreatedAt: now}, nil
}

func scanUser(row interface{ Scan(dest ...any) error }) (*User, error) {
	var u User
	var disabled int
	if err := row.Scan(&u.ID, &u.Username, &u.DisplayName, &u.Role, &disabled, &u.CreatedAt); err != nil {
		return nil, err
	}
	u.Disabled = disabled != 0
	return &u, nil
}

const userColumns = `id,username,display_name,role,disabled,created_at`

func (s *Service) ListUsers(ctx context.Context) ([]User, error) {
	if err := s.valid(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+userColumns+` FROM auth_users ORDER BY lower(username),id`)
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

// Login keeps the original Go API for non-HTTP callers; sessions default to non-persistent.
func (s *Service) Login(ctx context.Context, username, password string) (*Tokens, error) {
	return s.LoginWithRemember(ctx, username, password, false, "")
}

func (s *Service) LoginWithRemember(ctx context.Context, username, password string, remember bool, requestID string) (*Tokens, error) {
	if err := s.valid(); err != nil {
		return nil, err
	}
	var u User
	var hash string
	var disabled int
	err := s.db.QueryRowContext(ctx, `SELECT id,username,display_name,role,password_hash,disabled FROM auth_users WHERE lower(username)=lower($1)`, username).Scan(&u.ID, &u.Username, &u.DisplayName, &u.Role, &hash, &disabled)
	if errors.Is(err, sql.ErrNoRows) {
		// A fixed valid bcrypt hash keeps unknown-user failures comparable to bad passwords.
		_ = bcrypt.CompareHashAndPassword([]byte("$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"), []byte(password))
		return nil, ErrUnauthorized
	}
	if err != nil {
		return nil, err
	}
	if len(password) > 72 || bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return nil, ErrUnauthorized
	}
	// Only report the disabled state once the caller proved knowledge of the password.
	if disabled != 0 {
		return nil, ErrAccountDisabled
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
	rememberInt := 0
	if remember {
		rememberInt = 1
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO auth_sessions(id,user_id,expires_at,remember,created_at) VALUES($1,$2,$3,$4,$5)`, sid, u.ID, expires, rememberInt, now); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO auth_refresh_tokens(token_hash,session_id) VALUES($1,$2)`, hashToken(refresh), sid); err != nil {
		return nil, err
	}
	if err = s.writeAudit(ctx, tx, Actor{ID: u.ID, Username: u.Username, RequestID: requestID}, ActionLogin, "user", u.ID, ResultSuccess, ""); err != nil {
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
	var revoked, remember, consumed int
	if err = tx.QueryRowContext(ctx, `SELECT u.id,u.username,u.display_name,u.role,s.expires_at,s.revoked,s.remember
 FROM auth_sessions s JOIN auth_users u ON u.id=s.user_id WHERE s.id=$1`, sid).Scan(&u.ID, &u.Username, &u.DisplayName, &u.Role, &expires, &revoked, &remember); err != nil {
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
	if revoked != 0 || expires <= time.Now().Unix() {
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
// current session when listing or selectively revoking sessions.
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
	if err != nil || !token.Valid || claims.Subject == "" || claims.SessionID == "" {
		return nil, "", ErrUnauthorized
	}
	var u User
	err = s.db.QueryRowContext(ctx, `SELECT u.id,u.username,u.display_name,u.role FROM auth_sessions s
	 JOIN auth_users u ON u.id=s.user_id WHERE s.id=$1 AND s.user_id=$2 AND s.revoked=0 AND s.expires_at>$3 AND u.disabled=0`, claims.SessionID, claims.Subject, time.Now().Unix()).Scan(&u.ID, &u.Username, &u.DisplayName, &u.Role)
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
	claims := accessClaims{SessionID: sid, RegisteredClaims: jwt.RegisteredClaims{Issuer: Issuer, Subject: u.ID, Audience: jwt.ClaimStrings{Audience}, IssuedAt: jwt.NewNumericDate(time.Unix(now, 0)), ExpiresAt: jwt.NewNumericDate(time.Unix(expires, 0))}}
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
