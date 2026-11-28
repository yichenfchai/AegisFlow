// Package tenants owns tenant identity primitives shared by startup
// configuration, the auth service and the tenant operations CLI. Phase F1
// introduced exactly one tenant (the fixed row seeded by schema v18); phase F2
// adds the operator-only create/list/status primitives below so a second tenant
// can be provisioned, and every business query is already tenant-scoped the
// moment it exists. There is deliberately no HTTP tenant-management API yet.
package tenants

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"

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
	// ErrValidation rejects an invalid display name or status.
	ErrValidation = errors.New("validation failed")
	// ErrConflict means the slug is already taken.
	ErrConflict = errors.New("conflict")
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

// normalizeName trims and validates a tenant display name: non-empty, valid
// UTF-8, at most 128 bytes and a single line (it is rendered in operator output
// and, in a later batch, the UI).
func normalizeName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if !utf8.ValidString(name) || name == "" || len(name) > 128 {
		return "", fmt.Errorf("%w: name must be 1-128 UTF-8 bytes", ErrValidation)
	}
	if strings.ContainsFunc(name, unicode.IsControl) {
		return "", fmt.Errorf("%w: name must be a single line without control characters", ErrValidation)
	}
	return name, nil
}

// newID returns a 24-character base64url id with the same shape every other
// identity row uses.
func newID() (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Create provisions one new tenant. The slug is validated here and by the schema
// CHECK; a duplicate slug is ErrConflict and an invalid one ErrInvalidSlug, so
// the CLI can exit non-zero with a precise reason. New tenants start active.
// Creation does not migrate or seed business data: a tenant is a pure isolation
// boundary, and every business table is already tenant-scoped.
func Create(ctx context.Context, db *sql.DB, slug, name string) (*Tenant, error) {
	if !ValidSlug(slug) {
		return nil, fmt.Errorf("%w: %q must match %s (1-%d lowercase letters, digits and inner hyphens)", ErrInvalidSlug, slug, slugPattern.String(), MaxSlugLength)
	}
	name, err := normalizeName(name)
	if err != nil {
		return nil, err
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// The tenants advisory lock serializes creation against the startup
	// EnsureDefault rename, exactly like the migration path.
	if err = dbmigrate.Lock(ctx, tx, "tenants"); err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	_, err = tx.ExecContext(ctx, `INSERT INTO tenants(id,slug,name,status,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$5)`,
		id, slug, name, StatusActive, now)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, fmt.Errorf("%w: tenant slug %q already exists", ErrConflict, slug)
		}
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &Tenant{ID: id, Slug: slug, Name: name, Status: StatusActive, CreatedAt: now, UpdatedAt: now}, nil
}

// SetStatus changes one tenant's status (active or suspended) by slug and
// returns the updated row. A suspended tenant can no longer authenticate or
// refresh: the auth service re-checks the tenant row on every request. Existing
// sessions are not deleted; they become unusable while the tenant is suspended
// and stay revoked/expired semantics unchanged for the rest.
func SetStatus(ctx context.Context, db *sql.DB, slug, status string) (*Tenant, error) {
	if !ValidSlug(slug) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidSlug, slug)
	}
	if status != StatusActive && status != StatusSuspended {
		return nil, fmt.Errorf("%w: status must be active or suspended", ErrValidation)
	}
	now := time.Now().Unix()
	result, err := db.ExecContext(ctx, `UPDATE tenants SET status=$2,updated_at=$3 WHERE slug=$1`, slug, status, now)
	if err != nil {
		return nil, err
	}
	if n, err := result.RowsAffected(); err != nil {
		return nil, err
	} else if n == 0 {
		return nil, ErrNotFound
	}
	return BySlug(ctx, db, slug)
}

// Summary is one row of the operator tenant listing: identity, status and the
// number of user accounts. It never contains credentials.
type Summary struct {
	ID        string
	Slug      string
	Name      string
	Status    string
	UserCount int
	CreatedAt int64
	UpdatedAt int64
}

// List returns every tenant ordered by creation time, with a user count per
// tenant. It is an operator view: no session or tenant scope applies, and no
// credential material is selected.
func List(ctx context.Context, db *sql.DB) ([]Summary, error) {
	rows, err := db.QueryContext(ctx, `SELECT t.id,t.slug,t.name,t.status,
		(SELECT COUNT(*) FROM auth_users u WHERE u.tenant_id=t.id),t.created_at,t.updated_at
		FROM tenants t ORDER BY t.created_at,t.slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := make([]Summary, 0)
	for rows.Next() {
		var item Summary
		if err = rows.Scan(&item.ID, &item.Slug, &item.Name, &item.Status, &item.UserCount, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		list = append(list, item)
	}
	return list, rows.Err()
}
