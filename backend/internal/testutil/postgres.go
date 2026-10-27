// Package testutil supplies real PostgreSQL integration-test databases.
package testutil

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// OpenPostgres opens a pool of eight connections in a random isolated schema.
// A configured but unreachable database fails tests; an absent URL explicitly skips.
// The role must be permitted to CREATE SCHEMA. No SQLite or mock fallback exists.
func OpenPostgres(t testing.TB) *sql.DB {
	t.Helper()
	u := postgresURL(t)
	admin := openPool(t, u.String())
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	schema := "aegis_test_" + hex.EncodeToString(random[:])
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := dropSchema(ctx, admin, schema); err != nil {
			t.Errorf("drop PostgreSQL test schema: %v", err)
		}
	})
	return schemaPool(t, u, schema)
}

// dropSchema removes a test schema, retrying when PostgreSQL reports a
// deadlock. Parallel test packages create and drop schemas in the same
// database; catalog lock ordering can deadlock even though each statement is
// independent, and a deadlock victim is rolled back. The drop is idempotent
// (IF EXISTS), so retrying is safe.
func dropSchema(ctx context.Context, db *sql.DB, schema string) error {
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		if _, err = db.ExecContext(ctx, `DROP SCHEMA IF EXISTS "`+schema+`" CASCADE`); err == nil || !isDeadlock(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(time.Duration(attempt+1) * 50 * time.Millisecond):
		}
	}
	return err
}

func isDeadlock(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40P01"
}

// ClonePostgres opens an independent pool in an existing test database schema.
// It does not own the schema; the original OpenPostgres cleanup owns its lifetime.
func ClonePostgres(t testing.TB, source *sql.DB) *sql.DB {
	t.Helper()
	var schema string
	if err := source.QueryRow(`SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	return schemaPool(t, postgresURL(t), schema)
}

func postgresURL(t testing.TB) *url.URL {
	t.Helper()
	raw := os.Getenv("AEGIS_TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("real PostgreSQL integration test skipped: set AEGIS_TEST_DATABASE_URL")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" || u.Path == "" {
		t.Fatal("AEGIS_TEST_DATABASE_URL must be a postgres:// or postgresql:// database URL")
	}
	return u
}

func schemaPool(t testing.TB, u *url.URL, schema string) *sql.DB {
	t.Helper()
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return openPool(t, u.String())
}

func openPool(t testing.TB, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close PostgreSQL test pool: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("connect real PostgreSQL test database: %v", err)
	}
	return db
}
