package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/yichenfchai/AegisFlow/backend/internal/testutil"
)

const testPassword = "correct-horse-battery"
const testSecret = "integration-test-secret-at-least-32-bytes"

var testActor = Actor{ID: "actor-000000000000001", Username: "tester", RequestID: "req-test"}

func testService(t *testing.T) (*Service, *sql.DB) {
	t.Helper()
	db := testutil.OpenPostgres(t)
	s := New(db, testSecret, 15*time.Minute, 24*time.Hour)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Bootstrap(context.Background(), "admin", testPassword); err != nil {
		t.Fatal(err)
	}
	return s, db
}

func loginTest(t *testing.T, s *Service) *Tokens {
	t.Helper()
	tokens, err := s.Login(context.Background(), "admin", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	return tokens
}

func assertUnauthorized(t *testing.T, s *Service, token string) {
	t.Helper()
	if _, err := s.Authenticate(context.Background(), token); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Authenticate error = %v, want unauthorized", err)
	}
}

func TestLoginAndPasswordFailures(t *testing.T) {
	s, db := testService(t)
	ctx := context.Background()
	for _, pair := range [][2]string{{"admin", "wrong-password"}, {"missing", testPassword}, {"admin", strings.Repeat("x", 73)}} {
		if _, err := s.Login(ctx, pair[0], pair[1]); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("Login(%q) error = %v", pair[0], err)
		}
	}
	if _, err := s.Login(ctx, "ADMIN", testPassword); err != nil {
		t.Fatalf("ASCII case-insensitive login failed: %v", err)
	}
	tokens := loginTest(t, s)
	if tokens.ExpiresIn <= 0 || tokens.ExpiresIn > 900 || tokens.User.Role != "admin" || tokens.RefreshToken == "" {
		t.Fatalf("invalid tokens: %+v", tokens)
	}
	user, err := s.Authenticate(ctx, tokens.AccessToken)
	if err != nil || user.ID != tokens.User.ID {
		t.Fatalf("Authenticate = %+v, %v", user, err)
	}
	var passwordHash, storedRefresh string
	if err = db.QueryRow(`SELECT password_hash FROM auth_users`).Scan(&passwordHash); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(passwordHash, "$2") || strings.Contains(passwordHash, testPassword) {
		t.Fatal("password not bcrypt hashed")
	}
	if err = db.QueryRow(`SELECT token_hash FROM auth_refresh_tokens`).Scan(&storedRefresh); err != nil {
		t.Fatal(err)
	}
	if storedRefresh == tokens.RefreshToken || len(storedRefresh) != 64 {
		t.Fatal("refresh token not SHA256 hashed")
	}
	data, err := json.Marshal(tokens)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "password") || strings.Contains(string(data), "refreshToken") || strings.Contains(string(data), tokens.RefreshToken) || !strings.Contains(string(data), `"displayName"`) || !strings.Contains(string(data), `"expiresIn"`) {
		t.Fatalf("invalid JSON shape: %s", data)
	}
}

func TestJWTValidation(t *testing.T) {
	s, _ := testService(t)
	tokens := loginTest(t, s)
	parts := strings.Split(tokens.AccessToken, ".")
	parts[1] = parts[1][:len(parts[1])-1] + "!"
	assertUnauthorized(t, s, strings.Join(parts, "."))
	parsed, err := jwt.ParseWithClaims(tokens.AccessToken, &accessClaims{}, func(*jwt.Token) (any, error) { return []byte(testSecret), nil })
	if err != nil {
		t.Fatal(err)
	}
	original := *(parsed.Claims.(*accessClaims))
	cases := []struct {
		name   string
		method jwt.SigningMethod
		mutate func(*accessClaims)
	}{
		{"wrong issuer", jwt.SigningMethodHS256, func(c *accessClaims) { c.Issuer = "other" }},
		{"missing issuer", jwt.SigningMethodHS256, func(c *accessClaims) { c.Issuer = "" }},
		{"wrong audience", jwt.SigningMethodHS256, func(c *accessClaims) { c.Audience = jwt.ClaimStrings{"other"} }},
		{"missing audience", jwt.SigningMethodHS256, func(c *accessClaims) { c.Audience = nil }},
		{"missing expiration", jwt.SigningMethodHS256, func(c *accessClaims) { c.ExpiresAt = nil }},
		{"expired", jwt.SigningMethodHS256, func(c *accessClaims) { c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Hour)) }},
		{"missing session", jwt.SigningMethodHS256, func(c *accessClaims) { c.SessionID = "" }},
		{"wrong subject", jwt.SigningMethodHS256, func(c *accessClaims) { c.Subject = "another-user" }},
		{"wrong algorithm", jwt.SigningMethodHS384, func(c *accessClaims) {}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := original
			tc.mutate(&c)
			signed, err := jwt.NewWithClaims(tc.method, c).SignedString([]byte(testSecret))
			if err != nil {
				t.Fatal(err)
			}
			assertUnauthorized(t, s, signed)
		})
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, original).SignedString([]byte("wrong-secret"))
	if err != nil {
		t.Fatal(err)
	}
	assertUnauthorized(t, s, signed)
	unsigned, err := jwt.NewWithClaims(jwt.SigningMethodNone, original).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	assertUnauthorized(t, s, unsigned)
}

func TestRefreshRotationReplayRevokesSession(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	first := loginTest(t, s)
	independent := loginTest(t, s)
	second, err := s.Refresh(ctx, first.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if second.RefreshToken == first.RefreshToken {
		t.Fatal("refresh token did not rotate")
	}
	if _, err = s.Authenticate(ctx, second.AccessToken); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Refresh(ctx, first.RefreshToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("replay error = %v", err)
	}
	assertUnauthorized(t, s, first.AccessToken)
	assertUnauthorized(t, s, second.AccessToken)
	if _, err = s.Refresh(ctx, second.RefreshToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("revoked refresh error = %v", err)
	}
	if _, err = s.Authenticate(ctx, independent.AccessToken); err != nil {
		t.Fatalf("another session revoked: %v", err)
	}
}

func TestConcurrentRefreshSingleConsumer(t *testing.T) {
	s, db := testService(t)
	s2 := New(testutil.ClonePostgres(t, db), testSecret, time.Minute, time.Hour)
	ctx := context.Background()
	first := loginTest(t, s)
	start := make(chan struct{})
	results := make(chan *Tokens, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, s := range []*Service{s, s2} {
		wg.Add(1)
		go func(s *Service) {
			defer wg.Done()
			<-start
			tokens, err := s.Refresh(ctx, first.RefreshToken)
			results <- tokens
			errs <- err
		}(s)
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	success, unauthorized := 0, 0
	for err := range errs {
		if err == nil {
			success++
		} else if errors.Is(err, ErrUnauthorized) {
			unauthorized++
		} else {
			t.Fatalf("unexpected refresh error: %v", err)
		}
	}
	if success != 1 || unauthorized != 1 {
		t.Fatalf("success=%d unauthorized=%d", success, unauthorized)
	}
	for tokens := range results {
		if tokens != nil {
			assertUnauthorized(t, s, tokens.AccessToken)
			if _, err := s.Refresh(ctx, tokens.RefreshToken); !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("winner session not revoked by replay: %v", err)
			}
		}
	}
}

func TestLogout(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	first := loginTest(t, s)
	second, err := s.Refresh(ctx, first.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	// Even a historical refresh token identifies the session for logout.
	if err = s.Logout(ctx, first.RefreshToken); err != nil {
		t.Fatal(err)
	}
	assertUnauthorized(t, s, first.AccessToken)
	assertUnauthorized(t, s, second.AccessToken)
	if _, err = s.Refresh(ctx, second.RefreshToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("refresh after logout: %v", err)
	}
	if err = s.Logout(ctx, second.RefreshToken); err != nil {
		t.Fatal(err)
	}
	if err = s.Logout(ctx, "unknown-token"); err != nil {
		t.Fatal(err)
	}
}

func TestPersistence(t *testing.T) {
	ctx := context.Background()
	db := testutil.OpenPostgres(t)
	s := New(db, testSecret, time.Minute, time.Hour)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Bootstrap(ctx, "admin", testPassword); err != nil {
		t.Fatal(err)
	}
	first := loginTest(t, s)
	db2 := testutil.ClonePostgres(t, db)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s2 := New(db2, testSecret, time.Minute, time.Hour)
	if err := s2.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Authenticate(ctx, first.AccessToken); err != nil {
		t.Fatalf("access lost on restart: %v", err)
	}
	second, err := s2.Refresh(ctx, first.RefreshToken)
	if err != nil {
		t.Fatalf("refresh lost on restart: %v", err)
	}
	if err = s2.Logout(ctx, second.RefreshToken); err != nil {
		t.Fatal(err)
	}
	s3 := New(db2, testSecret, time.Minute, time.Hour)
	assertUnauthorized(t, s3, second.AccessToken)
	if _, err = s3.Refresh(ctx, first.RefreshToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("replay history lost: %v", err)
	}
}

func TestUsersValidationAndCurrentRole(t *testing.T) {
	s, db := testService(t)
	ctx := context.Background()
	cases := []struct{ username, password, role string }{
		{"ab", testPassword, "member"}, {"has space", testPassword, "member"}, {"' OR 1=1 --", testPassword, "member"},
		{"valid-user", "short", "member"}, {"valid-user", strings.Repeat("a", 73), "member"},
		{"valid-user", testPassword, "owner"}, {"valid-user", testPassword, "ADMIN"},
	}
	for _, tc := range cases {
		if _, err := s.CreateUser(ctx, testActor, tc.username, tc.password, "", tc.role); !errors.Is(err, ErrValidation) {
			t.Fatalf("CreateUser(%q,%q) error = %v", tc.username, tc.role, err)
		}
	}
	for _, role := range []string{"admin", "member", "viewer"} {
		u, err := s.CreateUser(ctx, testActor, "user-"+role, testPassword, "Display "+role, role)
		if err != nil {
			t.Fatal(err)
		}
		if u.Role != role {
			t.Fatalf("role = %s", u.Role)
		}
	}
	if _, err := s.CreateUser(ctx, testActor, "ADMIN", testPassword, "", "admin"); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate error = %v", err)
	}
	// Length is in bytes, including multibyte passwords.
	if _, err := s.CreateUser(ctx, testActor, "utf8-password", strings.Repeat("密", 25), "", "member"); !errors.Is(err, ErrValidation) {
		t.Fatalf("multibyte password: %v", err)
	}
	if _, err := s.CreateUser(ctx, testActor, "max-password", strings.Repeat("a", 72), "", "viewer"); err != nil {
		t.Fatalf("72 byte password rejected: %v", err)
	}
	tokens := loginTest(t, s)
	if _, err := db.Exec(`UPDATE auth_users SET role='viewer' WHERE id=$1`, tokens.User.ID); err != nil {
		t.Fatal(err)
	}
	user, err := s.Authenticate(ctx, tokens.AccessToken)
	if err != nil || user.Role != "viewer" {
		t.Fatalf("current role = %+v, %v", user, err)
	}
	next, err := s.Refresh(ctx, tokens.RefreshToken)
	if err != nil || next.User.Role != "viewer" {
		t.Fatalf("refresh current role = %+v, %v", next, err)
	}
	users, err := s.ListUsers(ctx)
	if err != nil || len(users) != 5 {
		t.Fatalf("ListUsers count=%d error=%v", len(users), err)
	}
}

func TestBootstrapIsIdempotentAndConcurrent(t *testing.T) {
	ctx := context.Background()
	db := testutil.OpenPostgres(t)
	s := New(db, testSecret, time.Minute, time.Hour)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	s2 := New(testutil.ClonePostgres(t, db), testSecret, time.Minute, time.Hour)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i, name := range []string{"first-admin", "second-admin"} {
		wg.Add(1)
		go func(s *Service, name string) { defer wg.Done(); errs <- s.Bootstrap(ctx, name, testPassword) }([]*Service{s, s2}[i], name)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	users, err := s.ListUsers(ctx)
	if err != nil || len(users) != 1 || users[0].Role != "admin" {
		t.Fatalf("bootstrap users=%+v err=%v", users, err)
	}
	if err = s.Bootstrap(ctx, "third-admin", "another-long-password"); err != nil {
		t.Fatal(err)
	}
	users, err = s.ListUsers(ctx)
	if err != nil || len(users) != 1 {
		t.Fatalf("bootstrap not idempotent: %+v %v", users, err)
	}
	if _, err = s.Login(ctx, users[0].Username, testPassword); err != nil {
		t.Fatalf("bootstrap modified original user: %v", err)
	}
}

func TestSessionExpiryAndInvalidConfig(t *testing.T) {
	s, db := testService(t)
	ctx := context.Background()
	tokens := loginTest(t, s)
	if _, err := db.Exec(`UPDATE auth_sessions SET expires_at=$1`, time.Now().Add(-time.Second).Unix()); err != nil {
		t.Fatal(err)
	}
	assertUnauthorized(t, s, tokens.AccessToken)
	if _, err := s.Refresh(ctx, tokens.RefreshToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expired refresh: %v", err)
	}
	if err := New(db, "", time.Minute, time.Hour).Migrate(ctx); !errors.Is(err, ErrValidation) {
		t.Fatalf("empty secret error=%v", err)
	}
}
