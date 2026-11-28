// Package securityevents is the defense-event registry (P5 remaining modules,
// batch A). External detection tools deliver one defense event through the
// signed HMAC webhook; AegisFlow never runs a detection engine itself. An event
// is global evidence (defense is not project-scoped): it starts open and an
// administrator acknowledges it, which is audited in the same transaction as the
// state change. Reads and acknowledgement are admin-only; the HTTP layer owns
// the signature check, the admin middleware and the error envelope. The package
// depends on context.Context, never on Gin.
package securityevents

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
)

var (
	// ErrValidation is the shared ingest validation sentinel: every payload or
	// filter failure maps to one HTTP 400 in the transport layer.
	ErrValidation = ingest.ErrInvalid
	ErrNotFound   = errors.New("not found")
	// ErrConflict means the stored delivery row disappeared between the insert
	// and the retry read (operator cleanup raced the webhook).
	ErrConflict = errors.New("conflict")
	// ErrForbidden is returned to a non-admin caller as defense in depth: the
	// HTTP routes also require the admin role.
	ErrForbidden = errors.New("forbidden")
)

// Event severities accepted from an external detection source.
const (
	SeverityLow      = "low"
	SeverityMedium   = "medium"
	SeverityHigh     = "high"
	SeverityCritical = "critical"
)

// Event statuses. Every event starts open; only an admin acknowledgement moves
// it to acknowledged, and acknowledged events never change again.
const (
	StatusOpen         = "open"
	StatusAcknowledged = "acknowledged"
)

// Audited defense action, recorded in the shared auth_audit trail. The detail
// only carries the event id, severity and type; the summary text is never copied
// into the audit trail.
const (
	ActionAck = "security_event.ack"
	// ObjectType is the audit object type of the acknowledgement.
	ObjectType = "security_event"

	globalAdminRole = "admin"
)

const (
	maxSourceBytes    = 64
	maxEventTypeBytes = 64
	// maxSummaryBytes bounds the free-text event note; it may be multi-line.
	maxSummaryBytes = 1000
	// maxOccurredFutureSkew is how far a source clock may run ahead of the
	// server. A future occurrence is rejected instead of trusted, so a sender
	// cannot push an event out of the ordering window.
	maxOccurredFutureSkew = 300 * time.Second
)

// Event is one defense event delivered by an external detection tool.
// AcknowledgedBy/AcknowledgedByName/AcknowledgedAt only appear once an admin
// acknowledged it; an open event omits them. DetailsURL is omitted when absent.
type Event struct {
	ID                 string  `json:"id"`
	Source             string  `json:"source"`
	Severity           string  `json:"severity"`
	EventType          string  `json:"eventType"`
	Summary            string  `json:"summary"`
	Status             string  `json:"status"`
	DetailsURL         string  `json:"detailsUrl,omitempty"`
	OccurredAt         int64   `json:"occurredAt"`
	ReceivedAt         int64   `json:"receivedAt"`
	AcknowledgedBy     *string `json:"acknowledgedBy,omitempty"`
	AcknowledgedByName *string `json:"acknowledgedByName,omitempty"`
	AcknowledgedAt     *int64  `json:"acknowledgedAt,omitempty"`
}

// IngestInput is the validated inbound payload. DeliveryID and ReceivedAt are
// supplied by the transport/database, not by the JSON body. OccurredAt is 0 when
// the delivery omitted it, which means "now" at ingestion time.
type IngestInput struct {
	Source     string
	Severity   string
	EventType  string
	Summary    string
	OccurredAt int64
	DetailsURL string
}

// ListFilter is the admin defense-event list filter. Severity, Status and Source
// are optional; Limit/Offset use the shared 1-100/non-negative pagination bounds.
type ListFilter struct {
	Severity string
	Status   string
	Source   string
	Limit    int
	Offset   int
}

type Service struct {
	db *sql.DB
}

func New(db *sql.DB) *Service { return &Service{db: db} }

// eventColumns is shared by every read; details_url/acknowledged_* may be NULL
// in the database and the acknowledged username is resolved for display.
const eventColumns = `e.id,e.source,e.severity,e.event_type,e.summary,e.status,e.details_url,
	e.occurred_at,e.received_at,e.acknowledged_by,COALESCE(au.username,''),e.acknowledged_at`

type rowScanner interface{ Scan(dest ...any) error }

func scanEvent(row rowScanner) (*Event, error) {
	var event Event
	var detailsURL, acknowledgedBy, acknowledgedByName sql.NullString
	var acknowledgedAt sql.NullInt64
	if err := row.Scan(&event.ID, &event.Source, &event.Severity, &event.EventType, &event.Summary,
		&event.Status, &detailsURL, &event.OccurredAt, &event.ReceivedAt, &acknowledgedBy,
		&acknowledgedByName, &acknowledgedAt); err != nil {
		return nil, err
	}
	event.DetailsURL = detailsURL.String
	if acknowledgedBy.Valid {
		event.AcknowledgedBy = &acknowledgedBy.String
		name := acknowledgedByName.String
		event.AcknowledgedByName = &name
	}
	if acknowledgedAt.Valid {
		event.AcknowledgedAt = &acknowledgedAt.Int64
	}
	return &event, nil
}

func newID() (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func validSeverity(severity string) bool {
	switch severity {
	case SeverityLow, SeverityMedium, SeverityHigh, SeverityCritical:
		return true
	default:
		return false
	}
}

func validStatus(status string) bool {
	return status == StatusOpen || status == StatusAcknowledged
}

// normalize validates the inbound payload and returns the stored values. source
// and eventType are required trimmed single lines; summary is required and may
// be multi-line, keeping its newlines; detailsUrl is an optional display-only
// http(s) link the server never fetches.
func normalize(input IngestInput, now time.Time) (IngestInput, error) {
	source, err := ingest.BoundedSingleLine("source", input.Source, maxSourceBytes, false)
	if err != nil {
		return IngestInput{}, err
	}
	eventType, err := ingest.BoundedSingleLine("eventType", input.EventType, maxEventTypeBytes, false)
	if err != nil {
		return IngestInput{}, err
	}
	if !validSeverity(input.Severity) {
		return IngestInput{}, fmt.Errorf("%w: severity must be low, medium, high or critical", ErrValidation)
	}
	summary, err := ingest.SanitizeText(input.Summary, maxSummaryBytes)
	if err != nil {
		return IngestInput{}, fmt.Errorf("%w: summary must be valid UTF-8 of at most %d bytes", ErrValidation, maxSummaryBytes)
	}
	if summary == "" {
		return IngestInput{}, fmt.Errorf("%w: summary must be 1-%d UTF-8 bytes", ErrValidation, maxSummaryBytes)
	}
	detailsURL := strings.TrimSpace(input.DetailsURL)
	if err = ingest.ValidateURL("detailsUrl", detailsURL); err != nil {
		return IngestInput{}, err
	}
	occurredAt := input.OccurredAt
	if occurredAt == 0 {
		occurredAt = now.Unix()
	} else if occurredAt < 0 || occurredAt > now.Unix()+int64(maxOccurredFutureSkew/time.Second) {
		return IngestInput{}, fmt.Errorf("%w: occurredAt must be a Unix timestamp not more than %d seconds in the future",
			ErrValidation, int64(maxOccurredFutureSkew/time.Second))
	}
	return IngestInput{Source: source, Severity: input.Severity, EventType: eventType, Summary: summary,
		OccurredAt: occurredAt, DetailsURL: detailsURL}, nil
}

// validateFilter checks the optional list filters and pagination so the service
// contract holds even for a non-HTTP caller.
func validateFilter(filter ListFilter) (ListFilter, error) {
	if filter.Severity != "" && !validSeverity(filter.Severity) {
		return filter, fmt.Errorf("%w: severity filter must be low, medium, high or critical", ErrValidation)
	}
	if filter.Status != "" && !validStatus(filter.Status) {
		return filter, fmt.Errorf("%w: status filter must be open or acknowledged", ErrValidation)
	}
	source, err := ingest.BoundedSingleLine("source", filter.Source, maxSourceBytes, true)
	if err != nil {
		return filter, err
	}
	filter.Source = source
	if filter.Limit < 1 || filter.Limit > 100 || filter.Offset < 0 {
		return filter, fmt.Errorf("%w: limit must be 1-100 and offset non-negative", ErrValidation)
	}
	return filter, nil
}

// requireAdmin reports whether the actor may read or acknowledge defense
// events. Event ids are never exposed to non-admins, so the check is a plain
// forbidden rather than a non-enumerable not found.
func requireAdmin(actor auth.Actor) error {
	if actor.Role != globalAdminRole {
		return fmt.Errorf("%w: defense events are admin-only", ErrForbidden)
	}
	return nil
}

// Ingest stores one signed defense-event delivery. The delivery id is the
// idempotency key: a repeated delivery returns the stored row and created=false,
// so a retried webhook never duplicates evidence even across replicas. Every
// payload failure is ErrValidation.
func (s *Service) Ingest(ctx context.Context, deliveryID string, input IngestInput) (*Event, bool, error) {
	if err := ingest.ValidDeliveryID(deliveryID); err != nil {
		return nil, false, err
	}
	now := time.Now()
	normalized, err := normalize(input, now)
	if err != nil {
		return nil, false, err
	}
	id, err := newID()
	if err != nil {
		return nil, false, err
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO security_events
		(id,delivery_id,source,severity,event_type,summary,details_url,status,occurred_at,received_at,
		 acknowledged_by,acknowledged_at)
		VALUES($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8,$9,$10,NULL,NULL)
		ON CONFLICT (delivery_id) DO NOTHING`,
		id, deliveryID, normalized.Source, normalized.Severity, normalized.EventType, normalized.Summary,
		normalized.DetailsURL, StatusOpen, normalized.OccurredAt, now.Unix())
	if err != nil {
		return nil, false, err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return nil, false, err
	}
	// Read the stored row either way: a new delivery returns the row it just
	// created, while a repeated delivery returns the original row unchanged (200)
	// so the sender's retry is idempotent.
	event, err := s.getByDeliveryID(ctx, deliveryID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, fmt.Errorf("%w: delivery %q was removed concurrently", ErrConflict, deliveryID)
	}
	if err != nil {
		return nil, false, err
	}
	return event, inserted == 1, nil
}

func (s *Service) getByDeliveryID(ctx context.Context, deliveryID string) (*Event, error) {
	return scanEvent(s.db.QueryRowContext(ctx, `SELECT `+eventColumns+` FROM security_events e
		LEFT JOIN auth_users au ON au.id=e.acknowledged_by WHERE e.delivery_id=$1`, deliveryID))
}

// getEventTx reads one event with its acknowledging username inside the
// caller's transaction.
func getEventTx(ctx context.Context, tx *sql.Tx, id string) (*Event, error) {
	event, err := scanEvent(tx.QueryRowContext(ctx, `SELECT `+eventColumns+` FROM security_events e
		LEFT JOIN auth_users au ON au.id=e.acknowledged_by WHERE e.id=$1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return event, err
}

// writeAudit inserts one defense-event audit row inside the caller's transaction
// so the acknowledgement and its evidence commit together. detail never contains
// the event summary: only the id, severity and event type.
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

// List pages defense events, newest first, with optional severity/status/source
// filters. The HTTP route is admin-only; requireAdmin repeats the check so a
// direct service caller cannot read defense evidence either.
func (s *Service) List(ctx context.Context, actor auth.Actor, filter ListFilter) ([]Event, int, error) {
	if err := requireAdmin(actor); err != nil {
		return nil, 0, err
	}
	normalized, err := validateFilter(filter)
	if err != nil {
		return nil, 0, err
	}
	from := ` FROM security_events e
		WHERE ($1='' OR e.severity=$1) AND ($2='' OR e.status=$2) AND ($3='' OR e.source=$3)`
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*)`+from,
		normalized.Severity, normalized.Status, normalized.Source).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+eventColumns+` FROM security_events e
		LEFT JOIN auth_users au ON au.id=e.acknowledged_by
		WHERE ($1='' OR e.severity=$1) AND ($2='' OR e.status=$2) AND ($3='' OR e.source=$3)
		ORDER BY e.occurred_at DESC,e.id DESC LIMIT $4 OFFSET $5`,
		normalized.Severity, normalized.Status, normalized.Source, normalized.Limit, normalized.Offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	list := make([]Event, 0)
	for rows.Next() {
		event, scanErr := scanEvent(rows)
		if scanErr != nil {
			return nil, 0, scanErr
		}
		list = append(list, *event)
	}
	return list, total, rows.Err()
}

// Get returns one defense event by id. Admin-only.
func (s *Service) Get(ctx context.Context, actor auth.Actor, eventID string) (*Event, error) {
	if err := requireAdmin(actor); err != nil {
		return nil, err
	}
	if !auth.ValidID(eventID) {
		return nil, fmt.Errorf("%w: invalid event id", ErrValidation)
	}
	event, err := scanEvent(s.db.QueryRowContext(ctx, `SELECT `+eventColumns+` FROM security_events e
		LEFT JOIN auth_users au ON au.id=e.acknowledged_by WHERE e.id=$1`, eventID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return event, nil
}

// Ack moves an open event to acknowledged with the current admin as the
// acknowledger, in one transaction with a security_event.ack audit row. A
// repeated acknowledgement is idempotent: the stored event is returned with no
// second audit row. An unknown event is ErrNotFound.
func (s *Service) Ack(ctx context.Context, actor auth.Actor, eventID string) (*Event, error) {
	if err := requireAdmin(actor); err != nil {
		return nil, err
	}
	if !auth.ValidID(eventID) {
		return nil, fmt.Errorf("%w: invalid event id", ErrValidation)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Lock the row: concurrent acknowledgements serialize, so only the first
	// transition writes the audit row.
	var severity, eventType, status string
	err = tx.QueryRowContext(ctx, `SELECT severity,event_type,status FROM security_events WHERE id=$1 FOR UPDATE`,
		eventID).Scan(&severity, &eventType, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if status == StatusAcknowledged {
		// Idempotent repeat: return the stored event unchanged, no audit row.
		return getEventTx(ctx, tx, eventID)
	}
	now := time.Now().Unix()
	if _, err = tx.ExecContext(ctx, `UPDATE security_events SET status=$2,acknowledged_by=$3,acknowledged_at=$4 WHERE id=$1`,
		eventID, StatusAcknowledged, actor.ID, now); err != nil {
		return nil, err
	}
	event, err := getEventTx(ctx, tx, eventID)
	if err != nil {
		return nil, err
	}
	detail := fmt.Sprintf("id=%s severity=%s eventType=%s", event.ID, event.Severity, event.EventType)
	if err = writeAudit(ctx, tx, actor, ActionAck, event.ID, detail); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return event, nil
}
