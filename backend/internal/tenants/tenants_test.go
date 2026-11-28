package tenants

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yichenfchai/AegisFlow/backend/internal/dbmigrate"
	"github.com/yichenfchai/AegisFlow/backend/internal/testutil"
)

func TestValidSlug(t *testing.T) {
	for _, slug := range []string{"default", "a", "acme", "acme-corp", "team-01", "0", strings.Repeat("a", 32)} {
		if !ValidSlug(slug) {
			t.Fatalf("ValidSlug(%q) = false, want true", slug)
		}
	}
	for _, slug := range []string{"", "-acme", "acme-", "Acme", "acme corp", "acme_corp", "acme/", strings.Repeat("a", 33), "中"} {
		if ValidSlug(slug) {
			t.Fatalf("ValidSlug(%q) = true, want false", slug)
		}
	}
}

func migratedDatabase(t *testing.T) *sql.DB {
	t.Helper()
	db := testutil.OpenPostgres(t)
	if err := dbmigrate.Apply(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestEnsureDefaultSeedsAndRenames(t *testing.T) {
	db := migratedDatabase(t)
	ctx := context.Background()

	seeded, err := EnsureDefault(ctx, db, DefaultSlug)
	if err != nil {
		t.Fatal(err)
	}
	if seeded.ID != DefaultID || seeded.Slug != DefaultSlug || seeded.Name != "Default" || !seeded.Active() {
		t.Fatalf("seeded tenant = %+v", seeded)
	}

	// The documented rename path: the configured slug differs and the schema
	// contains only the seeded tenant, so the same row keeps its id.
	renamed, err := EnsureDefault(ctx, db, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if renamed.ID != DefaultID || renamed.Slug != "acme" {
		t.Fatalf("renamed tenant = %+v", renamed)
	}
	again, err := EnsureDefault(ctx, db, "acme")
	if err != nil || again.ID != DefaultID || again.Slug != "acme" {
		t.Fatalf("idempotent ensure = %+v, %v", again, err)
	}
	var count int
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tenants`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("tenant count after rename = %d, %v", count, err)
	}
	// The seeded slug is gone: starting with the default slug after a rename is
	// a configuration regression and must fail closed.
	if _, err = EnsureDefault(ctx, db, DefaultSlug); err == nil {
		t.Fatal("missing default slug accepted after rename")
	}

	// Once any other tenant exists, a missing configured slug can no longer be
	// created by renaming the seed.
	if _, err = db.ExecContext(ctx, `INSERT INTO tenants(id,slug,name,status,created_at,updated_at)
		VALUES('tenant_other_0000000000000001','other','Other','active',1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err = EnsureDefault(ctx, db, "missing"); err == nil {
		t.Fatal("missing tenant accepted while other tenants exist")
	}
	if _, err = EnsureDefault(ctx, db, "Bad Slug"); !errors.Is(err, ErrInvalidSlug) {
		t.Fatalf("invalid slug error = %v", err)
	}
}

func TestEnsureDefaultConcurrentRename(t *testing.T) {
	db := migratedDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	db2 := testutil.ClonePostgres(t, db)
	start := make(chan struct{})
	results := make(chan *Tenant, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, candidate := range []*sql.DB{db, db2} {
		wg.Add(1)
		go func(db *sql.DB) {
			defer wg.Done()
			<-start
			tenant, err := EnsureDefault(ctx, db, "acme")
			results <- tenant
			errs <- err
		}(candidate)
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent ensure: %v", err)
		}
	}
	for tenant := range results {
		if tenant == nil || tenant.ID != DefaultID || tenant.Slug != "acme" {
			t.Fatalf("concurrent ensure result = %+v", tenant)
		}
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tenants`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("tenant count after concurrent rename = %d, %v", count, err)
	}
}
