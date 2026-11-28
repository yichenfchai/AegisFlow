package dbmigrate

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/yichenfchai/AegisFlow/backend/internal/testutil"
)

var createIndexPattern = regexp.MustCompile(`^CREATE (?:UNIQUE )?INDEX (?:IF NOT EXISTS )?([a-z0-9_]+)`)

// TestMigrationVersionsAreContiguousAndUnique keeps the ledger append-only: the
// implicit version of Migrations[i] is i+1, so every version must be present,
// non-empty, and must not redeclare an index name or repeat a statement.
func TestMigrationVersionsAreContiguousAndUnique(t *testing.T) {
	if len(Migrations) == 0 {
		t.Fatal("no schema migrations declared")
	}
	statements := map[string]int{}
	indexes := map[string]int{}
	for i, migration := range Migrations {
		version := i + 1
		if len(migration) == 0 {
			t.Fatalf("schema version %d has no statements", version)
		}
		for _, statement := range migration {
			if strings.TrimSpace(statement) == "" {
				t.Fatalf("schema version %d contains an empty statement", version)
			}
			if previous, ok := statements[statement]; ok {
				t.Fatalf("schema version %d repeats a statement from version %d", version, previous)
			}
			statements[statement] = version
			match := createIndexPattern.FindStringSubmatch(statement)
			if match == nil {
				continue
			}
			if previous, ok := indexes[match[1]]; ok {
				t.Fatalf("index %q declared in schema versions %d and %d", match[1], previous, version)
			}
			indexes[match[1]] = version
		}
	}
}

// TestApplyIsIdempotent applies every version twice and asserts the ledger stays
// contiguous, unique and complete.
func TestApplyIsIdempotent(t *testing.T) {
	db := testutil.OpenPostgres(t)
	ctx := context.Background()
	for attempt := 0; attempt < 2; attempt++ {
		if err := Apply(ctx, db); err != nil {
			t.Fatalf("apply attempt %d: %v", attempt+1, err)
		}
	}
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var versions []int64
	for rows.Next() {
		var version int64
		if err = rows.Scan(&version); err != nil {
			t.Fatal(err)
		}
		versions = append(versions, version)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(versions) != len(Migrations) {
		t.Fatalf("schema_migrations rows = %d, want %d (one per declared version)", len(versions), len(Migrations))
	}
	for i, version := range versions {
		if version != int64(i+1) {
			t.Fatalf("schema_migrations[%d] = %d, want %d (versions must stay contiguous)", i, version, i+1)
		}
	}
}

// TestAuthAndBuildIndexesApplied verifies the review P2-03 migration: the
// session, audit and build queries need these supporting indexes.
func TestAuthAndBuildIndexesApplied(t *testing.T) {
	db := testutil.OpenPostgres(t)
	ctx := context.Background()
	if err := Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"auth_sessions_user_idx":    "USING btree (user_id)",
		"auth_sessions_expires_idx": "USING btree (expires_at)",
		"auth_audit_actor_idx":      "USING btree (actor_id)",
		"builds_status_created_idx": "USING btree (status, created_at, id)",
	}
	rows, err := db.QueryContext(ctx, `SELECT indexname, indexdef FROM pg_indexes WHERE schemaname = current_schema()`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := map[string]string{}
	for rows.Next() {
		var name, definition string
		if err = rows.Scan(&name, &definition); err != nil {
			t.Fatal(err)
		}
		found[name] = definition
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	for name, columns := range want {
		definition, ok := found[name]
		if !ok {
			t.Errorf("index %s missing after migration", name)
			continue
		}
		if !strings.Contains(definition, columns) {
			t.Errorf("index %s definition %q does not cover %q", name, definition, columns)
		}
	}
}

// seedTenantID is the fixed id of the tenant seeded by schema v18.
const seedTenantID = "tenant_default_0000000000000001"

type columnInfo struct {
	dataType   string
	isNullable string
}

// columnState reads one column's type/nullability from the current schema.
func columnState(t *testing.T, db *sql.DB, table, column string) (columnInfo, bool) {
	t.Helper()
	var info columnInfo
	err := db.QueryRow(`SELECT data_type,is_nullable FROM information_schema.columns
		WHERE table_schema=current_schema() AND table_name=$1 AND column_name=$2`, table, column).
		Scan(&info.dataType, &info.isNullable)
	if errors.Is(err, sql.ErrNoRows) {
		return columnInfo{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return info, true
}

func indexDefinition(t *testing.T, db *sql.DB, name string) (string, bool) {
	t.Helper()
	var definition string
	err := db.QueryRow(`SELECT indexdef FROM pg_indexes WHERE schemaname=current_schema() AND indexname=$1`, name).Scan(&definition)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return definition, true
}

// TestTenantMigrationUpgradesExistingDeployment replays the pre-F1 schema,
// seeds the rows an existing single-tenant deployment would have, applies v18
// and then asserts the designed upgrade behavior: every auth row is backfilled
// with the seeded tenant, the columns become NOT NULL with foreign keys, the
// global username/email uniqueness is replaced by per-tenant uniqueness, and
// the tenant-scoped audit index exists.
func TestTenantMigrationUpgradesExistingDeployment(t *testing.T) {
	db := testutil.OpenPostgres(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE TABLE schema_migrations (version BIGINT PRIMARY KEY, applied_at BIGINT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	// Replay versions 1-17 exactly as an existing deployment has them.
	for i := 0; i < 17; i++ {
		for _, statement := range Migrations[i] {
			if _, err := db.ExecContext(ctx, statement); err != nil {
				t.Fatalf("seed migration %d: %v", i+1, err)
			}
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations(version,applied_at) VALUES($1,$2)`, i+1, time.Now().Unix()); err != nil {
			t.Fatal(err)
		}
	}
	// Legacy rows: one user, one session, one audit entry and one OIDC state.
	const legacyUserID = "legacyuser0000000001"
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,username,password_hash,display_name,role,disabled,created_at,email)
		VALUES($1,'legacy','legacy-hash','Legacy','admin',0,100,'legacy@example.com')`, legacyUserID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_sessions(id,user_id,expires_at,revoked,remember,created_at)
		VALUES('legacysession0000001',$1,9999999999,0,0,100)`, legacyUserID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_audit(id,created_at,actor_id,actor_username,action,object_type,object_id,result,detail,request_id)
		VALUES('legacyaudit00000001',100,$1,'legacy','auth.login','user',$1,'success','','req-legacy')`, legacyUserID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO oidc_login_states(state_hash,nonce,code_verifier,redirect_to,expires_at,created_at)
		VALUES('legacystatehash0001','nonce','verifier','/',9999999999,100)`); err != nil {
		t.Fatal(err)
	}

	if err := Apply(ctx, db); err != nil {
		t.Fatalf("apply tenant migration over existing rows: %v", err)
	}

	// The seeded default tenant exists exactly once.
	var tenantSlug, tenantName, tenantStatus string
	if err := db.QueryRowContext(ctx, `SELECT slug,name,status FROM tenants WHERE id=$1`, seedTenantID).
		Scan(&tenantSlug, &tenantName, &tenantStatus); err != nil {
		t.Fatalf("seeded tenant missing: %v", err)
	}
	if tenantSlug != "default" || tenantName != "Default" || tenantStatus != "active" {
		t.Fatalf("seeded tenant = %q/%q/%q", tenantSlug, tenantName, tenantStatus)
	}

	// Existing rows are backfilled with the seeded tenant.
	for _, query := range []string{
		`SELECT tenant_id FROM auth_users WHERE id='` + legacyUserID + `'`,
		`SELECT tenant_id FROM auth_sessions WHERE id='legacysession0000001'`,
		`SELECT tenant_id FROM auth_audit WHERE id='legacyaudit00000001'`,
		`SELECT tenant_id FROM oidc_login_states WHERE state_hash='legacystatehash0001'`,
	} {
		var got string
		if err := db.QueryRowContext(ctx, query).Scan(&got); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		if got != seedTenantID {
			t.Fatalf("%s backfilled tenant = %q, want %q", query, got, seedTenantID)
		}
	}

	// NOT NULL on the auth-owned tables; the OIDC one-time state stays nullable.
	for _, table := range []string{"auth_users", "auth_sessions", "auth_audit"} {
		info, ok := columnState(t, db, table, "tenant_id")
		if !ok {
			t.Fatalf("%s.tenant_id missing", table)
		}
		if info.dataType != "text" || info.isNullable != "NO" {
			t.Fatalf("%s.tenant_id = %+v, want NOT NULL text", table, info)
		}
	}
	if info, ok := columnState(t, db, "oidc_login_states", "tenant_id"); !ok || info.isNullable != "YES" {
		t.Fatalf("oidc_login_states.tenant_id = %+v, want nullable", info)
	}

	// Every tenant column carries a foreign key to tenants(id) with RESTRICT.
	for table, name := range map[string]string{
		"auth_users":        "auth_users_tenant_fk",
		"auth_sessions":     "auth_sessions_tenant_fk",
		"auth_audit":        "auth_audit_tenant_fk",
		"oidc_login_states": "oidc_login_states_tenant_fk",
	} {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pg_constraint
			WHERE conname=$1 AND contype='f' AND conrelid=$2::regclass AND confdeltype='r'`, name, table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("foreign key %s on %s missing or not ON DELETE RESTRICT", name, table)
		}
	}

	// The global uniqueness indexes are gone and replaced by tenant-scoped ones.
	for _, name := range []string{"auth_users_username_lower_idx", "auth_users_email_lower_idx"} {
		if _, ok := indexDefinition(t, db, name); ok {
			t.Fatalf("legacy global index %s still exists", name)
		}
	}
	usernameIndex, ok := indexDefinition(t, db, "auth_users_tenant_username_lower_idx")
	if !ok || !strings.Contains(usernameIndex, "UNIQUE") || !strings.Contains(usernameIndex, "tenant_id, lower(username)") {
		t.Fatalf("tenant username index = %q (exists=%v)", usernameIndex, ok)
	}
	emailIndex, ok := indexDefinition(t, db, "auth_users_tenant_email_lower_idx")
	if !ok || !strings.Contains(emailIndex, "UNIQUE") || !strings.Contains(emailIndex, "tenant_id, lower(email)") {
		t.Fatalf("tenant email index = %q (exists=%v)", emailIndex, ok)
	}
	auditIndex, ok := indexDefinition(t, db, "auth_audit_tenant_time_idx")
	if !ok || !strings.Contains(auditIndex, "(tenant_id, created_at DESC, id)") {
		t.Fatalf("tenant audit index = %q (exists=%v)", auditIndex, ok)
	}
	// Existing filter indexes are kept so the pre-F18 audit queries stay fast.
	for _, name := range []string{"auth_audit_time_idx", "auth_audit_action_idx", "auth_audit_actor_idx"} {
		if _, ok := indexDefinition(t, db, name); !ok {
			t.Fatalf("existing audit index %s was dropped", name)
		}
	}

	// Tenant-scoped uniqueness: the same username/email may now exist in two
	// tenants, but never twice in one tenant.
	if _, err := db.ExecContext(ctx, `INSERT INTO tenants(id,slug,name,status,created_at,updated_at)
		VALUES('tenant_other_0000000000000001','other','Other','active',1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,tenant_id,username,password_hash,display_name,role,disabled,created_at,email)
		VALUES('otheruser00000000001','tenant_other_0000000000000001','legacy','other-hash','Other','member',0,100,'legacy@example.com')`); err != nil {
		t.Fatalf("same username/email in a second tenant rejected: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,tenant_id,username,password_hash,display_name,role,disabled,created_at,email)
		VALUES('dupuser000000000001',$1,'legacy','dup-hash','Dup','member',0,100,'dupe@example.com')`, seedTenantID); err == nil {
		t.Fatal("duplicate username inside one tenant accepted")
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,tenant_id,username,password_hash,display_name,role,disabled,created_at,email)
		VALUES('dupemail00000000001',$1,'legacy-two','dup-hash','Dup','member',0,100,'LEGACY@example.com')`, seedTenantID); err == nil {
		t.Fatal("duplicate case-insensitive email inside one tenant accepted")
	}
}
