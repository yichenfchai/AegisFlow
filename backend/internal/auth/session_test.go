package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yichenfchai/AegisFlow/backend/internal/testutil"
)

func TestRememberAndAbsoluteDeadline(t *testing.T) {
	s, db := testService(t)
	ctx := context.Background()
	for _, remember := range []bool{false, true} {
		first, err := s.LoginWithRemember(ctx, "admin", testPassword, remember)
		if err != nil {
			t.Fatal(err)
		}
		if first.Remember != remember || first.SessionExpiresAt <= time.Now().Unix() {
			t.Fatalf("invalid session metadata: %+v", first)
		}
		deadline := time.Now().Add(30 * time.Second).Unix()
		if _, err = db.Exec(`UPDATE auth_sessions SET expires_at=$1 WHERE id=(SELECT session_id FROM auth_refresh_tokens WHERE token_hash=$2)`, deadline, hashToken(first.RefreshToken)); err != nil {
			t.Fatal(err)
		}
		next, err := s.Refresh(ctx, first.RefreshToken)
		if err != nil {
			t.Fatal(err)
		}
		if next.Remember != remember || next.SessionExpiresAt != deadline || next.ExpiresIn > 30 {
			t.Fatalf("rotation extended/changed session: %+v", next)
		}
		data, err := json.Marshal(next)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"refreshToken", "remember", "SessionExpiresAt", next.RefreshToken} {
			if strings.Contains(string(data), secret) {
				t.Fatalf("internal metadata leaked: %s", data)
			}
		}
	}
}

func TestConcurrentMigrateAndUnknownVersion(t *testing.T) {
	db := testutil.OpenPostgres(t)
	db2 := testutil.ClonePostgres(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := make(chan struct{})
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		s := New([]*sql.DB{db, db2}[i%2], testSecret, time.Minute, time.Hour)
		go func() { <-start; errs <- s.Migrate(ctx) }()
	}
	close(start)
	for i := 0; i < 8; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&count); err != nil || count != len(authMigrations) {
		t.Fatalf("migration count=%d err=%v", count, err)
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations(version,applied_at) VALUES($1,$2)`, len(authMigrations)+1, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if err := New(db2, testSecret, time.Minute, time.Hour).Migrate(ctx); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("unknown version accepted: %v", err)
	}
}

func TestConcurrentRefreshAndLogoutAcrossPools(t *testing.T) {
	s, db := testService(t)
	s2 := New(testutil.ClonePostgres(t, db), testSecret, time.Minute, time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for i := 0; i < 12; i++ {
		first := loginTest(t, s)
		// Logout with a historical token must serialize against a newer generation.
		current, err := s2.Refresh(ctx, first.RefreshToken)
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		type result struct {
			tokens *Tokens
			err    error
		}
		refresh := make(chan result, 1)
		logout := make(chan error, 1)
		go func() { <-start; tok, err := s.Refresh(ctx, current.RefreshToken); refresh <- result{tok, err} }()
		go func() { <-start; logout <- s2.Logout(ctx, first.RefreshToken) }()
		close(start)
		r := <-refresh
		if err := <-logout; err != nil {
			t.Fatal(err)
		}
		if r.err != nil && !errors.Is(r.err, ErrUnauthorized) {
			t.Fatal(r.err)
		}
		assertUnauthorized(t, s, first.AccessToken)
		assertUnauthorized(t, s2, current.AccessToken)
		if r.tokens != nil {
			assertUnauthorized(t, s2, r.tokens.AccessToken)
			if _, err := s2.Refresh(ctx, r.tokens.RefreshToken); !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("logout lost to refresh: %v", err)
			}
		}
	}
}

func TestOldReplayAndCurrentRefreshAcrossPools(t *testing.T) {
	s, db := testService(t)
	s2 := New(testutil.ClonePostgres(t, db), testSecret, time.Minute, time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for i := 0; i < 12; i++ {
		old := loginTest(t, s)
		current, err := s.Refresh(ctx, old.RefreshToken)
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		oldErr := make(chan error, 1)
		type result struct {
			tokens *Tokens
			err    error
		}
		next := make(chan result, 1)
		go func() { <-start; _, err := s.Refresh(ctx, old.RefreshToken); oldErr <- err }()
		go func() { <-start; tokens, err := s2.Refresh(ctx, current.RefreshToken); next <- result{tokens, err} }()
		close(start)
		if err := <-oldErr; !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("replay=%v", err)
		}
		r := <-next
		if r.err != nil && !errors.Is(r.err, ErrUnauthorized) {
			t.Fatal(r.err)
		}
		assertUnauthorized(t, s2, current.AccessToken)
		if r.tokens != nil {
			assertUnauthorized(t, s, r.tokens.AccessToken)
			if _, err := s.Refresh(ctx, r.tokens.RefreshToken); !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("replay revocation lost: %v", err)
			}
		}
	}
}

func TestRefreshEncodingAndUserValidation(t *testing.T) {
	token, err := randomToken(32)
	if err != nil || !validRefresh(token) {
		t.Fatalf("generated token invalid: %v", err)
	}
	for _, invalid := range []string{"", token + "=", " " + token, strings.Repeat("!", 43)} {
		if validRefresh(invalid) {
			t.Fatalf("accepted token %q", invalid)
		}
	}
	for _, name := range []string{"ab", "中文名", "has space", "' OR 1=1 --"} {
		if !errors.Is(validateUser(name, testPassword, "", "admin"), ErrValidation) {
			t.Fatalf("accepted name %q", name)
		}
	}
}
