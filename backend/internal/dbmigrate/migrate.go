// Package dbmigrate owns the single versioned schema ledger shared by every
// module. All DDL lives here so cross-module tables stay on one timeline;
// packages keep only their own advisory locks for runtime serialization.
package dbmigrate

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Each entry is an immutable schema version, applied atomically with its ledger row.
var Migrations = [][]string{{
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
}, {
	// Project registry (P2): global-role authorization only; per-project
	// membership is deliberately deferred. created_by is a denormalized actor
	// snapshot like the audit actor, not a foreign key.
	`CREATE TABLE projects (
	 id TEXT PRIMARY KEY,
	 name TEXT NOT NULL,
	 description TEXT NOT NULL DEFAULT '',
	 repo_url TEXT NOT NULL DEFAULT '',
	 default_branch TEXT NOT NULL DEFAULT '',
	 status TEXT NOT NULL CHECK(status IN ('active','archived')),
	 created_by TEXT NOT NULL DEFAULT '',
	 created_at BIGINT NOT NULL,
	 updated_at BIGINT NOT NULL)`,
	`CREATE UNIQUE INDEX projects_name_lower_idx ON projects(lower(name))`,
	`CREATE INDEX projects_status_name_idx ON projects(status, lower(name))`,
}, {
	// Build pipeline (P3 first slice): per-project build counter and command
	// snapshot, build records with an interrupted terminal state for restart
	// recovery, and log events whose global monotonic id doubles as the
	// client resume cursor. Single-instance worker boundary documented in the
	// builds package; SKIP LOCKED claiming already serializes instances.
	`ALTER TABLE projects ADD COLUMN build_count BIGINT NOT NULL DEFAULT 0`,
	`ALTER TABLE projects ADD COLUMN build_command TEXT NOT NULL DEFAULT ''`,
	`CREATE TABLE builds (
	 id TEXT PRIMARY KEY,
	 project_id TEXT NOT NULL REFERENCES projects(id),
	 build_number BIGINT NOT NULL,
	 status TEXT NOT NULL CHECK(status IN ('queued','running','succeeded','failed','cancelled','interrupted')),
	 branch TEXT NOT NULL DEFAULT '',
	 command TEXT NOT NULL,
	 triggered_by TEXT NOT NULL DEFAULT '',
	 created_at BIGINT NOT NULL,
	 started_at BIGINT NOT NULL DEFAULT 0,
	 finished_at BIGINT NOT NULL DEFAULT 0)`,
	`CREATE UNIQUE INDEX builds_project_number_idx ON builds(project_id, build_number)`,
	`CREATE INDEX builds_created_idx ON builds(created_at DESC, id)`,
	`CREATE TABLE build_log_events (
	 id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	 build_id TEXT NOT NULL REFERENCES builds(id),
	 stream TEXT NOT NULL CHECK(stream IN ('stdout','stderr','system')),
	 message TEXT NOT NULL,
	 created_at BIGINT NOT NULL)`,
	`CREATE INDEX build_log_events_build_idx ON build_log_events(build_id, id)`,
}, {
	// Project visibility and membership (permissions design P1). Existing
	// projects stay public; the creator becomes the first maintainer so an
	// owner keeps access when the default visibility changes later. user_id is
	// a denormalized actor snapshot like created_by and the audit actor — not
	// a foreign key — so membership does not depend on auth row lifecycle
	// (accounts are disabled, never deleted) and the service validates the
	// user explicitly on insert.
	`ALTER TABLE projects ADD COLUMN visibility TEXT NOT NULL DEFAULT 'public' CHECK (visibility IN ('public','private'))`,
	`CREATE TABLE project_members (
	 project_id TEXT NOT NULL REFERENCES projects(id),
	 user_id TEXT NOT NULL,
	 role TEXT NOT NULL CHECK (role IN ('maintainer','viewer')),
	 created_by TEXT NOT NULL DEFAULT '',
	 created_at BIGINT NOT NULL,
	 PRIMARY KEY (project_id, user_id))`,
	`CREATE INDEX project_members_user_idx ON project_members(user_id, project_id)`,
	`INSERT INTO project_members(project_id,user_id,role,created_by,created_at)
	 SELECT id, created_by, 'maintainer', created_by, created_at FROM projects WHERE created_by <> ''`,
}}

// Apply brings the schema to the latest version under a transaction-scoped
// lock. Unknown higher versions or gaps refuse to start.
func Apply(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = Lock(ctx, tx, "migrate"); err != nil {
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
		if found != version+1 || found > int64(len(Migrations)) {
			rows.Close()
			return fmt.Errorf("unsupported schema migration version %d", found)
		}
		version = found
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for i := int(version); i < len(Migrations); i++ {
		for _, stmt := range Migrations[i] {
			if _, err = tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("migrate version %d: %w", i+1, err)
			}
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations(version,applied_at) VALUES($1,$2)`, i+1, time.Now().Unix()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Namespacing by database and schema permits independent test/tenant schemas while
// every process targeting the same schema shares the same transaction-scoped lock.
// The "auth" segment is historical: the ledger once lived there, and changing the
// key would let old and new processes migrate concurrently during rolling deploys.
func Lock(ctx context.Context, tx *sql.Tx, operation string) error {
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(current_database() || ':' || current_schema() || ':aegisflow:auth:' || $1, 0))`, operation)
	return err
}
