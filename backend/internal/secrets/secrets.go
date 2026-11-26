// Package secrets is the independent write-only secret store (P5 remaining
// modules, batch B). A secret is a named opaque value sealed at rest with
// AES-256-GCM; the plaintext is never stored and there is no read path that
// returns it, so the API only exposes metadata (id, name, creator, timestamps).
// The value is encrypted with AEGIS_SECRET_KEY — the same key deployment-target
// credentials already use — and the AAD binds the ciphertext to the secret id
// (aegisflow:secrets:<id>), so a sealed value copied to another row cannot be
// opened there. Rotation replaces the ciphertext and bumps key_version; a
// single key is supported (the previous-key/rekey machinery of deployment
// credentials is deliberately not wired here, and old ciphertexts become
// unreadable if AEGIS_SECRET_KEY changes — acceptable because values are never
// read back out). The package depends on context.Context, never on Gin: the
// HTTP layer owns the admin middleware and the error envelope.
package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
)

var (
	// ErrValidation is returned for every invalid payload or pagination value;
	// the HTTP layer maps it to one 400.
	ErrValidation = errors.New("validation failed")
	ErrNotFound   = errors.New("not found")
	// ErrConflict means the requested secret name already exists (409).
	ErrConflict = errors.New("conflict")
	// ErrForbidden is the defense-in-depth refusal of a non-admin caller. The
	// HTTP routes also require the admin role.
	ErrForbidden = errors.New("forbidden")
	// ErrNotConfigured means AEGIS_SECRET_KEY is unset (or not exactly 32
	// bytes): creating or rotating a value is impossible and answers 503. The
	// message is part of the documented HTTP contract.
	ErrNotConfigured = errors.New("secrets are not configured")
)

// Audited secret actions, recorded in the shared auth_audit trail. detail only
// carries the secret id and name; the value and the ciphertext never enter the
// audit trail.
const (
	ActionCreate = "secret.create"
	ActionRotate = "secret.update"
	ActionDelete = "secret.delete"
	// ObjectType is the audit object type of every secret action.
	ObjectType = "secret"

	globalAdminRole = "admin"
)

const (
	// maxNameBytes bounds the single-line secret name.
	maxNameBytes = 128
	// maxValueBytes bounds the stored value (a certificate, token or password
	// that must fit an environment variable).
	maxValueBytes = 4096
	// secretAADPrefix binds a ciphertext to the secret it belongs to.
	secretAADPrefix = "aegisflow:secrets:"
)

// Options is explicit dependency configuration. An empty SecretKey disables
// writes: metadata reads and deletes keep working, while create/rotate answer
// ErrNotConfigured instead of silently storing a plaintext value.
type Options struct {
	SecretKey []byte
}

// Metadata is the public view of one secret: identity and lifecycle only. There
// is deliberately no value, ciphertext or key-version field in JSON.
// CreatedByName is resolved from auth_users for display and omitted when the
// actor row no longer resolves.
type Metadata struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	CreatedBy     string `json:"createdBy"`
	CreatedByName string `json:"createdByName,omitempty"`
	CreatedAt     int64  `json:"createdAt"`
	UpdatedAt     int64  `json:"updatedAt"`
}

// ListFilter is the secret-list pagination. Limit/Offset use the shared 1-100
// and non-negative bounds.
type ListFilter struct {
	Limit  int
	Offset int
}

type Service struct {
	db        *sql.DB
	secretKey []byte
}

// New builds the store. The key is copied so the caller cannot mutate it later.
func New(db *sql.DB, opts Options) *Service {
	return &Service{db: db, secretKey: append([]byte(nil), opts.SecretKey...)}
}

// secretReady reports whether encryption at rest is configured.
func (s *Service) secretReady() bool { return len(s.secretKey) == 32 }

// metadataColumns is the only projection over secrets: ciphertext and
// key_version are never selected for a response.
const metadataColumns = `s.id,s.name,s.created_by,COALESCE(cu.username,''),s.created_at,s.updated_at`

const metadataJoins = ` FROM secrets s LEFT JOIN auth_users cu ON cu.id=s.created_by`

type rowScanner interface{ Scan(dest ...any) error }

func scanMetadata(row rowScanner) (*Metadata, error) {
	var meta Metadata
	if err := row.Scan(&meta.ID, &meta.Name, &meta.CreatedBy, &meta.CreatedByName, &meta.CreatedAt, &meta.UpdatedAt); err != nil {
		return nil, err
	}
	return &meta, nil
}

func newID() (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// secretAAD is the additional authenticated data binding a ciphertext to its
// row, so ciphertexts cannot be swapped between secrets.
func secretAAD(id string) []byte { return []byte(secretAADPrefix + id) }

// newGCM builds the AEAD for the configured key.
func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// encrypt seals value for the secret id with AES-256-GCM. The output layout is
// [12-byte random nonce][ciphertext+tag]; a random nonce per write means two
// equal values never produce equal ciphertext.
func (s *Service) encrypt(id, value string) ([]byte, error) {
	if !s.secretReady() {
		return nil, ErrNotConfigured
	}
	aead, err := newGCM(s.secretKey)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, []byte(value), secretAAD(id)), nil
}

// validateName trims and bounds the unique name; it must stay a single display
// line without control characters.
func validateName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if !utf8.ValidString(name) || len(name) < 1 || len(name) > maxNameBytes {
		return "", fmt.Errorf("%w: name must be 1-%d UTF-8 bytes", ErrValidation, maxNameBytes)
	}
	if strings.ContainsFunc(name, unicode.IsControl) {
		return "", fmt.Errorf("%w: name must be a single line without control characters", ErrValidation)
	}
	return name, nil
}

// validateValue bounds the write-only value. It is never echoed in an error.
func validateValue(value string) error {
	if !utf8.ValidString(value) || len(value) < 1 || len(value) > maxValueBytes {
		return fmt.Errorf("%w: value must be 1-%d UTF-8 bytes", ErrValidation, maxValueBytes)
	}
	return nil
}

// paginate enforces the shared pagination bounds.
func paginate(limit, offset int) error {
	if limit < 1 || limit > 100 || offset < 0 {
		return fmt.Errorf("%w: limit must be 1-100 and offset non-negative", ErrValidation)
	}
	return nil
}

// requireAdmin reports whether the actor may read or write secrets. Secret
// names/ids are never exposed to non-admins, so the refusal is a plain
// forbidden (the HTTP routes already require the admin role).
func requireAdmin(actor auth.Actor) error {
	if actor.Role != globalAdminRole {
		return fmt.Errorf("%w: secrets are admin-only", ErrForbidden)
	}
	return nil
}

// isUniqueViolation reports whether err is a PostgreSQL unique-constraint
// violation (SQLSTATE 23505), which for this table means a duplicate name.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// writeAudit inserts one secret audit row inside the caller's transaction so
// the change and its evidence commit together. detail only carries the id and
// name.
func writeAudit(ctx context.Context, tx *sql.Tx, actor auth.Actor, action, objectID, detail string) error {
	id, err := newID()
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO auth_audit(id,created_at,actor_id,actor_username,action,object_type,object_id,result,detail,request_id)
	 VALUES($1,$2,$3,$4,$5,$6,$7,'success',$8,$9)`,
		id, time.Now().Unix(), actor.ID, actor.Username, action, ObjectType, objectID, detail, actor.RequestID)
	return err
}

// getMetadataTx reads one secret's metadata inside the caller's transaction.
func getMetadataTx(ctx context.Context, tx *sql.Tx, id string) (*Metadata, error) {
	meta, err := scanMetadata(tx.QueryRowContext(ctx, `SELECT `+metadataColumns+metadataJoins+` WHERE s.id=$1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return meta, err
}

// Create stores a new named secret: the value is sealed before it reaches the
// database and secret.create is audited in the same transaction. A duplicate
// name is ErrConflict; a missing key is ErrNotConfigured (nothing is written).
func (s *Service) Create(ctx context.Context, actor auth.Actor, name, value string) (*Metadata, error) {
	if err := requireAdmin(actor); err != nil {
		return nil, err
	}
	if !s.secretReady() {
		return nil, ErrNotConfigured
	}
	name, err := validateName(name)
	if err != nil {
		return nil, err
	}
	if err = validateValue(value); err != nil {
		return nil, err
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	ciphertext, err := s.encrypt(id, value)
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO secrets(id,name,ciphertext,key_version,created_by,created_at,updated_at)
	 VALUES($1,$2,$3,1,$4,$5,$5)`, id, name, ciphertext, actor.ID, now)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: secret name already exists", ErrConflict)
		}
		return nil, err
	}
	if err = writeAudit(ctx, tx, actor, ActionCreate, id, fmt.Sprintf("id=%s name=%s", id, name)); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &Metadata{ID: id, Name: name, CreatedBy: actor.ID, CreatedByName: actor.Username,
		CreatedAt: now, UpdatedAt: now}, nil
}

// List pages secret metadata, newest first. No response field can carry the
// value or the ciphertext.
func (s *Service) List(ctx context.Context, actor auth.Actor, filter ListFilter) ([]Metadata, int, error) {
	if err := requireAdmin(actor); err != nil {
		return nil, 0, err
	}
	if err := paginate(filter.Limit, filter.Offset); err != nil {
		return nil, 0, err
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM secrets`).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+metadataColumns+metadataJoins+`
		ORDER BY s.created_at DESC,s.id DESC LIMIT $1 OFFSET $2`, filter.Limit, filter.Offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	list := make([]Metadata, 0)
	for rows.Next() {
		meta, scanErr := scanMetadata(rows)
		if scanErr != nil {
			return nil, 0, scanErr
		}
		list = append(list, *meta)
	}
	return list, total, rows.Err()
}

// Rotate replaces an existing secret's value, bumps key_version and audits
// secret.update in the same transaction. The returned metadata never contains
// the new value. An unknown id is ErrNotFound; a missing key is
// ErrNotConfigured. READ COMMITTED plus the row lock serializes concurrent
// rotations, so each rotation records exactly one version increment and one
// audit row.
func (s *Service) Rotate(ctx context.Context, actor auth.Actor, secretID, value string) (*Metadata, error) {
	if err := requireAdmin(actor); err != nil {
		return nil, err
	}
	if !s.secretReady() {
		return nil, ErrNotConfigured
	}
	if !auth.ValidID(secretID) {
		return nil, fmt.Errorf("%w: invalid secret id", ErrValidation)
	}
	if err := validateValue(value); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var keyVersion int
	err = tx.QueryRowContext(ctx, `SELECT key_version FROM secrets WHERE id=$1 FOR UPDATE`, secretID).Scan(&keyVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	ciphertext, err := s.encrypt(secretID, value)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE secrets SET ciphertext=$2,key_version=$3,updated_at=$4 WHERE id=$1`,
		secretID, ciphertext, keyVersion+1, time.Now().Unix()); err != nil {
		return nil, err
	}
	meta, err := getMetadataTx(ctx, tx, secretID)
	if err != nil {
		return nil, err
	}
	if err = writeAudit(ctx, tx, actor, ActionRotate, secretID, fmt.Sprintf("id=%s name=%s", secretID, meta.Name)); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return meta, nil
}

// Delete removes one secret and audits secret.delete in the same transaction.
// The ciphertext is discarded with the row; an unknown id is ErrNotFound.
func (s *Service) Delete(ctx context.Context, actor auth.Actor, secretID string) error {
	if err := requireAdmin(actor); err != nil {
		return err
	}
	if !auth.ValidID(secretID) {
		return fmt.Errorf("%w: invalid secret id", ErrValidation)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var name string
	err = tx.QueryRowContext(ctx, `DELETE FROM secrets WHERE id=$1 RETURNING name`, secretID).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err = writeAudit(ctx, tx, actor, ActionDelete, secretID, fmt.Sprintf("id=%s name=%s", secretID, name)); err != nil {
		return err
	}
	return tx.Commit()
}
