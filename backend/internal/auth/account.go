// Account management: password lifecycle, disable/role state, session listing
// and revocation, the persistent audit trail, and expired-session cleanup.
// Callers must enforce authorization (admin vs self) before invoking these.
package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Audited actions. Failed logins stay in structured logs only: the login rate
// limiter bounds attempts, and unbounded failure rows would invite table abuse.
const (
	ActionLogin            = "auth.login"
	ActionUserCreate       = "user.create"
	ActionPasswordChange   = "user.password_change"
	ActionPasswordReset    = "user.password_reset"
	ActionUserDisable      = "user.disable"
	ActionUserEnable       = "user.enable"
	ActionRoleChange       = "user.role_change"
	ActionSessionRevoke    = "session.revoke"
	ActionSessionRevokeAll = "session.revoke_all"
)

const (
	ResultSuccess = "success"
	ResultFailure = "failure"
)

type SessionInfo struct {
	ID        string `json:"id"`
	CreatedAt int64  `json:"createdAt"`
	ExpiresAt int64  `json:"expiresAt"`
	Remember  bool   `json:"remember"`
	Current   bool   `json:"current"`
}

type AuditEntry struct {
	ID            string `json:"id"`
	CreatedAt     int64  `json:"createdAt"`
	ActorID       string `json:"actorId"`
	ActorUsername string `json:"actorUsername"`
	Action        string `json:"action"`
	ObjectType    string `json:"objectType"`
	ObjectID      string `json:"objectId"`
	Result        string `json:"result"`
	Detail        string `json:"detail"`
	RequestID     string `json:"requestId"`
}

func validatePassword(password string) error {
	if len(password) < 12 || len(password) > 72 {
		return fmt.Errorf("%w: password must be 12-72 bytes", ErrValidation)
	}
	return nil
}

func (s *Service) writeAudit(ctx context.Context, tx *sql.Tx, actor Actor, action, objectType, objectID, result, detail string) error {
	id, err := randomToken(18)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO auth_audit(id,created_at,actor_id,actor_username,action,object_type,object_id,result,detail,request_id)
	 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, id, time.Now().Unix(), actor.ID, actor.Username, action, objectType, objectID, result, detail, actor.RequestID)
	return err
}

// writeAuditStandalone records an outcome that has no owning transaction,
// such as a rejected password change.
func (s *Service) writeAuditStandalone(ctx context.Context, actor Actor, action, objectType, objectID, result, detail string) error {
	if err := s.valid(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = s.writeAudit(ctx, tx, actor, action, objectType, objectID, result, detail); err != nil {
		return err
	}
	return tx.Commit()
}

// ChangePassword verifies the current password, replaces the hash, and revokes
// every session except keepSessionID (the caller's current session).
func (s *Service) ChangePassword(ctx context.Context, actor Actor, userID, currentPassword, newPassword, keepSessionID string) error {
	if err := s.valid(); err != nil {
		return err
	}
	if !ValidID(userID) || (keepSessionID != "" && !ValidID(keepSessionID)) {
		return fmt.Errorf("%w: invalid user or session id", ErrValidation)
	}
	if err := validatePassword(newPassword); err != nil {
		return err
	}
	var hash string
	err := s.db.QueryRowContext(ctx, `SELECT password_hash FROM auth_users WHERE id=$1`, userID).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if len(currentPassword) > 72 || bcrypt.CompareHashAndPassword([]byte(hash), []byte(currentPassword)) != nil {
		if auditErr := s.writeAuditStandalone(ctx, actor, ActionPasswordChange, "user", userID, ResultFailure, "wrong current password"); auditErr != nil {
			return auditErr
		}
		return ErrUnauthorized
	}
	newHash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE auth_users SET password_hash=$2 WHERE id=$1`, userID, string(newHash)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE auth_sessions SET revoked=1 WHERE user_id=$1 AND revoked=0 AND id<>$2`, userID, keepSessionID); err != nil {
		return err
	}
	if err = s.writeAudit(ctx, tx, actor, ActionPasswordChange, "user", userID, ResultSuccess, ""); err != nil {
		return err
	}
	return tx.Commit()
}

// ResetPassword sets a new password for the target user and revokes all of
// their sessions; the target must authenticate again afterwards.
func (s *Service) ResetPassword(ctx context.Context, actor Actor, targetUserID, newPassword string) error {
	if err := s.valid(); err != nil {
		return err
	}
	if !ValidID(targetUserID) {
		return fmt.Errorf("%w: invalid user id", ErrValidation)
	}
	if err := validatePassword(newPassword); err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE auth_users SET password_hash=$2 WHERE id=$1`, targetUserID, string(hash))
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrNotFound
	}
	if _, err = tx.ExecContext(ctx, `UPDATE auth_sessions SET revoked=1 WHERE user_id=$1 AND revoked=0`, targetUserID); err != nil {
		return err
	}
	if err = s.writeAudit(ctx, tx, actor, ActionPasswordReset, "user", targetUserID, ResultSuccess, ""); err != nil {
		return err
	}
	return tx.Commit()
}

// ensureRemainingAdmin rejects operations that would leave the deployment
// without any enabled administrator. It runs under the "users" advisory lock.
func ensureRemainingAdmin(ctx context.Context, tx *sql.Tx, excludingUserID string) error {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_users WHERE role='admin' AND disabled=0 AND id<>$1`, excludingUserID).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("%w: the last enabled administrator cannot be disabled or demoted", ErrValidation)
	}
	return nil
}

// UpdateUser applies role and/or disabled changes atomically. The "users"
// advisory lock serializes administrator edits, so two concurrent callers can
// never both demote the last remaining administrator.
func (s *Service) UpdateUser(ctx context.Context, actor Actor, targetUserID string, role *string, disabled *bool) error {
	if err := s.valid(); err != nil {
		return err
	}
	if !ValidID(targetUserID) {
		return fmt.Errorf("%w: invalid user id", ErrValidation)
	}
	if role == nil && disabled == nil {
		return fmt.Errorf("%w: role or disabled is required", ErrValidation)
	}
	if role != nil && *role != "admin" && *role != "member" && *role != "viewer" {
		return fmt.Errorf("%w: unknown role", ErrValidation)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = advisoryLock(ctx, tx, "users"); err != nil {
		return err
	}
	var currentRole string
	var currentDisabled int
	err = tx.QueryRowContext(ctx, `SELECT role,disabled FROM auth_users WHERE id=$1`, targetUserID).Scan(&currentRole, &currentDisabled)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	newRole, newDisabled := currentRole, currentDisabled
	if role != nil {
		newRole = *role
	}
	if disabled != nil {
		newDisabled = 0
		if *disabled {
			newDisabled = 1
		}
	}
	roleChanged := newRole != currentRole
	disabledChanged := newDisabled != currentDisabled
	if !roleChanged && !disabledChanged {
		// Idempotent: no write and no duplicate audit row.
		return tx.Commit()
	}
	// Losing the last enabled administrator would lock the deployment out.
	if currentRole == "admin" && currentDisabled == 0 && (newRole != "admin" || newDisabled == 1) {
		if err = ensureRemainingAdmin(ctx, tx, targetUserID); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE auth_users SET role=$2,disabled=$3 WHERE id=$1`, targetUserID, newRole, newDisabled); err != nil {
		return err
	}
	if disabledChanged && newDisabled == 1 {
		if _, err = tx.ExecContext(ctx, `UPDATE auth_sessions SET revoked=1 WHERE user_id=$1 AND revoked=0`, targetUserID); err != nil {
			return err
		}
	}
	if roleChanged {
		if err = s.writeAudit(ctx, tx, actor, ActionRoleChange, "user", targetUserID, ResultSuccess, currentRole+" -> "+newRole); err != nil {
			return err
		}
	}
	if disabledChanged {
		action := ActionUserEnable
		if newDisabled == 1 {
			action = ActionUserDisable
		}
		if err = s.writeAudit(ctx, tx, actor, action, "user", targetUserID, ResultSuccess, ""); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SetUserDisabled is the focused API for callers that only change one field.
func (s *Service) SetUserDisabled(ctx context.Context, actor Actor, targetUserID string, disabled bool) error {
	return s.UpdateUser(ctx, actor, targetUserID, nil, &disabled)
}

// SetUserRole is the focused API for callers that only change the role.
// Roles are read live on every authenticated request, so sessions pick the
// change up immediately without being revoked.
func (s *Service) SetUserRole(ctx context.Context, actor Actor, targetUserID, role string) error {
	return s.UpdateUser(ctx, actor, targetUserID, &role, nil)
}

// ListSessions returns the user's active sessions with the current one marked.
// Token hashes are never exposed.
func (s *Service) ListSessions(ctx context.Context, userID, currentSessionID string) ([]SessionInfo, error) {
	if err := s.valid(); err != nil {
		return nil, err
	}
	if !ValidID(userID) {
		return nil, fmt.Errorf("%w: invalid user id", ErrValidation)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,created_at,expires_at,remember FROM auth_sessions
	 WHERE user_id=$1 AND revoked=0 AND expires_at>$2 ORDER BY created_at DESC,id`, userID, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sessions := make([]SessionInfo, 0)
	for rows.Next() {
		var info SessionInfo
		var remember int
		if err = rows.Scan(&info.ID, &info.CreatedAt, &info.ExpiresAt, &remember); err != nil {
			return nil, err
		}
		info.Remember = remember != 0
		info.Current = info.ID == currentSessionID
		sessions = append(sessions, info)
	}
	return sessions, rows.Err()
}

// RevokeSession revokes one of the user's own sessions; unknown or foreign
// session ids return ErrNotFound.
func (s *Service) RevokeSession(ctx context.Context, actor Actor, userID, sessionID string) error {
	if err := s.valid(); err != nil {
		return err
	}
	if !ValidID(userID) || !ValidID(sessionID) {
		return fmt.Errorf("%w: invalid user or session id", ErrValidation)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE auth_sessions SET revoked=1 WHERE id=$1 AND user_id=$2 AND revoked=0`, sessionID, userID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		if err = s.writeAudit(ctx, tx, actor, ActionSessionRevoke, "session", sessionID, ResultSuccess, ""); err != nil {
			return err
		}
	} else {
		var exists int
		if err = tx.QueryRowContext(ctx, `SELECT 1 FROM auth_sessions WHERE id=$1 AND user_id=$2`, sessionID, userID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		// Already revoked: idempotent success without a duplicate audit row.
	}
	return tx.Commit()
}

// RevokeOtherSessions revokes all of the user's sessions except the current
// one and reports how many were revoked.
func (s *Service) RevokeOtherSessions(ctx context.Context, actor Actor, userID, keepSessionID string) (int, error) {
	if err := s.valid(); err != nil {
		return 0, err
	}
	if !ValidID(userID) || !ValidID(keepSessionID) {
		return 0, fmt.Errorf("%w: invalid user or session id", ErrValidation)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE auth_sessions SET revoked=1 WHERE user_id=$1 AND revoked=0 AND id<>$2`, userID, keepSessionID)
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n > 0 {
		if err = s.writeAudit(ctx, tx, actor, ActionSessionRevokeAll, "user", userID, ResultSuccess, strconv.FormatInt(n, 10)); err != nil {
			return 0, err
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return int(n), nil
}

// PageBounds keeps list endpoints on a single validation rule.
func PageBounds(limit, offset int) error {
	if limit < 1 || limit > 100 || offset < 0 {
		return fmt.Errorf("%w: limit must be 1-100 and offset non-negative", ErrValidation)
	}
	return nil
}

func (s *Service) ListUsersPage(ctx context.Context, limit, offset int) ([]User, int, error) {
	if err := s.valid(); err != nil {
		return nil, 0, err
	}
	if err := PageBounds(limit, offset); err != nil {
		return nil, 0, err
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_users`).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+userColumns+` FROM auth_users ORDER BY lower(username),id LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	users := make([]User, 0)
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, 0, err
		}
		users = append(users, *u)
	}
	return users, total, rows.Err()
}

func (s *Service) ListAudit(ctx context.Context, limit, offset int) ([]AuditEntry, int, error) {
	if err := s.valid(); err != nil {
		return nil, 0, err
	}
	if err := PageBounds(limit, offset); err != nil {
		return nil, 0, err
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_audit`).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,created_at,actor_id,actor_username,action,object_type,object_id,result,detail,request_id
	 FROM auth_audit ORDER BY created_at DESC,id DESC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	entries := make([]AuditEntry, 0)
	for rows.Next() {
		var e AuditEntry
		if err = rows.Scan(&e.ID, &e.CreatedAt, &e.ActorID, &e.ActorUsername, &e.Action, &e.ObjectType, &e.ObjectID, &e.Result, &e.Detail, &e.RequestID); err != nil {
			return nil, 0, err
		}
		entries = append(entries, e)
	}
	return entries, total, rows.Err()
}

// CleanupExpired removes sessions past their absolute deadline together with
// their refresh-token history. Replay detection only needs history while the
// session is still valid, so this is the documented retention boundary.
func (s *Service) CleanupExpired(ctx context.Context) (int64, error) {
	if err := s.valid(); err != nil {
		return 0, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM auth_refresh_tokens WHERE session_id IN (SELECT id FROM auth_sessions WHERE expires_at <= $1)`, time.Now().Unix()); err != nil {
		return 0, err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM auth_sessions WHERE expires_at <= $1`, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, tx.Commit()
}
