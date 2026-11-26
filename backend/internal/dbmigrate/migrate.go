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
}, {
	// Repository snapshot for the controlled executor (P3 ④c): each build keeps
	// the repoUrl it was triggered with, so editing a project later does not
	// rewrite what a historical build says it cloned. Empty means "no
	// repository": the command runs in an empty workspace, the pre-④c behaviour.
	`ALTER TABLE builds ADD COLUMN repo_url TEXT NOT NULL DEFAULT ''`,
}, {
	// Deployment targets (P4 first slice): admin-only HTTP endpoints that P4
	// deploy tasks and approvals will reference. Secrets are encrypted at rest
	// with AES-256-GCM and an AAD bound to the target id; plaintext is never
	// stored or returned. last_check_* records the most recent connectivity
	// probe (HTTP GET, 2xx = healthy) so the UI can show freshness without a
	// separate status table. created_by is a denormalized actor snapshot like
	// projects.created_by and the audit actor, not a foreign key.
	`CREATE TABLE deployment_targets (
	 id TEXT PRIMARY KEY,
	 name TEXT NOT NULL,
	 kind TEXT NOT NULL CHECK(kind IN ('http')),
	 environment TEXT NOT NULL CHECK(environment IN ('development','staging','production')),
	 endpoint TEXT NOT NULL,
	 health_path TEXT NOT NULL DEFAULT '/',
	 secret_cipher BYTEA NOT NULL DEFAULT '',
	 status TEXT NOT NULL CHECK(status IN ('active','disabled')),
	 created_by TEXT NOT NULL DEFAULT '',
	 created_at BIGINT NOT NULL,
	 updated_at BIGINT NOT NULL,
	 last_check_at BIGINT NOT NULL DEFAULT 0,
	 last_check_ok INTEGER NOT NULL DEFAULT 0 CHECK(last_check_ok IN (0,1)),
	 last_check_detail TEXT NOT NULL DEFAULT '')`,
	`CREATE UNIQUE INDEX deployment_targets_name_lower_idx ON deployment_targets(lower(name))`,
	`CREATE INDEX deployment_targets_status_name_idx ON deployment_targets(status, lower(name))`,
}, {
	// Deployment tasks (P4.2, design §2): deployments are new rows per attempt,
	// never mutated history, and snapshot the target name/endpoint/environment
	// at trigger time so a later target rename or edit stays explainable.
	// deployment_number is a per-project monotonic sequence allocated from
	// projects.deploy_count under the project row lock (the build_count
	// pattern). deployment_events is the response/lifecycle timeline: the
	// identity id doubles as the client cursor. approvals is created here, and
	// awaiting_approval/rejected are part of the status CHECK, so P4.3 can add
	// the approval service without another migration; P4.2 always enqueues
	// directly and leaves approval_id empty. deploy_path on the target is the
	// path appended to its endpoint for the deploy POST (default /deploy).
	`ALTER TABLE deployment_targets ADD COLUMN deploy_path TEXT NOT NULL DEFAULT '/deploy'`,
	`ALTER TABLE projects ADD COLUMN deploy_count BIGINT NOT NULL DEFAULT 0`,
	`CREATE TABLE deployments (
	 id TEXT PRIMARY KEY,
	 project_id TEXT NOT NULL REFERENCES projects(id),
	 target_id TEXT NOT NULL REFERENCES deployment_targets(id),
	 target_name TEXT NOT NULL DEFAULT '',
	 target_endpoint TEXT NOT NULL DEFAULT '',
	 target_environment TEXT NOT NULL DEFAULT '',
	 deployment_number BIGINT NOT NULL,
	 status TEXT NOT NULL CHECK(status IN ('queued','awaiting_approval','running','succeeded','failed','cancelled','interrupted','rejected')),
	 version TEXT NOT NULL,
	 build_id TEXT NOT NULL DEFAULT '',
	 rollback_of TEXT NOT NULL DEFAULT '',
	 approval_id TEXT NOT NULL DEFAULT '',
	 triggered_by TEXT NOT NULL DEFAULT '',
	 detail TEXT NOT NULL DEFAULT '',
	 created_at BIGINT NOT NULL,
	 started_at BIGINT NOT NULL DEFAULT 0,
	 finished_at BIGINT NOT NULL DEFAULT 0)`,
	`CREATE UNIQUE INDEX deployments_project_number_idx ON deployments(project_id, deployment_number)`,
	`CREATE INDEX deployments_status_created_idx ON deployments(status, created_at)`,
	`CREATE INDEX deployments_target_created_idx ON deployments(target_id, created_at DESC)`,
	`CREATE TABLE deployment_events (
	 id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	 deployment_id TEXT NOT NULL REFERENCES deployments(id),
	 stream TEXT NOT NULL CHECK(stream IN ('system','response')),
	 message TEXT NOT NULL,
	 created_at BIGINT NOT NULL)`,
	`CREATE INDEX deployment_events_deployment_idx ON deployment_events(deployment_id, id)`,
	`CREATE TABLE approvals (
	 id TEXT PRIMARY KEY,
	 deployment_id TEXT NOT NULL REFERENCES deployments(id),
	 project_id TEXT NOT NULL REFERENCES projects(id),
	 status TEXT NOT NULL CHECK(status IN ('pending','approved','rejected','expired')),
	 requested_by TEXT NOT NULL DEFAULT '',
	 decided_by TEXT NOT NULL DEFAULT '',
	 reason TEXT NOT NULL DEFAULT '',
	 created_at BIGINT NOT NULL,
	 expires_at BIGINT NOT NULL,
	 decided_at BIGINT NOT NULL DEFAULT 0)`,
	`CREATE UNIQUE INDEX approvals_deployment_idx ON approvals(deployment_id)`,
	`CREATE INDEX approvals_pending_idx ON approvals(status, expires_at)`,
}, {
	// Approval webhook notifications (P4.4): a transactional outbox. Approval
	// state changes and the notification row commit together; delivery is
	// at-least-once, retried with backoff, and never affects the approval
	// state. The unique (approval_id,event) key makes repeated idempotent
	// approve/reject/cancel paths unable to enqueue duplicates.
	`CREATE TABLE approval_notifications (
	 id TEXT PRIMARY KEY,
	 approval_id TEXT NOT NULL REFERENCES approvals(id),
	 event TEXT NOT NULL CHECK(event IN ('approval.requested','approval.approved','approval.rejected','approval.expired')),
	 payload JSONB NOT NULL,
	 status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','delivered','failed')),
	 attempts INTEGER NOT NULL DEFAULT 0,
	 next_attempt_at BIGINT NOT NULL,
	 delivered_at BIGINT NOT NULL DEFAULT 0,
	 last_error TEXT NOT NULL DEFAULT '',
	 created_at BIGINT NOT NULL,
	 UNIQUE (approval_id, event))`,
	`CREATE INDEX approval_notifications_pending_idx ON approval_notifications(status, next_attempt_at, id)`,
}, {
	// Revocation and retention query indexes (review P2-03). Sessions live for
	// the refresh TTL (30 days) and are scanned by user on every session list,
	// revoke-all, disable and cleanup; the audit trail is retained for years.
	// Build history filters by status, so builds(status,created_at,id) serves the
	// list page the same way builds_created_idx already serves the global one.
	// IF NOT EXISTS keeps the migration harmless if an operator pre-created a
	// replacement index with the same purpose under the expected name.
	`CREATE INDEX IF NOT EXISTS auth_sessions_user_idx ON auth_sessions(user_id)`,
	`CREATE INDEX IF NOT EXISTS auth_sessions_expires_idx ON auth_sessions(expires_at)`,
	`CREATE INDEX IF NOT EXISTS auth_audit_actor_idx ON auth_audit(actor_id)`,
	`CREATE INDEX IF NOT EXISTS builds_status_created_idx ON builds(status, created_at, id)`,
}, {
	// Review center (P5-A, gate-style): a review is one immutable request record
	// whose status only moves pending -> approved/rejected/cancelled. number is a
	// per-project monotonic sequence allocated from projects.review_count under
	// the project row lock (the build_count/deploy_count pattern), so it survives
	// concurrent creators and stays the human-facing reference for later
	// deployment-gate and external-result wiring. decided_by/decided_at/reason
	// stay NULL until a decision exists, so "not decided yet" is not encoded as a
	// zero timestamp. The project foreign key cascades on delete even though the
	// current API never deletes projects: review rows have no meaning without it.
	`ALTER TABLE projects ADD COLUMN review_count BIGINT NOT NULL DEFAULT 0`,
	`CREATE TABLE reviews (
	 id TEXT PRIMARY KEY,
	 project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	 number BIGINT NOT NULL,
	 subject TEXT NOT NULL,
	 description TEXT NOT NULL DEFAULT '',
	 version TEXT NOT NULL DEFAULT '',
	 external_url TEXT NOT NULL DEFAULT '',
	 status TEXT NOT NULL CHECK(status IN ('pending','approved','rejected','cancelled')),
	 requested_by TEXT NOT NULL DEFAULT '',
	 decided_by TEXT,
	 decided_at BIGINT,
	 reason TEXT,
	 created_at BIGINT NOT NULL,
	 updated_at BIGINT NOT NULL)`,
	`CREATE UNIQUE INDEX reviews_project_number_idx ON reviews(project_id, number)`,
	`CREATE INDEX reviews_project_status_created_idx ON reviews(project_id, status, created_at)`,
	`CREATE INDEX reviews_status_created_idx ON reviews(status, created_at)`,
}, {
	// Deployment review gate (P5-A E2): a deployment snapshots the approved
	// review that gated it. review_id is nullable so the default-off gate and
	// every historical row stay valid; ON DELETE SET NULL preserves deployment
	// history if a review (or its cascading project) is ever removed, and the
	// partial index serves that referential action plus "which deployments used
	// this review" lookups without indexing the common NULL rows.
	`ALTER TABLE deployments ADD COLUMN review_id TEXT REFERENCES reviews(id) ON DELETE SET NULL`,
	`CREATE INDEX deployments_review_idx ON deployments(review_id) WHERE review_id IS NOT NULL`,
}, {
	// Review center external integration (P5-A E4). review_external_results stores
	// one immutable result per signed inbound delivery: delivery_id is globally
	// unique so a retried webhook is idempotent, and the review foreign key
	// cascades on delete like the review itself. review_notifications is the
	// transactional outbox for review events delivered over the shared approval
	// webhook channel; UNIQUE(review_id,event) makes repeated idempotent
	// transitions unable to enqueue duplicates. both tables only ever hold
	// review-scoped data: subject/description/version/externalUrl and the actor
	// ids, never credentials, request headers or webhook secrets.
	`CREATE TABLE review_external_results (
	 id TEXT PRIMARY KEY,
	 review_id TEXT NOT NULL REFERENCES reviews(id) ON DELETE CASCADE,
	 delivery_id TEXT NOT NULL UNIQUE,
	 source TEXT NOT NULL,
	 verdict TEXT NOT NULL CHECK(verdict IN ('passed','failed','warning')),
	 summary TEXT NOT NULL DEFAULT '',
	 url TEXT,
	 received_at BIGINT NOT NULL)`,
	`CREATE INDEX review_external_results_review_idx ON review_external_results(review_id, received_at DESC, id)`,
	`CREATE TABLE review_notifications (
	 id TEXT PRIMARY KEY,
	 review_id TEXT NOT NULL REFERENCES reviews(id) ON DELETE CASCADE,
	 event TEXT NOT NULL CHECK(event IN ('review.requested','review.approved','review.rejected','review.cancelled')),
	 payload JSONB NOT NULL,
	 status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','delivered','failed')),
	 attempts INTEGER NOT NULL DEFAULT 0,
	 next_attempt_at BIGINT NOT NULL,
	 last_error TEXT NOT NULL DEFAULT '',
	 delivered_at BIGINT,
	 created_at BIGINT NOT NULL,
	 UNIQUE (review_id, event))`,
	`CREATE INDEX review_notifications_pending_idx ON review_notifications(status, next_attempt_at, id)`,
}, {
	// Scan registry and defense events (P5 remaining modules, batch A). Both
	// tables are append-only evidence for signed inbound deliveries: the
	// delivery id is globally unique, so a retried webhook returns the stored row
	// instead of duplicating it. scans belongs to one project and cascades on
	// delete (a scan has no meaning without its project); verdict is the only
	// accepted external judgement. security_events is global (defense is not
	// project-scoped): a source reports one event that stays open until an admin
	// acknowledges it, and acknowledged_by/acknowledged_at stay NULL until then,
	// so "not acknowledged yet" is not encoded as a zero timestamp. Neither
	// table stores request headers or signing material.
	`CREATE TABLE scans (
	 id TEXT PRIMARY KEY,
	 project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	 delivery_id TEXT NOT NULL UNIQUE,
	 source TEXT NOT NULL,
	 tool TEXT NOT NULL DEFAULT '',
	 version TEXT NOT NULL DEFAULT '',
	 verdict TEXT NOT NULL CHECK(verdict IN ('passed','failed','warning')),
	 summary TEXT NOT NULL DEFAULT '',
	 report_url TEXT,
	 received_at BIGINT NOT NULL)`,
	`CREATE INDEX scans_project_received_idx ON scans(project_id, received_at DESC, id)`,
	`CREATE INDEX scans_verdict_received_idx ON scans(verdict, received_at DESC)`,
	`CREATE TABLE security_events (
	 id TEXT PRIMARY KEY,
	 delivery_id TEXT NOT NULL UNIQUE,
	 source TEXT NOT NULL,
	 severity TEXT NOT NULL CHECK(severity IN ('low','medium','high','critical')),
	 event_type TEXT NOT NULL,
	 summary TEXT NOT NULL,
	 details_url TEXT,
	 status TEXT NOT NULL DEFAULT 'open' CHECK(status IN ('open','acknowledged')),
	 occurred_at BIGINT NOT NULL,
	 received_at BIGINT NOT NULL,
	 acknowledged_by TEXT,
	 acknowledged_at BIGINT)`,
	`CREATE INDEX security_events_status_occurred_idx ON security_events(status, occurred_at DESC, id)`,
	`CREATE INDEX security_events_severity_occurred_idx ON security_events(severity, occurred_at DESC)`,
}, {
	// Knowledge base and the independent write-only secret store (P5 remaining
	// modules, batch B). knowledge_docs stores plain-text/Markdown documents:
	// the server never renders or executes the body, it only validates and
	// stores it; safe rendering is a client responsibility. The
	// (updated_at DESC, id) index serves the "recently updated first" page
	// without a second sort, and title search uses ILIKE with escaped wildcards
	// (a btree index would not accelerate a leading-wildcard match, so none is
	// created for that). created_by/updated_by are denormalized actor snapshots
	// like every other module, not foreign keys.
	//
	// secrets is the write-only secret store: the plaintext value is encrypted
	// with AES-256-GCM before it ever reaches the database (ciphertext layout
	// [12-byte random nonce][ciphertext+tag], AAD aegisflow:secrets:<id>), so no
	// read path can return it. key_version records the rotation counter of the
	// row (bumped on every value rotation); it is informational, and this batch
	// implements no multi-key migration: a changed AEGIS_SECRET_KEY makes old
	// ciphertexts unreadable, which is acceptable because values are never read
	// back out. name is unique (case-sensitive) and is the human reference.
	`CREATE TABLE knowledge_docs (
	 id TEXT PRIMARY KEY,
	 title TEXT NOT NULL,
	 body TEXT NOT NULL,
	 tags TEXT NOT NULL DEFAULT '',
	 created_by TEXT NOT NULL,
	 updated_by TEXT NOT NULL,
	 created_at BIGINT NOT NULL,
	 updated_at BIGINT NOT NULL)`,
	`CREATE INDEX knowledge_docs_updated_idx ON knowledge_docs(updated_at DESC, id)`,
	`CREATE TABLE secrets (
	 id TEXT PRIMARY KEY,
	 name TEXT NOT NULL UNIQUE,
	 ciphertext BYTEA NOT NULL,
	 key_version INTEGER NOT NULL DEFAULT 1,
	 created_by TEXT NOT NULL,
	 created_at BIGINT NOT NULL,
	 updated_at BIGINT NOT NULL)`,
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
