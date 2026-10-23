package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yichenfchai/AegisFlow/backend/internal/testutil"
)

func TestSharedSessionsAcrossIndependentPools(t *testing.T) {
	s1, db1 := testService(t)
	db2 := testutil.ClonePostgres(t, db1)
	if db1 == db2 {
		t.Fatal("expected independent SQL pools")
	}
	s2 := New(db2, testSecret, time.Minute, time.Hour)
	ctx := context.Background()
	first := loginTest(t, s1)
	if _, err := s2.Authenticate(ctx, first.AccessToken); err != nil {
		t.Fatalf("instance 2 cannot authenticate instance 1 session: %v", err)
	}
	next, err := s2.Refresh(ctx, first.RefreshToken)
	if err != nil {
		t.Fatalf("instance 2 cannot rotate instance 1 refresh: %v", err)
	}
	if _, err := s1.Authenticate(ctx, next.AccessToken); err != nil {
		t.Fatal(err)
	}
	if _, err := db2.Exec(`UPDATE auth_users SET role='viewer' WHERE id=$1`, first.User.ID); err != nil {
		t.Fatal(err)
	}
	user, err := s1.Authenticate(ctx, next.AccessToken)
	if err != nil || user.Role != "viewer" {
		t.Fatalf("Authenticate did not query live DB: user=%+v err=%v", user, err)
	}
	if err := s1.Logout(ctx, first.RefreshToken); err != nil {
		t.Fatal(err)
	}
	assertUnauthorized(t, s2, next.AccessToken)
	if _, err := s2.Refresh(ctx, next.RefreshToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("instance 2 missed instance 1 logout: %v", err)
	}
}
