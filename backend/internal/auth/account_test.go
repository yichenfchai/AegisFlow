package auth

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestChangePassword(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	kept := loginTest(t, s)
	other, err := s.Login(ctx, "admin", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	user, sid, err := s.InspectToken(ctx, kept.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	const newPassword = "new-strong-password-42"
	if err = s.ChangePassword(ctx, testActor, user.ID, "wrong-current-password", newPassword, sid); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong current password error = %v", err)
	}
	rotatedOther, err := s.Refresh(ctx, other.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ChangePassword(ctx, testActor, user.ID, testPassword, "short", sid); !errors.Is(err, ErrValidation) {
		t.Fatalf("short new password error = %v", err)
	}
	if err = s.ChangePassword(ctx, testActor, user.ID, testPassword, newPassword, sid); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.InspectToken(ctx, kept.AccessToken); err != nil {
		t.Fatalf("kept session invalidated: %v", err)
	}
	if _, err = s.Refresh(ctx, kept.RefreshToken); err != nil {
		t.Fatalf("kept session cannot refresh: %v", err)
	}
	if _, err = s.Refresh(ctx, rotatedOther.RefreshToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("other session refresh after change = %v", err)
	}
	if _, err = s.Login(ctx, "admin", testPassword); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("old password accepted: %v", err)
	}
	if _, err = s.Login(ctx, "admin", newPassword); err != nil {
		t.Fatalf("new password rejected: %v", err)
	}
	if err = s.ChangePassword(ctx, testActor, "missing-0000000000000001", newPassword, newPassword, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown user error = %v", err)
	}
}

func TestResetPassword(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	admin := loginTest(t, s)
	if _, _, err := s.InspectToken(ctx, admin.AccessToken); err != nil {
		t.Fatal(err)
	}
	created, err := s.CreateUser(ctx, testActor, "reset-member", testPassword, "", "member")
	if err != nil {
		t.Fatal(err)
	}
	memberTokens, err := s.Login(ctx, "reset-member", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	const newPassword = "reset-password-0001"
	if err = s.ResetPassword(ctx, testActor, "missing-0000000000000001", newPassword); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown target error = %v", err)
	}
	if err = s.ResetPassword(ctx, testActor, created.ID, "short"); !errors.Is(err, ErrValidation) {
		t.Fatalf("short password error = %v", err)
	}
	if err = s.ResetPassword(ctx, testActor, created.ID, newPassword); err != nil {
		t.Fatal(err)
	}
	assertUnauthorized(t, s, memberTokens.AccessToken)
	if _, err = s.Refresh(ctx, memberTokens.RefreshToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("member refresh after reset = %v", err)
	}
	if _, err = s.Login(ctx, "reset-member", testPassword); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("old member password accepted: %v", err)
	}
	if _, err = s.Login(ctx, "reset-member", newPassword); err != nil {
		t.Fatalf("reset password rejected: %v", err)
	}
	if _, err = s.Refresh(ctx, admin.RefreshToken); err != nil {
		t.Fatalf("admin session disturbed by member reset: %v", err)
	}
}

func TestDisableEnableAndLastAdmin(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	admin := loginTest(t, s)
	user, _, err := s.InspectToken(ctx, admin.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	created, err := s.CreateUser(ctx, testActor, "disable-member", testPassword, "", "member")
	if err != nil {
		t.Fatal(err)
	}
	memberTokens, err := s.Login(ctx, "disable-member", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetUserDisabled(ctx, testActor, "missing-0000000000000001", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown target error = %v", err)
	}
	if err = s.SetUserDisabled(ctx, testActor, created.ID, true); err != nil {
		t.Fatal(err)
	}
	assertUnauthorized(t, s, memberTokens.AccessToken)
	if _, err = s.Refresh(ctx, memberTokens.RefreshToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("member refresh after disable = %v", err)
	}
	// The disabled state is only revealed with password knowledge.
	if _, err = s.Login(ctx, "disable-member", "wrong-password-xxxx"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong password for disabled user = %v", err)
	}
	if _, err = s.Login(ctx, "disable-member", testPassword); !errors.Is(err, ErrAccountDisabled) {
		t.Fatalf("disabled login error = %v", err)
	}
	// Idempotent repeat keeps success.
	if err = s.SetUserDisabled(ctx, testActor, created.ID, true); err != nil {
		t.Fatal(err)
	}
	if err = s.SetUserDisabled(ctx, testActor, created.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Login(ctx, "disable-member", testPassword); err != nil {
		t.Fatalf("enabled login rejected: %v", err)
	}
	// Last-admin protection on the single bootstrapped admin.
	if err = s.SetUserDisabled(ctx, testActor, user.ID, true); !errors.Is(err, ErrValidation) {
		t.Fatalf("last admin disable error = %v", err)
	}
	if err = s.SetUserRole(ctx, testActor, user.ID, "viewer"); !errors.Is(err, ErrValidation) {
		t.Fatalf("last admin demote error = %v", err)
	}
	if _, err = s.CreateUser(ctx, testActor, "admin-two", testPassword, "", "admin"); err != nil {
		t.Fatal(err)
	}
	if err = s.SetUserRole(ctx, testActor, user.ID, "viewer"); err != nil {
		t.Fatalf("demote with second admin = %v", err)
	}
	demoted, err := s.Authenticate(ctx, admin.AccessToken)
	if err != nil || demoted.Role != "viewer" {
		t.Fatalf("role change not live for existing sessions: %+v %v", demoted, err)
	}
	if err = s.SetUserRole(ctx, testActor, user.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	if err = s.SetUserRole(ctx, testActor, "missing-0000000000000001", "viewer"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown role target = %v", err)
	}
	if err = s.SetUserRole(ctx, testActor, user.ID, "owner"); !errors.Is(err, ErrValidation) {
		t.Fatalf("invalid role accepted: %v", err)
	}
}

func TestSessionsListAndRevoke(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	first := loginTest(t, s)
	second, err := s.Login(ctx, "admin", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	user, sid, err := s.InspectToken(ctx, first.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := s.ListSessions(ctx, user.ID, sid)
	if err != nil || len(sessions) != 2 {
		t.Fatalf("sessions=%+v err=%v", sessions, err)
	}
	current := 0
	otherID := ""
	for _, session := range sessions {
		if session.Current {
			current++
			if session.ID != sid {
				t.Fatal("current session mismatch")
			}
		} else {
			otherID = session.ID
		}
	}
	if current != 1 || otherID == "" {
		t.Fatalf("current marking wrong: %+v", sessions)
	}
	if sessions[0].CreatedAt == 0 {
		t.Fatal("session createdAt missing")
	}
	if err = s.RevokeSession(ctx, testActor, user.ID, otherID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Refresh(ctx, second.RefreshToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("revoked session refresh = %v", err)
	}
	if err = s.RevokeSession(ctx, testActor, user.ID, otherID); err != nil {
		t.Fatalf("idempotent revoke failed: %v", err)
	}
	if err = s.RevokeSession(ctx, testActor, user.ID, "missing-0000000000000001"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown session error = %v", err)
	}
	if sessions, err = s.ListSessions(ctx, user.ID, sid); err != nil || len(sessions) != 1 || !sessions[0].Current {
		t.Fatalf("session list after revoke: %+v %v", sessions, err)
	}
	third, err := s.Login(ctx, "admin", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	revoked, err := s.RevokeOtherSessions(ctx, testActor, user.ID, sid)
	if err != nil || revoked != 1 {
		t.Fatalf("revoke others count=%d err=%v", revoked, err)
	}
	if _, err = s.Refresh(ctx, third.RefreshToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("third session refresh = %v", err)
	}
	if _, err = s.Refresh(ctx, first.RefreshToken); err != nil {
		t.Fatalf("current session refresh after revoke others: %v", err)
	}
}

func TestAuditTrailAndPagination(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	admin := loginTest(t, s)
	user, sid, err := s.InspectToken(ctx, admin.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	actor := Actor{ID: user.ID, Username: user.Username, RequestID: "req-audit"}
	created, err := s.CreateUser(ctx, actor, "audited-member", testPassword, "", "member")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ChangePassword(ctx, actor, user.ID, "wrong-current-12345", "new-password-123456", sid); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
	if err = s.ChangePassword(ctx, actor, user.ID, testPassword, "new-password-123456", sid); err != nil {
		t.Fatal(err)
	}
	if err = s.SetUserDisabled(ctx, actor, created.ID, true); err != nil {
		t.Fatal(err)
	}
	// Idempotent repeat must not add a row.
	if err = s.SetUserDisabled(ctx, actor, created.ID, true); err != nil {
		t.Fatal(err)
	}
	entries, total, err := s.ListAudit(ctx, 50, 0)
	if err != nil || total != 5 || len(entries) != 5 {
		t.Fatalf("audit total=%d len=%d err=%v", total, len(entries), err)
	}
	actions := map[string]int{}
	sawFailure, sawActor := false, false
	for _, entry := range entries {
		actions[entry.Action]++
		if entry.Action == ActionPasswordChange && entry.Result == ResultFailure {
			sawFailure = true
		}
		if entry.ActorUsername == "admin" && entry.RequestID == "req-audit" && entry.Result == ResultSuccess {
			sawActor = true
		}
		if entry.ObjectType == "" || entry.ObjectID == "" {
			t.Fatalf("audit row missing object: %+v", entry)
		}
	}
	if !sawFailure || !sawActor {
		t.Fatalf("failure/actor rows missing: %+v", entries)
	}
	for action, want := range map[string]int{ActionLogin: 1, ActionUserCreate: 1, ActionPasswordChange: 2, ActionUserDisable: 1} {
		if actions[action] != want {
			t.Fatalf("action %s count=%d want=%d", action, actions[action], want)
		}
	}
	if entries, total, err = s.ListAudit(ctx, 2, 0); err != nil || total != 5 || len(entries) != 2 {
		t.Fatalf("audit page 1: %d %d %v", len(entries), total, err)
	}
	if entries, _, err = s.ListAudit(ctx, 2, 4); err != nil || len(entries) != 1 {
		t.Fatalf("audit page 3: %d %v", len(entries), err)
	}
	if _, _, err = s.ListAudit(ctx, 0, 0); !errors.Is(err, ErrValidation) {
		t.Fatalf("limit 0 accepted: %v", err)
	}
	if _, _, err = s.ListAudit(ctx, 101, 0); !errors.Is(err, ErrValidation) {
		t.Fatalf("limit 101 accepted: %v", err)
	}
	users, total, err := s.ListUsersPage(ctx, 50, 0)
	if err != nil || total != 2 || len(users) != 2 {
		t.Fatalf("users page: %d %d %v", len(users), total, err)
	}
	if users[0].CreatedAt == 0 || users[1].CreatedAt == 0 {
		t.Fatalf("user createdAt missing: %+v", users)
	}
	if users, total, err = s.ListUsersPage(ctx, 1, 1); err != nil || total != 2 || len(users) != 1 {
		t.Fatalf("users offset page: %d %d %v", len(users), total, err)
	}
	if _, _, err = s.ListUsersPage(ctx, 10, -1); !errors.Is(err, ErrValidation) {
		t.Fatalf("negative offset accepted: %v", err)
	}
}

func TestCleanupExpired(t *testing.T) {
	s, db := testService(t)
	ctx := context.Background()
	keep := loginTest(t, s)
	expire, err := s.Login(ctx, "admin", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	_, expireSid, err := s.InspectToken(ctx, expire.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE auth_sessions SET expires_at=$1 WHERE id=$2`, time.Now().Add(-time.Minute).Unix(), expireSid); err != nil {
		t.Fatal(err)
	}
	removed, err := s.CleanupExpired(ctx)
	if err != nil || removed != 1 {
		t.Fatalf("cleanup removed=%d err=%v", removed, err)
	}
	var sessions, tokens int
	if err = db.QueryRow(`SELECT COUNT(*) FROM auth_sessions WHERE id=$1`, expireSid).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT COUNT(*) FROM auth_refresh_tokens WHERE session_id=$1`, expireSid).Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 || tokens != 0 {
		t.Fatalf("expired session not fully removed: sessions=%d tokens=%d", sessions, tokens)
	}
	if _, err = s.Refresh(ctx, expire.RefreshToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expired refresh after cleanup = %v", err)
	}
	if _, err = s.Refresh(ctx, keep.RefreshToken); err != nil {
		t.Fatalf("active session lost after cleanup: %v", err)
	}
	if removed, err = s.CleanupExpired(ctx); err != nil || removed != 0 {
		t.Fatalf("second cleanup removed=%d err=%v", removed, err)
	}
}
