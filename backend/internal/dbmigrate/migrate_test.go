package dbmigrate

import (
	"context"
	"regexp"
	"strings"
	"testing"

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
