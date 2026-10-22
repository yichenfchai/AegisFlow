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
	ErrUnauthorized = errors.New("unauthorized")
	ErrForbidden    = errors.New("forbidden")
	ErrConflict     = errors.New("conflict")
	ErrValidation   = errors.New("validation failed")
)

const (
	Issuer   = "aegisflow"
	Audience = "aegisflow-api"
)

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{2,63}$`)

type User struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	Role        string `json:"role"`
}

type Tokens struct {
	User         User   `json:"user"`
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresIn    int64  `json:"expiresIn"` // Seconds until the access token expires.
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

// Migrate is idempotent. Refresh history is retained so replay can revoke a session.
func (s *Service) Migrate(ctx context.Context) error {
	if err := s.valid(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS auth_users (
 id TEXT PRIMARY KEY, username TEXT NOT NULL COLLATE NOCASE UNIQUE,
 password_hash TEXT NOT NULL, display_name TEXT NOT NULL,
 role TEXT NOT NULL CHECK(role IN ('admin','member','viewer')))`,
		`CREATE TABLE IF NOT EXISTS auth_sessions (
 id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES auth_users(id),
 expires_at INTEGER NOT NULL, revoked INTEGER NOT NULL DEFAULT 0 CHECK(revoked IN (0,1)))`,
		`CREATE TABLE IF NOT EXISTS auth_refresh_tokens (
 token_hash TEXT PRIMARY KEY, session_id TEXT NOT NULL REFERENCES auth_sessions(id),
 consumed INTEGER NOT NULL DEFAULT 0 CHECK(consumed IN (0,1)))`,
		`CREATE INDEX IF NOT EXISTS auth_refresh_session_idx ON auth_refresh_tokens(session_id)`,
	} {
		if _, err = tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migrate auth: %w", err)
		}
	}
	return tx.Commit()
}

func validateUser(username, password, displayName, role string) error {
	if !usernamePattern.MatchString(username) {
		return fmt.Errorf("%w: username must be 3-64 ASCII letters, digits, dots, underscores or hyphens and begin with a letter or digit", ErrValidation)
	}
	if len(password) < 12 || len(password) > 72 {
		return fmt.Errorf("%w: password must be 12-72 bytes", ErrValidation)
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO auth_users(id,username,password_hash,display_name,role)
 SELECT ?,?,?,?,'admin' WHERE NOT EXISTS(SELECT 1 FROM auth_users)`, id, username, string(hash), username)
	if err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	return tx.Commit()
}

// CreateUser does not authorize the caller; the HTTP layer must require an admin.
func (s *Service) CreateUser(ctx context.Context, username, password, displayName, role string) (*User, error) {
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
	result, err := s.db.ExecContext(ctx, `INSERT INTO auth_users(id,username,password_hash,display_name,role) VALUES(?,?,?,?,?) ON CONFLICT(username) DO NOTHING`, id, username, string(hash), displayName, role)
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
	return &User{ID: id, Username: username, DisplayName: displayName, Role: role}, nil
}

func (s *Service) ListUsers(ctx context.Context) ([]User, error) {
	if err := s.valid(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,username,display_name,role FROM auth_users ORDER BY username COLLATE NOCASE,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	users := make([]User, 0)
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.DisplayName, &u.Role); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

func (s *Service) Login(ctx context.Context, username, password string) (*Tokens, error) {
	if err := s.valid(); err != nil {
		return nil, err
	}
	var u User
	var hash string
	err := s.db.QueryRowContext(ctx, `SELECT id,username,display_name,role,password_hash FROM auth_users WHERE username=?`, username).Scan(&u.ID, &u.Username, &u.DisplayName, &u.Role, &hash)
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
	sid, err := randomToken(18)
	if err != nil {
		return nil, err
	}
	refresh, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	expires := time.Now().Add(s.refreshTTL).Unix()
	tokens, err := s.issue(u, sid, refresh, expires)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO auth_sessions(id,user_id,expires_at) VALUES(?,?,?)`, sid, u.ID, expires); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO auth_refresh_tokens(token_hash,session_id) VALUES(?,?)`, hashToken(refresh), sid); err != nil {
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// The first statement is a write: concurrent consumers serialize before reading.
	result, err := tx.ExecContext(ctx, `UPDATE auth_refresh_tokens SET consumed=1 WHERE token_hash=? AND consumed=0
 AND EXISTS(SELECT 1 FROM auth_sessions s WHERE s.id=auth_refresh_tokens.session_id AND s.revoked=0 AND s.expires_at>?)`, hash, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		// A previously consumed token is evidence of replay, even if a newer token exists.
		_, err = tx.ExecContext(ctx, `UPDATE auth_sessions SET revoked=1 WHERE id IN
   (SELECT session_id FROM auth_refresh_tokens WHERE token_hash=? AND consumed=1)`, hash)
		if err != nil {
			return nil, err
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		return nil, ErrUnauthorized
	}
	var u User
	var sid string
	var expires int64
	err = tx.QueryRowContext(ctx, `SELECT u.id,u.username,u.display_name,u.role,s.id,s.expires_at
 FROM auth_refresh_tokens r JOIN auth_sessions s ON s.id=r.session_id JOIN auth_users u ON u.id=s.user_id WHERE r.token_hash=?`, hash).Scan(&u.ID, &u.Username, &u.DisplayName, &u.Role, &sid, &expires)
	if err != nil {
		return nil, err
	}
	next, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	tokens, err := s.issue(u, sid, next, expires)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO auth_refresh_tokens(token_hash,session_id) VALUES(?,?)`, hashToken(next), sid); err != nil {
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
	_, err := s.db.ExecContext(ctx, `UPDATE auth_sessions SET revoked=1 WHERE id IN(SELECT session_id FROM auth_refresh_tokens WHERE token_hash=?)`, hashToken(refreshToken))
	return err
}

func (s *Service) Authenticate(ctx context.Context, accessToken string) (*User, error) {
	if err := s.valid(); err != nil {
		return nil, err
	}
	claims := &accessClaims{}
	token, err := jwt.ParseWithClaims(accessToken, claims, func(token *jwt.Token) (any, error) {
		if token.Method != jwt.SigningMethodHS256 {
			return nil, ErrUnauthorized
		}
		return s.secret, nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer(Issuer), jwt.WithAudience(Audience), jwt.WithExpirationRequired(), jwt.WithIssuedAt())
	if err != nil || !token.Valid || claims.Subject == "" || claims.SessionID == "" {
		return nil, ErrUnauthorized
	}
	var u User
	err = s.db.QueryRowContext(ctx, `SELECT u.id,u.username,u.display_name,u.role FROM auth_sessions s
 JOIN auth_users u ON u.id=s.user_id WHERE s.id=? AND s.user_id=? AND s.revoked=0 AND s.expires_at>?`, claims.SessionID, claims.Subject, time.Now().Unix()).Scan(&u.ID, &u.Username, &u.DisplayName, &u.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUnauthorized
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (s *Service) issue(u User, sid, refresh string, sessionExpires int64) (*Tokens, error) {
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
	return &Tokens{User: u, AccessToken: access, RefreshToken: refresh, ExpiresIn: expires - now}, nil
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
