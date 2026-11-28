// Package knowledge is the knowledge-base document registry (P5 remaining
// modules, batch B). A document is plain text or Markdown stored verbatim: the
// server validates and persists it but never renders, evaluates or sanitizes
// the body, so a stored <script> payload is inert data that the client must
// render as text. Reads are available to every authenticated role; create,
// update and delete are admin-only and audited in the same transaction as the
// change. The package depends on context.Context, never on Gin; the HTTP layer
// owns authentication and the error envelope.
package knowledge

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
)

var (
	// ErrValidation is returned for every invalid payload or filter; the HTTP
	// layer maps it to one 400.
	ErrValidation = errors.New("validation failed")
	ErrNotFound   = errors.New("not found")
	// ErrForbidden is the defense-in-depth refusal of a non-admin write. The
	// HTTP routes also require the admin role.
	ErrForbidden = errors.New("forbidden")
)

// Audited knowledge-base actions, recorded in the shared auth_audit trail. The
// detail only carries the document id and title; the body (which may contain
// sensitive runbook content) never enters the audit trail.
const (
	ActionCreate = "knowledge.create"
	ActionUpdate = "knowledge.update"
	ActionDelete = "knowledge.delete"
	// ObjectType is the audit object type of every knowledge action.
	ObjectType = "knowledge_doc"

	globalAdminRole = "admin"
)

const (
	// maxTitleBytes bounds the single-line title.
	maxTitleBytes = 200
	// maxBodyBytes bounds the Markdown/plain-text body (100 KiB).
	maxBodyBytes = 100 << 10
	// maxTagsBytes bounds the whole comma-separated tag line.
	maxTagsBytes = 200
	// maxQueryBytes bounds the free-text title search.
	maxQueryBytes = 200
)

// Doc is the list projection of one knowledge document. The body is
// deliberately absent: the list endpoint must not ship every document body.
// CreatedByName/UpdatedByName are resolved from auth_users for display and are
// omitted when the actor row no longer resolves.
type Doc struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	Tags          string `json:"tags"`
	CreatedBy     string `json:"createdBy"`
	CreatedByName string `json:"createdByName,omitempty"`
	UpdatedByName string `json:"updatedByName,omitempty"`
	CreatedAt     int64  `json:"createdAt"`
	UpdatedAt     int64  `json:"updatedAt"`
}

// Document is the detail projection: the list metadata plus the stored body.
type Document struct {
	Doc
	Body string `json:"body"`
}

// Input is the validated create/update payload. Tags is normalized to a
// canonical comma-separated form; an empty Tags means "no tags".
type Input struct {
	Title string
	Body  string
	Tags  string
}

// ListFilter is the knowledge-list filter. Query is a title substring (ILIKE,
// with % and _ treated literally); Limit/Offset use the shared 1-100 and
// non-negative pagination bounds.
type ListFilter struct {
	Query  string
	Limit  int
	Offset int
}

type Service struct {
	db *sql.DB
}

func New(db *sql.DB) *Service { return &Service{db: db} }

// listColumns is the list projection with the actor usernames resolved for
// display. created_by is always set; updated_by is set by every write.
const listColumns = `d.id,d.title,d.tags,d.created_by,COALESCE(cu.username,''),COALESCE(uu.username,''),d.created_at,d.updated_at`

// documentColumns is the detail projection, which adds the body.
const documentColumns = `d.id,d.title,d.body,d.tags,d.created_by,COALESCE(cu.username,''),COALESCE(uu.username,''),d.created_at,d.updated_at`

// docJoins resolves creator/updater display names. Both joins are LEFT joins:
// actor snapshots are not foreign keys, so a removed account must not hide the
// document. The joins are tenant-qualified like every other actor join, so a
// corrupted snapshot can never display another tenant's username.
const docJoins = ` FROM knowledge_docs d
	LEFT JOIN auth_users cu ON cu.id=d.created_by AND cu.tenant_id=d.tenant_id
	LEFT JOIN auth_users uu ON uu.id=d.updated_by AND uu.tenant_id=d.tenant_id`

type rowScanner interface{ Scan(dest ...any) error }

func scanDoc(row rowScanner) (*Doc, error) {
	var doc Doc
	if err := row.Scan(&doc.ID, &doc.Title, &doc.Tags, &doc.CreatedBy, &doc.CreatedByName,
		&doc.UpdatedByName, &doc.CreatedAt, &doc.UpdatedAt); err != nil {
		return nil, err
	}
	return &doc, nil
}

func scanDocument(row rowScanner) (*Document, error) {
	var doc Document
	if err := row.Scan(&doc.ID, &doc.Title, &doc.Body, &doc.Tags, &doc.CreatedBy, &doc.CreatedByName,
		&doc.UpdatedByName, &doc.CreatedAt, &doc.UpdatedAt); err != nil {
		return nil, err
	}
	return &doc, nil
}

func newID() (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// singleLine reports whether value contains no control characters (which
// includes CR/LF and tabs): the stored field is a single display line.
func singleLine(value string) bool {
	return !strings.ContainsFunc(value, unicode.IsControl)
}

// normalizeTags canonicalizes the comma-separated tag line: each tag is
// trimmed, internal whitespace runs collapse to one space, empty tags are
// dropped and the result is comma-joined without spaces. The canonical form
// keeps the title/tags search and display predictable.
func normalizeTags(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if !utf8.ValidString(raw) || len(raw) > maxTagsBytes {
		return "", fmt.Errorf("%w: tags must be valid UTF-8 of at most %d bytes", ErrValidation, maxTagsBytes)
	}
	if !singleLine(raw) {
		return "", fmt.Errorf("%w: tags must be a single line without control characters", ErrValidation)
	}
	parts := strings.Split(raw, ",")
	tags := make([]string, 0, len(parts))
	for _, part := range parts {
		tag := strings.Join(strings.Fields(part), " ")
		if tag == "" {
			continue
		}
		tags = append(tags, tag)
	}
	return strings.Join(tags, ","), nil
}

// normalize validates the create/update payload. The title is required, trimmed
// and must stay on one line; the body is stored verbatim (newlines and
// indentation are meaningful for Markdown) but must be valid UTF-8 without NUL
// and at most 100 KiB; tags are optional and canonicalized.
func normalize(input Input) (string, string, string, error) {
	title := strings.TrimSpace(input.Title)
	if !utf8.ValidString(title) || len(title) < 1 || len(title) > maxTitleBytes {
		return "", "", "", fmt.Errorf("%w: title must be 1-%d UTF-8 bytes", ErrValidation, maxTitleBytes)
	}
	if !singleLine(title) {
		return "", "", "", fmt.Errorf("%w: title must be a single line without control characters", ErrValidation)
	}
	body := input.Body
	if !utf8.ValidString(body) {
		return "", "", "", fmt.Errorf("%w: body must be valid UTF-8", ErrValidation)
	}
	if strings.ContainsRune(body, '\x00') {
		return "", "", "", fmt.Errorf("%w: body must not contain NUL bytes", ErrValidation)
	}
	if len(body) > maxBodyBytes {
		return "", "", "", fmt.Errorf("%w: body must be at most %d bytes", ErrValidation, maxBodyBytes)
	}
	tags, err := normalizeTags(input.Tags)
	if err != nil {
		return "", "", "", err
	}
	return title, body, tags, nil
}

// paginate enforces the shared pagination bounds.
func paginate(limit, offset int) error {
	if limit < 1 || limit > 100 || offset < 0 {
		return fmt.Errorf("%w: limit must be 1-100 and offset non-negative", ErrValidation)
	}
	return nil
}

// likePattern escapes the ILIKE wildcards in a user search so a query like
// "100%" matches the literal text instead of every document. The backslash is
// escaped first, then percent and underscore; the caller binds the result to
// ILIKE ... ESCAPE '\'.
func likePattern(query string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + replacer.Replace(query) + "%"
}

// validateQuery trims the title search and bounds it so a pathological query
// cannot become an unbounded LIKE pattern.
func validateQuery(query string) (string, error) {
	query = strings.TrimSpace(query)
	if !utf8.ValidString(query) || len(query) > maxQueryBytes {
		return "", fmt.Errorf("%w: q must be valid UTF-8 of at most %d bytes", ErrValidation, maxQueryBytes)
	}
	return query, nil
}

// requireAdmin reports whether the actor may write documents. Reads need no
// role check beyond the HTTP authentication layer; writes are admin-only, so
// the check is repeated here for a direct service caller.
func requireAdmin(actor auth.Actor) error {
	if actor.Role != globalAdminRole {
		return fmt.Errorf("%w: knowledge documents are writable by admins only", ErrForbidden)
	}
	return nil
}

// writeAudit inserts one knowledge audit row inside the caller's transaction so
// the change and its evidence commit together. detail only carries the id and
// title: the body never enters the audit trail.
func writeAudit(ctx context.Context, tx *sql.Tx, actor auth.Actor, action, objectID, detail string) error {
	id, err := newID()
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO auth_audit(id,tenant_id,created_at,actor_id,actor_username,action,object_type,object_id,result,detail,request_id)
	 VALUES($1,$2,$3,$4,$5,$6,$7,$8,'success',$9,$10)`,
		id, actor.TenantID, time.Now().Unix(), actor.ID, actor.Username, action, ObjectType, objectID, detail, actor.RequestID)
	return err
}

// getDocumentTx reads one document inside the caller's transaction, scoped to
// the caller's tenant.
func getDocumentTx(ctx context.Context, tx *sql.Tx, tenantID, id string) (*Document, error) {
	doc, err := scanDocument(tx.QueryRowContext(ctx, `SELECT `+documentColumns+docJoins+` WHERE d.id=$1 AND d.tenant_id=$2`, id, tenantID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return doc, err
}

// Create stores one new admin-authored document of the actor's tenant and audits
// knowledge.create in the same transaction. The returned detail carries the
// stored body.
func (s *Service) Create(ctx context.Context, actor auth.Actor, input Input) (*Document, error) {
	if err := requireAdmin(actor); err != nil {
		return nil, err
	}
	title, body, tags, err := normalize(input)
	if err != nil {
		return nil, err
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO knowledge_docs(id,tenant_id,title,body,tags,created_by,updated_by,created_at,updated_at)
	 VALUES($1,$2,$3,$4,$5,$6,$6,$7,$7)`, id, actor.TenantID, title, body, tags, actor.ID, now); err != nil {
		return nil, err
	}
	if err = writeAudit(ctx, tx, actor, ActionCreate, id, fmt.Sprintf("id=%s title=%s", id, title)); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &Document{Doc: Doc{ID: id, Title: title, Tags: tags, CreatedBy: actor.ID,
		CreatedByName: actor.Username, UpdatedByName: actor.Username,
		CreatedAt: now, UpdatedAt: now}, Body: body}, nil
}

// Get returns one document of the caller's tenant with its body; an unknown or
// other-tenant id is ErrNotFound.
func (s *Service) Get(ctx context.Context, actor auth.Actor, docID string) (*Document, error) {
	if !auth.ValidID(docID) {
		return nil, fmt.Errorf("%w: invalid knowledge id", ErrValidation)
	}
	doc, err := scanDocument(s.db.QueryRowContext(ctx, `SELECT `+documentColumns+docJoins+` WHERE d.id=$1 AND d.tenant_id=$2`, docID, actor.TenantID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return doc, nil
}

// List pages the caller's tenant's documents by last update (newest first) with
// an optional title substring search. Every authenticated role may read; the body
// is never part of the projection.
func (s *Service) List(ctx context.Context, actor auth.Actor, filter ListFilter) ([]Doc, int, error) {
	if err := paginate(filter.Limit, filter.Offset); err != nil {
		return nil, 0, err
	}
	query, err := validateQuery(filter.Query)
	if err != nil {
		return nil, 0, err
	}
	// An empty query becomes "%%", which matches every title; the same SQL is
	// used for count and page so the two can never diverge.
	pattern := likePattern(query)
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM knowledge_docs d
		WHERE d.tenant_id=$2 AND d.title ILIKE $1 ESCAPE '\'`,
		pattern, actor.TenantID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+listColumns+docJoins+`
		WHERE d.tenant_id=$2 AND d.title ILIKE $1 ESCAPE '\'
		ORDER BY d.updated_at DESC,d.id DESC LIMIT $3 OFFSET $4`,
		pattern, actor.TenantID, filter.Limit, filter.Offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	list := make([]Doc, 0)
	for rows.Next() {
		doc, scanErr := scanDoc(rows)
		if scanErr != nil {
			return nil, 0, scanErr
		}
		list = append(list, *doc)
	}
	return list, total, rows.Err()
}

// Update replaces the title/body/tags of an existing document of the actor's
// tenant, stamps the updater and audits knowledge.update in the same transaction.
// An unknown or other-tenant id is ErrNotFound.
func (s *Service) Update(ctx context.Context, actor auth.Actor, docID string, input Input) (*Document, error) {
	if err := requireAdmin(actor); err != nil {
		return nil, err
	}
	if !auth.ValidID(docID) {
		return nil, fmt.Errorf("%w: invalid knowledge id", ErrValidation)
	}
	title, body, tags, err := normalize(input)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Lock the row first so a concurrent delete surfaces as ErrNotFound rather
	// than a silently reported update; the audit row then always describes a
	// document that still exists in this transaction.
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT title FROM knowledge_docs WHERE id=$1 AND tenant_id=$2 FOR UPDATE`, docID, actor.TenantID).Scan(&existing)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	if _, err = tx.ExecContext(ctx, `UPDATE knowledge_docs SET title=$2,body=$3,tags=$4,updated_by=$5,updated_at=$6 WHERE id=$1 AND tenant_id=$7`,
		docID, title, body, tags, actor.ID, now, actor.TenantID); err != nil {
		return nil, err
	}
	doc, err := getDocumentTx(ctx, tx, actor.TenantID, docID)
	if err != nil {
		return nil, err
	}
	if err = writeAudit(ctx, tx, actor, ActionUpdate, docID, fmt.Sprintf("id=%s title=%s", docID, title)); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return doc, nil
}

// Delete removes one document of the actor's tenant and audits knowledge.delete
// in the same transaction. An unknown or other-tenant id is ErrNotFound.
func (s *Service) Delete(ctx context.Context, actor auth.Actor, docID string) error {
	if err := requireAdmin(actor); err != nil {
		return err
	}
	if !auth.ValidID(docID) {
		return fmt.Errorf("%w: invalid knowledge id", ErrValidation)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var title string
	err = tx.QueryRowContext(ctx, `DELETE FROM knowledge_docs WHERE id=$1 AND tenant_id=$2 RETURNING title`, docID, actor.TenantID).Scan(&title)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err = writeAudit(ctx, tx, actor, ActionDelete, docID, fmt.Sprintf("id=%s title=%s", docID, title)); err != nil {
		return err
	}
	return tx.Commit()
}
