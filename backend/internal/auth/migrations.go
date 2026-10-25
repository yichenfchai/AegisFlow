package auth

import (
	"context"
	"database/sql"
)

// Each entry is an immutable schema version, applied atomically with its ledger row.
var authMigrations = [][]string{{
	`CREATE TABLE auth_users (
 id TEXT PRIMARY KEY, username TEXT NOT NULL,
 password_hash TEXT NOT NULL, display_name TEXT NOT NULL,
 role TEXT NOT NULL CHECK(role IN ('admin','member','viewer')))`,
	`CREATE UNIQUE INDEX auth_users_username_lower_idx ON auth_users(lower(username))`,
	`CREATE TABLE auth_sessions (
 id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES auth_users(id),
 expires_at BIGINT NOT NULL,
 revoked INTEGER NOT NULL DEFAULT 0 CHECK(revoked IN (0,1)),
 remember INTEGER NOT NULL DEFAULT 0 CHECK(remember IN (0,1)))`,
	`CREATE TABLE auth_refresh_tokens (
	 token_hash TEXT PRIMARY KEY, session_id TEXT NOT NULL REFERENCES auth_sessions(id),
	 consumed INTEGER NOT NULL DEFAULT 0 CHECK(consumed IN (0,1)))`,
	`CREATE INDEX auth_refresh_session_idx ON auth_refresh_tokens(session_id)`,
}, {
	// Account management: disable flag, row timestamps, and the persistent audit trail.
	`ALTER TABLE auth_users ADD COLUMN disabled INTEGER NOT NULL DEFAULT 0 CHECK(disabled IN (0,1))`,
	`ALTER TABLE auth_users ADD COLUMN created_at BIGINT NOT NULL DEFAULT 0`,
	`ALTER TABLE auth_sessions ADD COLUMN created_at BIGINT NOT NULL DEFAULT 0`,
	`CREATE TABLE auth_audit (
	 id TEXT PRIMARY KEY,
	 created_at BIGINT NOT NULL,
	 actor_id TEXT NOT NULL DEFAULT '',
	 actor_username TEXT NOT NULL DEFAULT '',
	 action TEXT NOT NULL,
	 object_type TEXT NOT NULL,
	 object_id TEXT NOT NULL DEFAULT '',
	 result TEXT NOT NULL CHECK(result IN ('success','failure')),
	 detail TEXT NOT NULL DEFAULT '',
	 request_id TEXT NOT NULL DEFAULT '')`,
	`CREATE INDEX auth_audit_time_idx ON auth_audit(created_at, id)`,
	`CREATE INDEX auth_audit_action_idx ON auth_audit(action, created_at)`,
}}

// Namespacing by database and schema permits independent test/tenant schemas while
// every process targeting the same schema shares the same transaction-scoped lock.
func advisoryLock(ctx context.Context, tx *sql.Tx, operation string) error {
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(current_database() || ':' || current_schema() || ':aegisflow:auth:' || $1, 0))`, operation)
	return err
}
