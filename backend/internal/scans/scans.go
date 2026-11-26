// Package scans is the scan-result registry (P5 remaining modules, batch A).
// It receives scan verdicts that external scanning tools deliver through the
// signed HMAC webhook; AegisFlow never runs a scanner itself. A scan is an
// immutable evidence row attached to one project: the delivery id is the
// idempotency key, so a retried webhook returns the stored row instead of
// duplicating it. Reads follow the project visibility predicate, exactly like
// builds and reviews, so scan ids cannot be used to enumerate private projects.
// The package depends on context.Context, never on Gin; the HTTP layer owns the
// signature check and the error envelope.
package scans

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
	"github.com/yichenfchai/AegisFlow/backend/internal/ingest"
	"github.com/yichenfchai/AegisFlow/backend/internal/projects"
)

var (
	// ErrValidation is the shared ingest validation sentinel: every payload
	// failure maps to one HTTP 400 in the transport layer.
	ErrValidation = ingest.ErrInvalid
	ErrNotFound   = errors.New("not found")
	// ErrConflict means the stored delivery row disappeared between the insert
	// and the retry read (the project was deleted concurrently).
	ErrConflict = errors.New("conflict")
)

// Verdicts accepted from an external scanning tool. They mirror the external
// review-result vocabulary so one dashboard can read both.
const (
	VerdictPassed  = "passed"
	VerdictFailed  = "failed"
	VerdictWarning = "warning"
)

const (
	maxSourceBytes  = 64
	maxToolBytes    = 64
	maxVersionBytes = 64
	// maxSummaryBytes bounds the free-text scan note; it may be multi-line.
	maxSummaryBytes = 1000
)

// Scan is one immutable scan result delivered by an external tool. Tool and
// Version are empty when the delivery carried none; ReportURL is omitted when
// absent. ProjectName is resolved from the project registry for display, so the
// list does not need a second project query per row.
type Scan struct {
	ID          string `json:"id"`
	ProjectID   string `json:"projectId"`
	ProjectName string `json:"projectName"`
	Source      string `json:"source"`
	Tool        string `json:"tool"`
	Version     string `json:"version"`
	Verdict     string `json:"verdict"`
	Summary     string `json:"summary"`
	ReportURL   string `json:"reportUrl,omitempty"`
	ReceivedAt  int64  `json:"receivedAt"`
}

// Input is the validated inbound payload. DeliveryID and ReceivedAt are
// supplied by the transport/database, not by the JSON body.
type Input struct {
	Source    string
	Tool      string
	Version   string
	Verdict   string
	Summary   string
	ReportURL string
}

// ListFilter is the global scan-list filter. ProjectID and Verdict are optional;
// Limit/Offset use the shared 1-100/non-negative pagination bounds.
type ListFilter struct {
	ProjectID string
	Verdict   string
	Limit     int
	Offset    int
}

type Service struct {
	db *sql.DB
}

func New(db *sql.DB) *Service { return &Service{db: db} }

// scanColumns is shared by every read; report_url may be NULL in the database.
const scanColumns = `s.id,s.project_id,p.name,s.source,s.tool,s.version,s.verdict,s.summary,s.report_url,s.received_at`

type rowScanner interface{ Scan(dest ...any) error }

func scanScan(row rowScanner) (*Scan, error) {
	var scan Scan
	var reportURL sql.NullString
	if err := row.Scan(&scan.ID, &scan.ProjectID, &scan.ProjectName, &scan.Source, &scan.Tool, &scan.Version,
		&scan.Verdict, &scan.Summary, &reportURL, &scan.ReceivedAt); err != nil {
		return nil, err
	}
	scan.ReportURL = reportURL.String
	return &scan, nil
}

func newID() (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func validVerdict(verdict string) bool {
	switch verdict {
	case VerdictPassed, VerdictFailed, VerdictWarning:
		return true
	default:
		return false
	}
}

// normalize validates the inbound payload and returns the stored values. source
// is required and trimmed; tool and version are optional but, when present, are
// trimmed single lines of at most 64 bytes; summary may be multi-line and keeps
// its newlines; reportUrl is an optional display-only http(s) link the server
// never fetches.
func normalize(input Input) (Input, error) {
	source, err := ingest.BoundedSingleLine("source", input.Source, maxSourceBytes, false)
	if err != nil {
		return Input{}, err
	}
	tool, err := ingest.BoundedSingleLine("tool", input.Tool, maxToolBytes, true)
	if err != nil {
		return Input{}, err
	}
	version, err := ingest.BoundedSingleLine("version", input.Version, maxVersionBytes, true)
	if err != nil {
		return Input{}, err
	}
	if !validVerdict(input.Verdict) {
		return Input{}, fmt.Errorf("%w: verdict must be passed, failed or warning", ErrValidation)
	}
	summary, err := ingest.SanitizeText(input.Summary, maxSummaryBytes)
	if err != nil {
		return Input{}, fmt.Errorf("%w: summary must be valid UTF-8 of at most %d bytes", ErrValidation, maxSummaryBytes)
	}
	reportURL := strings.TrimSpace(input.ReportURL)
	if err = ingest.ValidateURL("reportUrl", reportURL); err != nil {
		return Input{}, err
	}
	return Input{Source: source, Tool: tool, Version: version, Verdict: input.Verdict, Summary: summary, ReportURL: reportURL}, nil
}

func paginate(limit, offset int) error {
	if limit < 1 || limit > 100 || offset < 0 {
		return fmt.Errorf("%w: limit must be 1-100 and offset non-negative", ErrValidation)
	}
	return nil
}

// validateFilter checks the optional verdict filter and pagination so the
// service contract holds even for a non-HTTP caller.
func validateFilter(verdict string, limit, offset int) error {
	if verdict != "" && !validVerdict(verdict) {
		return fmt.Errorf("%w: verdict filter must be passed, failed or warning", ErrValidation)
	}
	return paginate(limit, offset)
}

// Ingest stores one signed scan delivery for an existing project. The delivery
// id is the idempotency key: a repeated delivery returns the stored row and
// created=false, so a retried webhook never duplicates evidence even across
// replicas. A missing project is ErrNotFound; every payload failure is
// ErrValidation. Ingestion has no project-visibility concept: the trusted signer
// may report a scan for any existing project.
func (s *Service) Ingest(ctx context.Context, projectID, deliveryID string, input Input) (*Scan, bool, error) {
	if !auth.ValidID(projectID) {
		return nil, false, fmt.Errorf("%w: invalid project id", ErrValidation)
	}
	if err := ingest.ValidDeliveryID(deliveryID); err != nil {
		return nil, false, err
	}
	normalized, err := normalize(input)
	if err != nil {
		return nil, false, err
	}
	var found int
	err = s.db.QueryRowContext(ctx, `SELECT 1 FROM projects WHERE id=$1`, projectID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, ErrNotFound
	}
	if err != nil {
		return nil, false, err
	}
	id, err := newID()
	if err != nil {
		return nil, false, err
	}
	receivedAt := time.Now().Unix()
	result, err := s.db.ExecContext(ctx, `INSERT INTO scans
		(id,project_id,delivery_id,source,tool,version,verdict,summary,report_url,received_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''),$10)
		ON CONFLICT (delivery_id) DO NOTHING
		`, id, projectID, deliveryID, normalized.Source, normalized.Tool, normalized.Version,
		normalized.Verdict, normalized.Summary, normalized.ReportURL, receivedAt)
	if err != nil {
		return nil, false, err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return nil, false, err
	}
	// Read the stored row either way: a new delivery returns the row it just
	// created, while a repeated delivery returns the original row unchanged (200)
	// so the sender's retry is idempotent. The stored project is authoritative:
	// it is the project the original delivery named.
	stored, err := scanScan(s.db.QueryRowContext(ctx, `SELECT `+scanColumns+` FROM scans s
		JOIN projects p ON p.id=s.project_id WHERE s.delivery_id=$1`, deliveryID))
	if errors.Is(err, sql.ErrNoRows) {
		// The conflicting row disappeared between INSERT and SELECT (project
		// deletion cascaded); surface a conflict instead of a stale id.
		return nil, false, fmt.Errorf("%w: delivery %q was removed concurrently", ErrConflict, deliveryID)
	}
	if err != nil {
		return nil, false, err
	}
	return stored, inserted == 1, nil
}

// Get returns one scan whose project the actor may see; an invisible project is
// a 404, exactly like the project itself, so private scan ids stay
// non-enumerable.
func (s *Service) Get(ctx context.Context, actor auth.Actor, scanID string) (*Scan, error) {
	if !auth.ValidID(scanID) {
		return nil, fmt.Errorf("%w: invalid scan id", ErrValidation)
	}
	isAdmin, userID := projects.VisibilityArgs(actor)
	scan, err := scanScan(s.db.QueryRowContext(ctx, `SELECT `+scanColumns+` FROM scans s
		JOIN projects p ON p.id=s.project_id
		WHERE s.id=$1 AND `+projects.VisiblePredicate("p", 2, 3), scanID, isAdmin, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return scan, nil
}

// visibleProject applies the single project-visibility predicate; an invisible
// project is reported as not found so a project-scoped scan list cannot be used
// to probe private project ids.
func visibleProject(ctx context.Context, db *sql.DB, actor auth.Actor, projectID string) error {
	isAdmin, userID := projects.VisibilityArgs(actor)
	var found int
	err := db.QueryRowContext(ctx, `SELECT 1 FROM projects p WHERE p.id=$1 AND `+projects.VisiblePredicate("p", 2, 3),
		projectID, isAdmin, userID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// ListProject pages one visible project's scans, newest first, filtered by the
// optional verdict. An invisible project is ErrNotFound rather than an empty
// page, matching every other project-scoped read.
func (s *Service) ListProject(ctx context.Context, actor auth.Actor, projectID, verdict string, limit, offset int) ([]Scan, int, error) {
	if !auth.ValidID(projectID) {
		return nil, 0, fmt.Errorf("%w: invalid project id", ErrValidation)
	}
	if err := validateFilter(verdict, limit, offset); err != nil {
		return nil, 0, err
	}
	if err := visibleProject(ctx, s.db, actor, projectID); err != nil {
		return nil, 0, err
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM scans s WHERE s.project_id=$1 AND ($2='' OR s.verdict=$2)`,
		projectID, verdict).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+scanColumns+` FROM scans s
		JOIN projects p ON p.id=s.project_id
		WHERE s.project_id=$1 AND ($2='' OR s.verdict=$2)
		ORDER BY s.received_at DESC,s.id DESC LIMIT $3 OFFSET $4`, projectID, verdict, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	list := make([]Scan, 0)
	for rows.Next() {
		scan, scanErr := scanScan(rows)
		if scanErr != nil {
			return nil, 0, scanErr
		}
		list = append(list, *scan)
	}
	return list, total, rows.Err()
}

// ListVisible pages every scan the actor may see, newest first, with optional
// verdict and project filters. It reuses the single project visibility
// predicate, so a global admin sees every scan while every other role sees only
// visible projects. A projectId filter cannot widen that set: an invisible
// project simply contributes no rows, matching the global review and deployment
// lists, so the filter cannot probe private project ids.
func (s *Service) ListVisible(ctx context.Context, actor auth.Actor, filter ListFilter) ([]Scan, int, error) {
	if filter.ProjectID != "" && !auth.ValidID(filter.ProjectID) {
		return nil, 0, fmt.Errorf("%w: invalid project id", ErrValidation)
	}
	if err := validateFilter(filter.Verdict, filter.Limit, filter.Offset); err != nil {
		return nil, 0, err
	}
	isAdmin, userID := projects.VisibilityArgs(actor)
	from := ` FROM scans s JOIN projects p ON p.id=s.project_id
		WHERE ($1='' OR s.project_id=$1) AND ($2='' OR s.verdict=$2) AND ` + projects.VisiblePredicate("p", 3, 4)
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*)`+from,
		filter.ProjectID, filter.Verdict, isAdmin, userID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+scanColumns+` FROM scans s
		JOIN projects p ON p.id=s.project_id
		WHERE ($1='' OR s.project_id=$1) AND ($2='' OR s.verdict=$2) AND `+projects.VisiblePredicate("p", 3, 4)+`
		ORDER BY s.received_at DESC,s.id DESC LIMIT $5 OFFSET $6`,
		filter.ProjectID, filter.Verdict, isAdmin, userID, filter.Limit, filter.Offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	list := make([]Scan, 0)
	for rows.Next() {
		scan, scanErr := scanScan(rows)
		if scanErr != nil {
			return nil, 0, scanErr
		}
		list = append(list, *scan)
	}
	return list, total, rows.Err()
}
