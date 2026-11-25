package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// Regression tests for the A3 auth fixes: the login/revocation race (P1-03),
// the long-password bcrypt shortcut (P2-01) and the missing createdAt field
// (P2-04) reported in the 2026-11-23 review.

// pauseLoginAfterPasswordCheck installs the test hook so a login blocks after
// its password comparison and before opening the session transaction. It
// returns a channel that closes when the login reaches that point and a release
// function that lets the login continue.
func pauseLoginAfterPasswordCheck(s *Service, entered chan<- struct{}) func() {
	release := make(chan struct{})
	var once sync.Once
	s.testHookAfterPasswordCheck = func() {
		once.Do(func() { close(entered) })
		<-release
	}
	return func() { close(release) }
}

func TestLoginRacingPasswordRevocationCreatesNoSession(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		revoke func(context.Context, *Service, string) error
	}{
		{"reset", func(ctx context.Context, s *Service, userID string) error {
			return s.ResetPassword(ctx, testActor, userID, "reset-password-0001")
		}},
		{"change", func(ctx context.Context, s *Service, userID string) error {
			return s.ChangePassword(ctx, testActor, userID, testPassword, "changed-password-0001", "")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, db := testService(t)
			username := "race-" + tc.name + "-member"
			created, err := s.CreateUser(ctx, testActor, username, testPassword, "", "member")
			if err != nil {
				t.Fatal(err)
			}
			entered := make(chan struct{})
			release := pauseLoginAfterPasswordCheck(s, entered)
			loginErr := make(chan error, 1)
			go func() {
				_, err := s.Login(ctx, username, testPassword)
				loginErr <- err
			}()
			<-entered
			// The revocation commits while the login is suspended after its
			// bcrypt check, which is exactly the window the review identified.
			if err = tc.revoke(ctx, s, created.ID); err != nil {
				t.Fatalf("revocation while login suspended: %v", err)
			}
			release()
			if err = <-loginErr; !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("login with the pre-revocation password error = %v, want unauthorized", err)
			}
			var sessions int
			if err = db.QueryRow(`SELECT COUNT(*) FROM auth_sessions WHERE user_id=$1`, created.ID).Scan(&sessions); err != nil {
				t.Fatal(err)
			}
			if sessions != 0 {
				t.Fatalf("login that raced a password revocation created %d session(s), want 0", sessions)
			}
		})
	}
}

func TestLoginRacingDisableCreatesNoSession(t *testing.T) {
	s, db := testService(t)
	ctx := context.Background()
	const username = "race-disabled-member"
	created, err := s.CreateUser(ctx, testActor, username, testPassword, "", "member")
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := pauseLoginAfterPasswordCheck(s, entered)
	loginErr := make(chan error, 1)
	go func() {
		_, err := s.Login(ctx, username, testPassword)
		loginErr <- err
	}()
	<-entered
	if err = s.SetUserDisabled(ctx, testActor, created.ID, true); err != nil {
		t.Fatalf("disable while login suspended: %v", err)
	}
	release()
	if err = <-loginErr; !errors.Is(err, ErrAccountDisabled) {
		t.Fatalf("login racing disable error = %v, want account disabled", err)
	}
	var sessions int
	if err = db.QueryRow(`SELECT COUNT(*) FROM auth_sessions WHERE user_id=$1`, created.ID).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 {
		t.Fatalf("login that raced a disable created %d session(s), want 0", sessions)
	}
}

func TestRefreshRejectsDisabledUser(t *testing.T) {
	s, db := testService(t)
	ctx := context.Background()
	const username = "refresh-disabled-member"
	created, err := s.CreateUser(ctx, testActor, username, testPassword, "", "member")
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := s.Login(ctx, username, testPassword)
	if err != nil {
		t.Fatal(err)
	}
	// Flip the flag directly so the session row stays unrevoked: refresh must
	// reject a disabled account on its own, not only through the revocation path.
	if _, err = db.Exec(`UPDATE auth_users SET disabled=1 WHERE id=$1`, created.ID); err != nil {
		t.Fatal(err)
	}
	var revoked int
	if err = db.QueryRow(`SELECT revoked FROM auth_sessions WHERE user_id=$1`, created.ID).Scan(&revoked); err != nil {
		t.Fatal(err)
	}
	if revoked != 0 {
		t.Fatal("setup: session unexpectedly revoked")
	}
	if _, err = s.Refresh(ctx, tokens.RefreshToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("refresh for disabled account error = %v, want unauthorized", err)
	}
}

func TestLongPasswordRunsPasswordComparison(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	const longPassword = "0123456789abcdefghijklmnopqrstuvwxyz-0123456789abcdefghijklmnopqrstuvwxyz-0123456789abcdefghijkl"
	if len(longPassword) <= 72 {
		t.Fatalf("test password must exceed bcrypt's 72-byte limit, got %d bytes", len(longPassword))
	}
	comparisons := 0
	s.testHookAfterPasswordCheck = func() { comparisons++ }
	for _, username := range []string{"admin", "no-such-user"} {
		comparisons = 0
		if _, err := s.Login(ctx, username, longPassword); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("Login(%q, long password) error = %v, want unauthorized", username, err)
		}
		if comparisons != 1 {
			t.Fatalf("Login(%q, long password) ran %d password comparison(s), want 1: the >72-byte path must pay the bcrypt cost too", username, comparisons)
		}
	}
}

func TestUserCreatedAtOnLoginRefreshAndInspect(t *testing.T) {
	s, db := testService(t)
	ctx := context.Background()
	const username = "created-at-member"
	created, err := s.CreateUser(ctx, testActor, username, testPassword, "", "member")
	if err != nil {
		t.Fatal(err)
	}
	var want int64
	if err = db.QueryRow(`SELECT created_at FROM auth_users WHERE id=$1`, created.ID).Scan(&want); err != nil {
		t.Fatal(err)
	}
	if want <= 0 {
		t.Fatal("setup: created_at not stored by CreateUser")
	}
	tokens, err := s.Login(ctx, username, testPassword)
	if err != nil {
		t.Fatal(err)
	}
	if tokens.User.CreatedAt != want {
		t.Fatalf("login user.createdAt = %d, want %d", tokens.User.CreatedAt, want)
	}
	next, err := s.Refresh(ctx, tokens.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if next.User.CreatedAt != want {
		t.Fatalf("refresh user.createdAt = %d, want %d", next.User.CreatedAt, want)
	}
	user, sessionID, err := s.InspectToken(ctx, next.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if user.CreatedAt != want {
		t.Fatalf("inspected user.createdAt = %d, want %d (the /auth/me path)", user.CreatedAt, want)
	}
	if sessionID == "" {
		t.Fatal("inspect returned an empty session id")
	}
}

// TestLongPasswordIsNotRejectedByValidation guards the HTTP-visible contract:
// an oversized password is a failed credential (401), never a 400/500.
func TestLongPasswordIsNotRejectedByValidation(t *testing.T) {
	s, _ := testService(t)
	if _, err := s.Login(context.Background(), "admin", strings.Repeat("x", 500)); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("oversized password login error = %v, want unauthorized", err)
	}
}
