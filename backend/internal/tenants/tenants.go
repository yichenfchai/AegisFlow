// Package tenants owns tenant identity primitives shared by startup
// configuration and the auth service. Phase F1 (multi-tenancy foundations)
// introduces exactly one tenant: the fixed row seeded by schema v18. Creating
// additional tenants is deliberately deferred to the batch that also isolates
// business data, so this package has no create/list API yet.
package tenants

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/yichenfchai/AegisFlow/backend/internal/dbmigrate"
)

const (
	// DefaultID is the fixed id of the tenant seeded by schema v18. It is
	// stable: renaming the slug never changes this id, so system rows that must
	// reference the deployment's single tenant can rely on it.
	DefaultID = "tenant_default_0000000000000001"
	// DefaultSlug is the seeded slug and the value assumed when
	// AEGIS_DEFAULT_TENANT_SLUG is unset.
	DefaultSlug = "default"
	// StatusActive is the only status that may authenticate.
	StatusActive = "active"
	// StatusSuspended blocks every login and refresh in the tenant.
	StatusSuspended = "suspended"
	// MaxSlugLength bounds a slug's byte length (1-32 characters).
	MaxSlugLength = 32
)

var (
	// ErrNotFound means no tenant carries the requested slug/id.
	ErrNotFound = errors.New("tenant not found")
	// ErrInvalidSlug rejects slugs outside the documented format.
	ErrInvalidSlug = errors.New("invalid tenant slug")
)

// slugPattern is the documented AEGIS_DEFAULT_TENANT_SLUG format: 1-32
// lowercase ASCII letters, digits and inner hyphens, never starting or ending
// with a hyphen. The same pattern is enforced by the schema CHECK constraint.
var slugPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,30}[a-z0-9])?$`)

// ValidSlug reports whether slug matches the documented format.
func ValidSlug(slug string) bool { return slugPattern.MatchString(slug) }

// Tenant is one tenant row.
type Tenant struct {
	ID        string
	Slug      string
	Name      string
	Status    string
	CreatedAt int64
	UpdatedAt int64
}

// Active reports whether the tenant may authenticate users.
func (t Tenant) Active() bool { return t.Status == StatusActive }

// Querier is satisfied by both *sql.DB and *sql.Tx, so callers can resolve a
// tenant inside their own transaction without racing a rename.
type Querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// BySlug resolves one tenant by slug. Unknown slugs return ErrNotFound.
func BySlug(ctx context.Context, q Querier, slug string) (*Tenant, error) {
	if !ValidSlug(slug) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidSlug, slug)
	}
	var t Tenant
	err := q.QueryRowContext(ctx, `SELECT id,slug,name,status,created_at,updated_at FROM tenants WHERE slug=$1`, slug).
		Scan(&t.ID, &t.Slug, &t.Name, &t.Status, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// EnsureDefault guarantees that the deployment's configured default tenant
// exists before the server starts serving:
//
//   - the configured slug already exists: nothing to do;
//   - the configured slug differs from the seeded default and the schema still
//     contains only that seeded tenant: the seed is atomically renamed (id and
//     name stay unchanged, so existing rows keep their tenant);
//   - anything else: fail closed with an explicit error instead of silently
//     serving a deployment whose default tenant is missing.
//
// The rename runs under the same advisory-lock convention as the migrations so
// two instances booting at once cannot both create/rename the row.
func EnsureDefault(ctx context.Context, db *sql.DB, slug string) (*Tenant, error) {
	if !ValidSlug(slug) {
		return nil, fmt.Errorf("%w: %q must match %s (1-%d lowercase letters, digits and inner hyphens)", ErrInvalidSlug, slug, slugPattern.String(), MaxSlugLength)
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = dbmigrate.Lock(ctx, tx, "tenants"); err != nil {
		return nil, err
	}
	current, err := BySlug(ctx, tx, slug)
	if err == nil {
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		return current, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if slug == DefaultSlug {
		return nil, fmt.Errorf("default tenant %q is missing although schema v18 seeds it; refusing to start", slug)
	}
	// Rename the seed only while it is the single tenant. The NOT EXISTS guard
	// makes the transition atomic: a concurrent tenant creation would make the
	// UPDATE match zero rows and this branch fail closed.
	now := time.Now().Unix()
	result, err := tx.ExecContext(ctx, `UPDATE tenants SET slug=$1, updated_at=$2
		WHERE id=$3 AND slug=$4 AND NOT EXISTS (SELECT 1 FROM tenants WHERE id<>$3)`,
		slug, now, DefaultID, DefaultSlug)
	if err != nil {
		return nil, err
	}
	renamed, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if renamed != 1 {
		var otherTenants int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM tenants WHERE id<>$1`, DefaultID).Scan(&otherTenants); err != nil {
			return nil, err
		}
		if otherTenants > 0 {
			return nil, fmt.Errorf("default tenant %q does not exist and cannot be created automatically because %d other tenant(s) exist; refusing to start", slug, otherTenants)
		}
		return nil, fmt.Errorf("default tenant %q does not exist and the seeded tenant row (id %s) is missing; refusing to start", slug, DefaultID)
	}
	created, err := BySlug(ctx, tx, slug)
	if err != nil {
		return nil, fmt.Errorf("read renamed default tenant: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return created, nil
}
