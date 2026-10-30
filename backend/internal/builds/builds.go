// Package builds is the build pipeline: enqueue, claim, execute in a
// temporary working directory with a timeout, capture logs, and expose
// polling reads. Business logic never depends on Gin.
//
// P3 first-slice boundaries, deliberately: commands run through the system
// shell without a sandbox (no dedicated execution identity, resource caps, or
// output redaction yet — that arrives with the controlled-environment batch).
// The worker runs in-process with concurrency one and is guarded by a
// PostgreSQL session advisory lock (RunWorker): only the lease holder recovers
// interrupted builds and claims queued ones, so standby replicas never turn a
// peer's running build into interrupted. Claiming also uses SELECT ... FOR
// UPDATE SKIP LOCKED as a second line of defence.
package builds

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
	"github.com/yichenfchai/AegisFlow/backend/internal/projects"
)

var (
	ErrValidation = errors.New("validation failed")
	ErrConflict   = errors.New("conflict")
	ErrNotFound   = errors.New("not found")
)

const (
	StatusQueued      = "queued"
	StatusRunning     = "running"
	StatusSucceeded   = "succeeded"
	StatusFailed      = "failed"
	StatusCancelled   = "cancelled"
	StatusInterrupted = "interrupted"
)

// Terminal reports whether a status is final, so streaming readers know when a
// build can no longer change.
func Terminal(status string) bool {
	switch status {
	case StatusSucceeded, StatusFailed, StatusCancelled, StatusInterrupted:
		return true
	default:
		return false
	}
}

type Build struct {
	ID          string `json:"id"`
	ProjectID   string `json:"projectId"`
	BuildNumber int64  `json:"buildNumber"`
	Status      string `json:"status"`
	Branch      string `json:"branch"`
	Command     string `json:"command"`
	TriggeredBy string `json:"triggeredBy"`
	CreatedAt   int64  `json:"createdAt"`
	StartedAt   int64  `json:"startedAt"`
	FinishedAt  int64  `json:"finishedAt"`
}

type LogEvent struct {
	ID        int64  `json:"id"`
	BuildID   string `json:"buildId"`
	Stream    string `json:"stream"`
	Message   string `json:"message"`
	CreatedAt int64  `json:"createdAt"`
}

const (
	// Log lines longer than this are byte-truncated (runes split safely).
	maxLineBytes = 4096
	// A build stops recording output past this many events; one system line
	// marks the truncation.
	maxLogEvents = 5000
	// Upper bound of a single logs response.
	maxLogPageSize = 1000
)

type Service struct {
	db         *sql.DB
	projects   *projects.Service
	timeout    time.Duration
	maxEvents  int
	notify     chan struct{}
	leaseRetry time.Duration
	sandbox    Sandbox
}

// New uses the default execution timeout of ten minutes.
func New(db *sql.DB, projectRegistry *projects.Service) *Service {
	return newWithLimits(db, projectRegistry, 10*time.Minute, maxLogEvents)
}

func newWithLimits(db *sql.DB, projectRegistry *projects.Service, timeout time.Duration, maxEvents int) *Service {
	return &Service{db: db, projects: projectRegistry, timeout: timeout, maxEvents: maxEvents, notify: make(chan struct{}, 1), leaseRetry: defaultLeaseRetry}
}

const buildColumns = `id,project_id,build_number,status,branch,command,triggered_by,created_at,started_at,finished_at`

// buildColumnsQualified is buildColumns with the table alias needed by the
// visibility join: reads embed projects.VisiblePredicate, so a build of an
// invisible project is not found rather than forbidden.
const buildColumnsQualified = `b.id,b.project_id,b.build_number,b.status,b.branch,b.command,b.triggered_by,b.created_at,b.started_at,b.finished_at`

type rowScanner interface{ Scan(dest ...any) error }

func newID() (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// writeAudit records a build operation in the shared audit trail inside the
// caller's transaction, so the state change and its audit row commit together.
// The detail never contains the command: it may hold secrets from the project.
func writeAudit(ctx context.Context, tx *sql.Tx, actor auth.Actor, action, objectID, detail string) error {
	id, err := newID()
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO auth_audit(id,created_at,actor_id,actor_username,action,object_type,object_id,result,detail,request_id)
	 VALUES($1,$2,$3,$4,$5,'build',$6,'success',$7,$8)`, id, time.Now().Unix(), actor.ID, actor.Username, action, objectID, detail, actor.RequestID)
	return err
}

func scanBuild(row rowScanner) (*Build, error) {
	var b Build
	if err := row.Scan(&b.ID, &b.ProjectID, &b.BuildNumber, &b.Status, &b.Branch, &b.Command, &b.TriggeredBy, &b.CreatedAt, &b.StartedAt, &b.FinishedAt); err != nil {
		return nil, err
	}
	return &b, nil
}

// Trigger enqueues a build. The project must exist, be active, and carry a
// build command; the row lock serializes the per-project number sequence.
func (s *Service) Trigger(ctx context.Context, actor auth.Actor, projectID, branch string) (*Build, error) {
	return s.enqueue(ctx, actor, projectID, branch, "build.trigger", "")
}

// Retry enqueues a new build for the branch of an existing terminal build. The
// command comes from the project's current configuration, so a retry after
// fixing the command builds the fix; the original row stays as the historical
// record of what ran. Queued and running builds cannot be retried (409).
func (s *Service) Retry(ctx context.Context, actor auth.Actor, id string) (*Build, error) {
	if !auth.ValidID(id) {
		return nil, fmt.Errorf("%w: invalid build id", ErrValidation)
	}
	original, err := s.Get(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	if !Terminal(original.Status) {
		return nil, fmt.Errorf("%w: only terminal builds can be retried", ErrConflict)
	}
	return s.enqueue(ctx, actor, original.ProjectID, original.Branch, "build.retry", fmt.Sprintf(" retry_of=%d", original.BuildNumber))
}

// enqueue inserts a queued build and commits its audit row in the same
// transaction. action distinguishes a fresh trigger from a retry; detailSuffix
// carries the retry provenance and never contains the command.
func (s *Service) enqueue(ctx context.Context, actor auth.Actor, projectID, branch, action, detailSuffix string) (*Build, error) {
	if !auth.ValidID(projectID) {
		return nil, fmt.Errorf("%w: invalid project id", ErrValidation)
	}
	if len(branch) > 128 {
		return nil, fmt.Errorf("%w: branch too long", ErrValidation)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var status, command, defaultBranch string
	var count int64
	isAdmin, userID := projects.VisibilityArgs(actor)
	// The visibility predicate runs inside the locking read: triggering a build
	// on an invisible project is a 404, exactly like reading it.
	err = tx.QueryRowContext(ctx, `SELECT p.status,p.build_command,p.default_branch,p.build_count FROM projects p
	 WHERE p.id=$1 AND `+projects.VisiblePredicate("p", 2, 3)+` FOR UPDATE`, projectID, isAdmin, userID).Scan(&status, &command, &defaultBranch, &count)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if status != projects.StatusActive {
		return nil, fmt.Errorf("%w: archived project cannot start builds", ErrConflict)
	}
	// Visibility decided whether the row was found; the maintainer overlay then
	// decides whether this actor may start work on the project.
	if err = s.projects.AuthorizeWrite(ctx, tx, actor, projectID); err != nil {
		return nil, err
	}
	if command == "" {
		return nil, fmt.Errorf("%w: project has no build command configured", ErrValidation)
	}
	if branch == "" {
		branch = defaultBranch
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	build := &Build{ID: id, ProjectID: projectID, BuildNumber: count + 1, Status: StatusQueued, Branch: branch, Command: command, TriggeredBy: actor.ID, CreatedAt: now}
	if _, err = tx.ExecContext(ctx, `INSERT INTO builds(id,project_id,build_number,status,branch,command,triggered_by,created_at)
	 VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, id, projectID, build.BuildNumber, StatusQueued, branch, command, actor.ID, now); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE projects SET build_count=$2 WHERE id=$1`, projectID, build.BuildNumber); err != nil {
		return nil, err
	}
	if err = writeAudit(ctx, tx, actor, action, id, fmt.Sprintf("project=%s build=%d%s", projectID, build.BuildNumber, detailSuffix)); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	s.wake()
	return build, nil
}

// wake nudges the worker loop without blocking the request path.
func (s *Service) wake() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// getByID is the internal, visibility-free read used by the worker's status
// watchdog: the worker must observe cancellation and shutdown regardless of who
// triggered the build. Request paths always go through Get.
func (s *Service) getByID(ctx context.Context, id string) (*Build, error) {
	build, err := scanBuild(s.db.QueryRowContext(ctx, `SELECT `+buildColumns+` FROM builds WHERE id=$1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return build, nil
}

// Get returns one build the actor may see. A build whose project is invisible
// to the actor is reported as ErrNotFound (404) to avoid enumeration.
func (s *Service) Get(ctx context.Context, actor auth.Actor, id string) (*Build, error) {
	if !auth.ValidID(id) {
		return nil, fmt.Errorf("%w: invalid build id", ErrValidation)
	}
	isAdmin, userID := projects.VisibilityArgs(actor)
	build, err := scanBuild(s.db.QueryRowContext(ctx, `SELECT `+buildColumnsQualified+` FROM builds b
	 JOIN projects p ON p.id=b.project_id
	 WHERE b.id=$1 AND `+projects.VisiblePredicate("p", 2, 3), id, isAdmin, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return build, nil
}

// List pages the builds of projects the actor may see, newest first; projectID
// may be empty for all visible projects.
func (s *Service) List(ctx context.Context, actor auth.Actor, projectID string, limit, offset int) ([]Build, int, error) {
	if projectID != "" && !auth.ValidID(projectID) {
		return nil, 0, fmt.Errorf("%w: invalid project id", ErrValidation)
	}
	if limit < 1 || limit > 100 || offset < 0 {
		return nil, 0, fmt.Errorf("%w: limit must be 1-100 and offset non-negative", ErrValidation)
	}
	isAdmin, userID := projects.VisibilityArgs(actor)
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM builds b JOIN projects p ON p.id=b.project_id
	 WHERE ($1='' OR b.project_id=$1) AND `+projects.VisiblePredicate("p", 2, 3), projectID, isAdmin, userID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+buildColumnsQualified+` FROM builds b
	 JOIN projects p ON p.id=b.project_id
	 WHERE ($1='' OR b.project_id=$1) AND `+projects.VisiblePredicate("p", 4, 5)+`
	 ORDER BY b.created_at DESC,b.id DESC LIMIT $2 OFFSET $3`, projectID, limit, offset, isAdmin, userID)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	list := make([]Build, 0)
	for rows.Next() {
		build, err := scanBuild(rows)
		if err != nil {
			return nil, 0, err
		}
		list = append(list, *build)
	}
	return list, total, rows.Err()
}

// Logs returns events after the exclusive cursor, oldest first. The cursor is
// the last event id the client saw; it doubles as the SSE resume sequence. The
// project join keeps the SSE fallback channel behind the same visibility rule.
func (s *Service) Logs(ctx context.Context, actor auth.Actor, buildID string, after int64, limit int) ([]LogEvent, error) {
	if !auth.ValidID(buildID) {
		return nil, fmt.Errorf("%w: invalid build id", ErrValidation)
	}
	if after < 0 {
		return nil, fmt.Errorf("%w: after must be non-negative", ErrValidation)
	}
	if limit < 1 || limit > maxLogPageSize {
		limit = 200
	}
	// Resolve the build first so an invisible project yields ErrNotFound rather
	// than an empty page the client cannot distinguish from "no output yet".
	if _, err := s.Get(ctx, actor, buildID); err != nil {
		return nil, err
	}
	isAdmin, userID := projects.VisibilityArgs(actor)
	rows, err := s.db.QueryContext(ctx, `SELECT e.id,e.build_id,e.stream,e.message,e.created_at FROM build_log_events e
	 JOIN builds b ON b.id=e.build_id
	 JOIN projects p ON p.id=b.project_id
	 WHERE e.build_id=$1 AND e.id>$2 AND `+projects.VisiblePredicate("p", 4, 5)+`
	 ORDER BY e.id LIMIT $3`, buildID, after, limit, isAdmin, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := make([]LogEvent, 0)
	for rows.Next() {
		var e LogEvent
		if err = rows.Scan(&e.ID, &e.BuildID, &e.Stream, &e.Message, &e.CreatedAt); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

// Cancel moves a queued or running build to cancelled. The executing worker
// notices through its status watchdog and terminates the process. Repeating on
// an already-cancelled build is idempotent and writes no second audit row.
func (s *Service) Cancel(ctx context.Context, actor auth.Actor, id string) (*Build, error) {
	if !auth.ValidID(id) {
		return nil, fmt.Errorf("%w: invalid build id", ErrValidation)
	}
	// Resolve the build first: this applies visibility (404) and yields the
	// project whose maintainer overlay then decides whether cancelling is
	// allowed (403).
	current, err := s.Get(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	if err = s.projects.AuthorizeWrite(ctx, s.db, actor, current.ProjectID); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	build, err := scanBuild(tx.QueryRowContext(ctx, `UPDATE builds SET status=$2, finished_at=CASE WHEN finished_at=0 THEN $3 ELSE finished_at END
	 WHERE id=$1 AND status IN ($4,$5) RETURNING `+buildColumns, id, StatusCancelled, time.Now().Unix(), StatusQueued, StatusRunning))
	if errors.Is(err, sql.ErrNoRows) {
		// Distinguish "already cancelled" from "finished another way"; the re-read
		// also keeps the visibility check on the fallback path.
		fresh, freshErr := s.Get(ctx, actor, id)
		if freshErr != nil {
			return nil, freshErr
		}
		if fresh.Status == StatusCancelled {
			return fresh, nil
		}
		return nil, fmt.Errorf("%w: only queued or running builds can be cancelled", ErrConflict)
	}
	if err != nil {
		return nil, err
	}
	if err = writeAudit(ctx, tx, actor, "build.cancel", build.ID, fmt.Sprintf("project=%s build=%d", build.ProjectID, build.BuildNumber)); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return build, nil
}

// RecoverInterrupted marks running builds interrupted. Call once at startup
// before the worker starts; correct for single-instance deployments.
func (s *Service) RecoverInterrupted(ctx context.Context) (int64, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE builds SET status=$1, finished_at=$2 WHERE status=$3`, StatusInterrupted, time.Now().Unix(), StatusRunning)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
