// Package reviews is the project review center: gate-style change reviews that a
// project maintainer (or a global admin) approves or rejects, and that the
// requester or a maintainer can cancel. This slice owns the request record, the
// per-project number sequence and the audit trail; wiring a review into
// deployment gates and external results is a later slice. Status only moves
// pending -> approved/rejected/cancelled, and terminal rows never change again.
// The package depends on context.Context, never on Gin; the HTTP layer owns the
// error envelope. Visibility and write authorization reuse the project registry,
// so a review cannot widen who may see or change a project.
package reviews

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
	"github.com/yichenfchai/AegisFlow/backend/internal/projects"
)

var (
	ErrValidation = errors.New("validation failed")
	ErrConflict   = errors.New("conflict")
	ErrNotFound   = errors.New("not found")
	// ErrForbidden means the caller may see the project but may not write to it
	// or decide its reviews (a viewer, or a member without the maintainer role).
	ErrForbidden = errors.New("forbidden")
)

// Review statuses. A review starts pending and only a decision can move it to a
// terminal state; terminal states are immutable.
const (
	StatusPending   = "pending"
	StatusApproved  = "approved"
	StatusRejected  = "rejected"
	StatusCancelled = "cancelled"
)

// Audited review actions, recorded in the shared auth_audit trail. The detail
// only carries the project id and review number plus a decision reason; review
// bodies (subject, description, version, external URL) are never copied into
// the audit trail.
const (
	ActionRequest = "review.request"
	ActionApprove = "review.approve"
	ActionReject  = "review.reject"
	ActionCancel  = "review.cancel"
	// ObjectType is the audit object type of every review action.
	ObjectType = "review"
)

const (
	minSubjectBytes     = 3
	maxSubjectBytes     = 200
	maxDescriptionBytes = 2000
	maxVersionBytes     = 64
	maxExternalURLBytes = 2048
	maxReasonBytes      = 512
)

// globalAdminRole mirrors the auth package's role vocabulary; a global admin is
// never bounded by project visibility or membership.
const globalAdminRole = "admin"

// Review is one change-review request. DecidedBy/DecidedByName/DecidedAt/Reason
// only appear once a decision exists (or, for cancel, once the requester or a
// maintainer cancelled the pending request); a pending review omits them.
type Review struct {
	ID              string  `json:"id"`
	ProjectID       string  `json:"projectId"`
	Number          int64   `json:"number"`
	Subject         string  `json:"subject"`
	Description     string  `json:"description"`
	Version         string  `json:"version"`
	ExternalURL     string  `json:"externalUrl"`
	Status          string  `json:"status"`
	RequestedBy     string  `json:"requestedBy"`
	RequestedByName string  `json:"requestedByName"`
	DecidedBy       *string `json:"decidedBy,omitempty"`
	DecidedByName   *string `json:"decidedByName,omitempty"`
	DecidedAt       *int64  `json:"decidedAt,omitempty"`
	Reason          *string `json:"reason,omitempty"`
	CreatedAt       int64   `json:"createdAt"`
	UpdatedAt       int64   `json:"updatedAt"`
}

// Input is the create payload: subject is required, the other fields are
// optional context for the reviewer. ExternalURL is display-only and is never
// fetched by the server.
type Input struct {
	Subject     string
	Description string
	Version     string
	ExternalURL string
}

// ListFilter is the global review-list filter. ProjectID and Status are
// optional; Limit/Offset use the shared 1-100/non-negative pagination bounds.
type ListFilter struct {
	ProjectID string
	Status    string
	Limit     int
	Offset    int
}

// Options configures the optional review-event notification outbox. The
// outbound webhook reuses the approval channel (AEGIS_APPROVAL_WEBHOOK_*): one
// receiver is expected to handle both approval.* and review.* events, so this
// slice adds no outbound configuration of its own.
type Options struct {
	// WebhookURL enables review notifications; empty disables enqueueing and
	// the notifier poll loop.
	WebhookURL string
	// WebhookSecret signs the exact JSON payload with HMAC-SHA256 when set.
	WebhookSecret []byte
	// WebhookTimeout bounds one delivery; zero means 5s.
	WebhookTimeout time.Duration
}

type Service struct {
	db *sql.DB
	// projects decides project visibility and the write/maintainer overlays.
	// Nil fails closed: only global admins keep access.
	projects *projects.Service
	// Review notifications use a transactional outbox stored in
	// review_notifications; an empty webhookURL disables enqueueing.
	webhookURL    string
	webhookSecret []byte
	webhookClient *http.Client
	// notificationClaimLease bounds how long a claimed outbox row is invisible
	// to other replicas; it always exceeds the maximum allowed webhook delivery.
	notificationClaimLease time.Duration
}

// New builds the review-center service. The optional notification options are a
// variadic parameter so existing callers keep working; cmd/server passes the
// shared approval webhook configuration.
func New(db *sql.DB, registry *projects.Service, options ...Options) *Service {
	var opts Options
	if len(options) > 0 {
		opts = options[0]
	}
	webhookTimeout := opts.WebhookTimeout
	if webhookTimeout <= 0 {
		webhookTimeout = defaultNotificationWebhookTimeout
	}
	// A delivery that is still running must never be re-claimed by another
	// replica; derive the lease from the configured timeout plus headroom.
	claimLease := webhookTimeout + 30*time.Second
	if claimLease < minNotificationClaimLease {
		claimLease = minNotificationClaimLease
	}
	webhookClient := &http.Client{
		Timeout: webhookTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			// A redirect could post the signed body to an unintended host;
			// treat it as a delivery failure instead of following it.
			return http.ErrUseLastResponse
		},
	}
	return &Service{
		db:                     db,
		projects:               registry,
		webhookURL:             opts.WebhookURL,
		webhookSecret:          append([]byte(nil), opts.WebhookSecret...),
		webhookClient:          webhookClient,
		notificationClaimLease: claimLease,
	}
}

const reviewColumns = `id,project_id,number,subject,description,version,external_url,status,requested_by,decided_by,decided_at,reason,created_at,updated_at`

// reviewSelectColumns is reviewColumns with the requesting/deciding usernames
// resolved for display (accounts are disabled, never deleted, so a username is
// always available for a stored actor id).
const reviewSelectColumns = `r.id,r.project_id,r.number,r.subject,r.description,r.version,r.external_url,r.status,r.requested_by,COALESCE(ru.username,''),r.decided_by,COALESCE(du.username,''),r.decided_at,r.reason,r.created_at,r.updated_at`

type rowScanner interface{ Scan(dest ...any) error }

// rowQuerier is satisfied by *sql.DB and *sql.Tx.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// scanReview scans a row without the username join; it backs the locking read.
func scanReview(row rowScanner) (*Review, error) {
	var review Review
	var decidedBy, reason sql.NullString
	var decidedAt sql.NullInt64
	if err := row.Scan(&review.ID, &review.ProjectID, &review.Number, &review.Subject, &review.Description, &review.Version,
		&review.ExternalURL, &review.Status, &review.RequestedBy, &decidedBy, &decidedAt, &reason,
		&review.CreatedAt, &review.UpdatedAt); err != nil {
		return nil, err
	}
	if decidedBy.Valid {
		review.DecidedBy = &decidedBy.String
	}
	if decidedAt.Valid {
		review.DecidedAt = &decidedAt.Int64
	}
	if reason.Valid {
		review.Reason = &reason.String
	}
	return &review, nil
}

// scanReviewWithNames scans reviewSelectColumns, filling both display names.
func scanReviewWithNames(row rowScanner) (*Review, error) {
	var review Review
	var decidedBy, decidedByName, reason sql.NullString
	var decidedAt sql.NullInt64
	if err := row.Scan(&review.ID, &review.ProjectID, &review.Number, &review.Subject, &review.Description, &review.Version,
		&review.ExternalURL, &review.Status, &review.RequestedBy, &review.RequestedByName,
		&decidedBy, &decidedByName, &decidedAt, &reason, &review.CreatedAt, &review.UpdatedAt); err != nil {
		return nil, err
	}
	if decidedBy.Valid {
		review.DecidedBy = &decidedBy.String
		name := decidedByName.String
		review.DecidedByName = &name
	}
	if decidedAt.Valid {
		review.DecidedAt = &decidedAt.Int64
	}
	if reason.Valid {
		review.Reason = &reason.String
	}
	return &review, nil
}

func newID() (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// writeAudit inserts one review audit row inside the caller's transaction so the
// state change and its evidence commit together. detail never contains review
// bodies: only the review number and an optional decision reason.
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

// singleLine reports whether value contains no control characters (which
// includes CR/LF and tabs): the stored field is a single display line.
func singleLine(value string) bool {
	return !strings.ContainsFunc(value, unicode.IsControl)
}

// normalize validates the create payload and returns the stored values. subject
// and version are trimmed and must stay on one line; description is free text
// and keeps its formatting.
func normalize(input Input) (subject, description, version, externalURL string, err error) {
	subject = strings.TrimSpace(input.Subject)
	if !utf8.ValidString(subject) || len(subject) < minSubjectBytes || len(subject) > maxSubjectBytes {
		return "", "", "", "", fmt.Errorf("%w: subject must be 3-200 UTF-8 bytes", ErrValidation)
	}
	if !singleLine(subject) {
		return "", "", "", "", fmt.Errorf("%w: subject must be a single line without control characters", ErrValidation)
	}
	description = input.Description
	if !utf8.ValidString(description) || len(description) > maxDescriptionBytes {
		return "", "", "", "", fmt.Errorf("%w: description must be valid UTF-8 of at most %d bytes", ErrValidation, maxDescriptionBytes)
	}
	version = strings.TrimSpace(input.Version)
	if !utf8.ValidString(version) || len(version) > maxVersionBytes {
		return "", "", "", "", fmt.Errorf("%w: version must be valid UTF-8 of at most %d bytes", ErrValidation, maxVersionBytes)
	}
	if !singleLine(version) {
		return "", "", "", "", fmt.Errorf("%w: version must be a single line without control characters", ErrValidation)
	}
	externalURL = strings.TrimSpace(input.ExternalURL)
	if err = validateExternalURL(externalURL); err != nil {
		return "", "", "", "", err
	}
	return subject, description, version, externalURL, nil
}

// validateExternalURL accepts an optional display link over http(s) and rejects
// credentials in the URL (they would be shown to every reader) and fragments
// (they do not select a server resource). Queries stay allowed: external
// integrations frequently link to a filtered view.
func validateExternalURL(externalURL string) error {
	return validateDisplayURL("externalUrl", externalURL)
}

// validateDisplayURL is the shared display-link rule for the review body
// (externalUrl) and external results (url): optional, absolute http(s), no
// userinfo and no fragment. field only names the rejected input in the error.
func validateDisplayURL(field, value string) error {
	if value == "" {
		return nil
	}
	if !utf8.ValidString(value) || len(value) > maxExternalURLBytes {
		return fmt.Errorf("%w: %s must be valid UTF-8 of at most %d bytes", ErrValidation, field, maxExternalURLBytes)
	}
	if strings.ContainsFunc(value, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) {
		return fmt.Errorf("%w: %s must not contain whitespace", ErrValidation, field)
	}
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		return fmt.Errorf("%w: %s must be an absolute http or https URL with a host", ErrValidation, field)
	}
	if parsed.User != nil {
		return fmt.Errorf("%w: %s must not contain userinfo", ErrValidation, field)
	}
	if parsed.Fragment != "" || parsed.RawFragment != "" {
		return fmt.Errorf("%w: %s must not contain a fragment", ErrValidation, field)
	}
	return nil
}

// normalizeReason trims and bounds a decision reason. It becomes a stored field
// and an audit detail, so it must stay a single line. required is true for
// reject, where an empty reason would make the decision unexplainable.
func normalizeReason(reason string, required bool) (string, error) {
	reason = strings.TrimSpace(reason)
	if required && reason == "" {
		return "", fmt.Errorf("%w: reason is required", ErrValidation)
	}
	if !utf8.ValidString(reason) || len(reason) > maxReasonBytes {
		return "", fmt.Errorf("%w: reason must be valid UTF-8 of at most %d bytes", ErrValidation, maxReasonBytes)
	}
	if !singleLine(reason) {
		return "", fmt.Errorf("%w: reason must be a single line without control characters", ErrValidation)
	}
	return reason, nil
}

func validStatus(status string) bool {
	switch status {
	case StatusPending, StatusApproved, StatusRejected, StatusCancelled:
		return true
	default:
		return false
	}
}

func paginate(limit, offset int) error {
	if limit < 1 || limit > 100 || offset < 0 {
		return fmt.Errorf("%w: limit must be 1-100 and offset non-negative", ErrValidation)
	}
	return nil
}

func decideError(message string) error {
	return fmt.Errorf("%w: %s", ErrForbidden, message)
}

// visibleProject checks the single project-visibility predicate: an invisible
// project is reported as not found so review ids cannot be used to enumerate a
// private project.
func visibleProject(ctx context.Context, q rowQuerier, actor auth.Actor, projectID string) error {
	isAdmin, userID := projects.VisibilityArgs(actor)
	var found int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM projects p WHERE p.id=$1 AND `+projects.VisiblePredicate("p", 2, 3),
		projectID, isAdmin, userID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// authorizeProjectWrite mirrors the deployment write overlay: visibility first
// (404), then AuthorizeWrite (403 for a viewer or a non-maintainer of a private
// project).
func (s *Service) authorizeProjectWrite(ctx context.Context, q rowQuerier, actor auth.Actor, projectID string) error {
	if err := visibleProject(ctx, q, actor, projectID); err != nil {
		return err
	}
	if actor.Role == globalAdminRole {
		return nil
	}
	if s.projects == nil {
		return decideError("the review service is not wired to the project registry")
	}
	switch err := s.projects.AuthorizeWrite(ctx, q, actor, projectID); {
	case err == nil:
		return nil
	case errors.Is(err, projects.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, projects.ErrForbidden):
		return ErrForbidden
	default:
		return err
	}
}

// authorizeDecide is the reviewer overlay: global admin, or a global member that
// is a maintainer of this project. Visibility is checked by the caller.
func (s *Service) authorizeDecide(ctx context.Context, q rowQuerier, actor auth.Actor, projectID string) error {
	if s.projects == nil {
		if actor.Role == globalAdminRole {
			return nil
		}
		return decideError("the review service is not wired to the project registry")
	}
	switch err := s.projects.AuthorizeDecide(ctx, q, actor, projectID); {
	case err == nil:
		return nil
	case errors.Is(err, projects.ErrForbidden):
		return ErrForbidden
	default:
		return err
	}
}

// lockReview takes the review row lock, serializing every decision on one
// review. A missing row is not found.
func lockReview(ctx context.Context, tx *sql.Tx, id string) (*Review, error) {
	review, err := scanReview(tx.QueryRowContext(ctx, `SELECT `+reviewColumns+` FROM reviews WHERE id=$1 FOR UPDATE`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return review, err
}

// getReviewTx reads one review with its display names inside the caller's
// transaction. The caller has already authorized the project.
func getReviewTx(ctx context.Context, tx *sql.Tx, id string) (*Review, error) {
	review, err := scanReviewWithNames(tx.QueryRowContext(ctx, `SELECT `+reviewSelectColumns+` FROM reviews r
	 LEFT JOIN auth_users ru ON ru.id=r.requested_by
	 LEFT JOIN auth_users du ON du.id=r.decided_by
	 WHERE r.id=$1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return review, err
}

// Create registers a pending review for a project the actor may write, and
// allocates its per-project number under the project row lock so concurrent
// creators never collide. review.request is audited in the same transaction.
func (s *Service) Create(ctx context.Context, actor auth.Actor, projectID string, input Input) (*Review, error) {
	if !auth.ValidID(projectID) {
		return nil, fmt.Errorf("%w: invalid project id", ErrValidation)
	}
	subject, description, version, externalURL, err := normalize(input)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = s.authorizeProjectWrite(ctx, tx, actor, projectID); err != nil {
		return nil, err
	}
	// The project row lock is the numbering serialization point (the
	// build_count/deploy_count pattern); a missing row is a concurrent deletion.
	var count int64
	err = tx.QueryRowContext(ctx, `SELECT review_count FROM projects WHERE id=$1 FOR UPDATE`, projectID).Scan(&count)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	review := &Review{
		ID: id, ProjectID: projectID, Number: count + 1, Subject: subject, Description: description,
		Version: version, ExternalURL: externalURL, Status: StatusPending, RequestedBy: actor.ID,
		RequestedByName: actor.Username, CreatedAt: now, UpdatedAt: now,
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO reviews(id,project_id,number,subject,description,version,external_url,status,requested_by,decided_by,decided_at,reason,created_at,updated_at)
	 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,NULL,NULL,NULL,$10,$10)`,
		review.ID, review.ProjectID, review.Number, review.Subject, review.Description, review.Version,
		review.ExternalURL, review.Status, review.RequestedBy, now); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE projects SET review_count=$2 WHERE id=$1`, projectID, review.Number); err != nil {
		return nil, err
	}
	if err = writeAudit(ctx, tx, actor, ActionRequest, review.ID, fmt.Sprintf("project=%s number=%d", projectID, review.Number)); err != nil {
		return nil, err
	}
	// The notification row commits with the review, so a delivered
	// review.requested always describes a review that exists.
	if err = s.enqueueReviewNotification(ctx, tx, review, EventRequested); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return review, nil
}

// Get returns one review whose project the actor may see; an invisible project
// is a 404, exactly like the project itself.
func (s *Service) Get(ctx context.Context, actor auth.Actor, reviewID string) (*Review, error) {
	if !auth.ValidID(reviewID) {
		return nil, fmt.Errorf("%w: invalid review id", ErrValidation)
	}
	isAdmin, userID := projects.VisibilityArgs(actor)
	review, err := scanReviewWithNames(s.db.QueryRowContext(ctx, `SELECT `+reviewSelectColumns+` FROM reviews r
	 JOIN projects p ON p.id=r.project_id
	 LEFT JOIN auth_users ru ON ru.id=r.requested_by
	 LEFT JOIN auth_users du ON du.id=r.decided_by
	 WHERE r.id=$1 AND `+projects.VisiblePredicate("p", 2, 3), reviewID, isAdmin, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return review, nil
}

// List pages a project's reviews, newest first, filtered by status. The project
// itself must be visible (404 otherwise), and the status filter is limited to the
// four documented values.
func (s *Service) List(ctx context.Context, actor auth.Actor, projectID, status string, limit, offset int) ([]Review, int, error) {
	if !auth.ValidID(projectID) {
		return nil, 0, fmt.Errorf("%w: invalid project id", ErrValidation)
	}
	if status != "" && !validStatus(status) {
		return nil, 0, fmt.Errorf("%w: status filter must be pending, approved, rejected or cancelled", ErrValidation)
	}
	if err := paginate(limit, offset); err != nil {
		return nil, 0, err
	}
	if err := visibleProject(ctx, s.db, actor, projectID); err != nil {
		return nil, 0, err
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM reviews WHERE project_id=$1 AND ($2='' OR status=$2)`,
		projectID, status).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+reviewSelectColumns+` FROM reviews r
	 LEFT JOIN auth_users ru ON ru.id=r.requested_by
	 LEFT JOIN auth_users du ON du.id=r.decided_by
	 WHERE r.project_id=$1 AND ($2='' OR r.status=$2)
	 ORDER BY r.created_at DESC,r.number DESC,r.id DESC LIMIT $3 OFFSET $4`, projectID, status, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	list := make([]Review, 0)
	for rows.Next() {
		review, err := scanReviewWithNames(rows)
		if err != nil {
			return nil, 0, err
		}
		list = append(list, *review)
	}
	return list, total, rows.Err()
}

// ListVisible pages every review whose project the actor may see, newest first,
// filtered by the optional status and project. It reuses the single project
// visibility predicate, so a global admin sees all reviews while every other
// role sees only public projects, projects it created and projects it belongs
// to. A projectId filter cannot widen that set: an invisible project simply
// contributes no rows, matching GET /api/v1/deployments, so the filter cannot be
// used to probe private project ids.
func (s *Service) ListVisible(ctx context.Context, actor auth.Actor, filter ListFilter) ([]Review, int, error) {
	if filter.ProjectID != "" && !auth.ValidID(filter.ProjectID) {
		return nil, 0, fmt.Errorf("%w: invalid project id", ErrValidation)
	}
	if filter.Status != "" && !validStatus(filter.Status) {
		return nil, 0, fmt.Errorf("%w: status filter must be pending, approved, rejected or cancelled", ErrValidation)
	}
	if err := paginate(filter.Limit, filter.Offset); err != nil {
		return nil, 0, err
	}
	isAdmin, userID := projects.VisibilityArgs(actor)
	from := ` FROM reviews r JOIN projects p ON p.id=r.project_id
		WHERE ($1='' OR r.project_id=$1) AND ($2='' OR r.status=$2) AND ` + projects.VisiblePredicate("p", 3, 4)
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*)`+from, filter.ProjectID, filter.Status, isAdmin, userID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+reviewSelectColumns+` FROM reviews r
	 JOIN projects p ON p.id=r.project_id
	 LEFT JOIN auth_users ru ON ru.id=r.requested_by
	 LEFT JOIN auth_users du ON du.id=r.decided_by
	 WHERE ($1='' OR r.project_id=$1) AND ($2='' OR r.status=$2) AND `+projects.VisiblePredicate("p", 3, 4)+`
	 ORDER BY r.created_at DESC,r.number DESC,r.id DESC LIMIT $5 OFFSET $6`,
		filter.ProjectID, filter.Status, isAdmin, userID, filter.Limit, filter.Offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	list := make([]Review, 0)
	for rows.Next() {
		review, err := scanReviewWithNames(rows)
		if err != nil {
			return nil, 0, err
		}
		list = append(list, *review)
	}
	return list, total, rows.Err()
}
