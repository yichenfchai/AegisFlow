// Package projects is the project registry: validation, active/archived
// lifecycle, pagination, and audit rows committed atomically with each
// mutation. Authorization uses global roles only (admin/member write, viewer
// read); per-project membership is deferred by design. The HTTP layer owns
// role checks; this package never depends on Gin.
package projects

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
	"unicode/utf8"

	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
)

var (
	ErrValidation = errors.New("validation failed")
	ErrConflict   = errors.New("conflict")
	ErrNotFound   = errors.New("not found")
)

// Audited project actions, recorded in the shared auth_audit trail.
const (
	ActionCreate    = "project.create"
	ActionUpdate    = "project.update"
	ActionArchive   = "project.archive"
	ActionUnarchive = "project.unarchive"
)

const (
	StatusActive   = "active"
	StatusArchived = "archived"
)

type Project struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	RepoURL       string `json:"repoUrl"`
	DefaultBranch string `json:"defaultBranch"`
	Status        string `json:"status"`
	CreatedBy     string `json:"createdBy"`
	CreatedAt     int64  `json:"createdAt"`
	UpdatedAt     int64  `json:"updatedAt"`
}

type Input struct {
	Name          string
	Description   string
	RepoURL       string
	DefaultBranch string
}

type Patch struct {
	Name          *string
	Description   *string
	RepoURL       *string
	DefaultBranch *string
	Archived      *bool
}

type Service struct {
	db *sql.DB
}

func New(db *sql.DB) *Service { return &Service{db: db} }

const projectColumns = `id,name,description,repo_url,default_branch,status,created_by,created_at,updated_at`

// Source locations serve different deployment scenarios: https or SSH git URLs
// for hosted repositories, absolute local paths for single-node checkout
// deployments. The branch keeps a light git-ref shape check; full ref
// validation happens where refs are actually used (builds).
var (
	repoURLPatterns = []*regexp.Regexp{
		regexp.MustCompile(`^https://[^\s]+$`),
		regexp.MustCompile(`^git@[A-Za-z0-9._-]+:[^\s]+$`),
		regexp.MustCompile(`^file:///[^\s]*$`),
		regexp.MustCompile(`^/[^\s]*$`),
		regexp.MustCompile(`^[A-Za-z]:[\\/][^\s]*$`),
	}
	branchPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$`)
)

func validateName(name string) error {
	if !utf8.ValidString(name) || len(name) < 3 || len(name) > 64 {
		return fmt.Errorf("%w: name must be 3-64 UTF-8 bytes", ErrValidation)
	}
	if strings.TrimSpace(name) != name || name[0] == '.' || name[0] == '-' || name[0] == ' ' {
		return fmt.Errorf("%w: name must not start with a dot, hyphen or space or have surrounding whitespace", ErrValidation)
	}
	if strings.ContainsAny(name, "/\\:?!*\"<>|") {
		return fmt.Errorf("%w: name must not contain path or shell meta characters", ErrValidation)
	}
	return nil
}

func validateText(field, value string, max int) error {
	if !utf8.ValidString(value) || len(value) > max {
		return fmt.Errorf("%w: %s must be valid UTF-8 of at most %d bytes", ErrValidation, field, max)
	}
	return nil
}

func validateRepoURL(repoURL string) error {
	if err := validateText("repoUrl", repoURL, 512); err != nil {
		return err
	}
	if repoURL == "" {
		return nil
	}
	for _, pattern := range repoURLPatterns {
		if pattern.MatchString(repoURL) {
			return nil
		}
	}
	return fmt.Errorf("%w: repoUrl must be an https:// or git@ URL, a file:/// URL, or an absolute local path", ErrValidation)
}

func validateBranch(branch string) error {
	if err := validateText("defaultBranch", branch, 128); err != nil {
		return err
	}
	if branch == "" {
		return nil
	}
	if !branchPattern.MatchString(branch) || strings.Contains(branch, "..") || strings.Contains(branch, "//") || strings.HasSuffix(branch, "/") || strings.HasSuffix(branch, ".lock") || strings.HasSuffix(branch, ".") {
		return fmt.Errorf("%w: defaultBranch must be a plain git ref name", ErrValidation)
	}
	return nil
}

func validateInput(input Input) error {
	if err := validateName(input.Name); err != nil {
		return err
	}
	if err := validateText("description", input.Description, 512); err != nil {
		return err
	}
	if err := validateRepoURL(input.RepoURL); err != nil {
		return err
	}
	return validateBranch(input.DefaultBranch)
}

func newID() (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func writeAudit(ctx context.Context, tx *sql.Tx, actor auth.Actor, action, objectID, detail string) error {
	id, err := newID()
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO auth_audit(id,created_at,actor_id,actor_username,action,object_type,object_id,result,detail,request_id)
	 VALUES($1,$2,$3,$4,$5,'project',$6,'success',$7,$8)`, id, time.Now().Unix(), actor.ID, actor.Username, action, objectID, detail, actor.RequestID)
	return err
}

type rowScanner interface{ Scan(dest ...any) error }

func scanProject(row rowScanner) (*Project, error) {
	var p Project
	if err := row.Scan(&p.ID, &p.Name, &p.Description, &p.RepoURL, &p.DefaultBranch, &p.Status, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	return &p, nil
}

// Create registers a new active project; names are unique case-insensitively.
func (s *Service) Create(ctx context.Context, actor auth.Actor, input Input) (*Project, error) {
	if err := validateInput(input); err != nil {
		return nil, err
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	project := &Project{ID: id, Name: input.Name, Description: input.Description, RepoURL: input.RepoURL, DefaultBranch: input.DefaultBranch, Status: StatusActive, CreatedBy: actor.ID, CreatedAt: now, UpdatedAt: now}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO projects(id,name,description,repo_url,default_branch,status,created_by,created_at,updated_at)
	 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$8) ON CONFLICT (lower(name)) DO NOTHING`, id, input.Name, input.Description, input.RepoURL, input.DefaultBranch, StatusActive, actor.ID, now)
	if err != nil {
		return nil, err
	}
	if n, err := result.RowsAffected(); err != nil {
		return nil, err
	} else if n == 0 {
		return nil, fmt.Errorf("%w: project name already exists", ErrConflict)
	}
	if err = writeAudit(ctx, tx, actor, ActionCreate, id, input.Name); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return project, nil
}

// Get returns one project by id.
func (s *Service) Get(ctx context.Context, id string) (*Project, error) {
	if !auth.ValidID(id) {
		return nil, fmt.Errorf("%w: invalid project id", ErrValidation)
	}
	project, err := scanProject(s.db.QueryRowContext(ctx, `SELECT `+projectColumns+` FROM projects WHERE id=$1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return project, nil
}

// List pages projects ordered by name; status must be empty, active or archived.
func (s *Service) List(ctx context.Context, status string, limit, offset int) ([]Project, int, error) {
	if status != "" && status != StatusActive && status != StatusArchived {
		return nil, 0, fmt.Errorf("%w: status filter must be active or archived", ErrValidation)
	}
	if limit < 1 || limit > 100 || offset < 0 {
		return nil, 0, fmt.Errorf("%w: limit must be 1-100 and offset non-negative", ErrValidation)
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects WHERE ($1='' OR status=$1)`, status).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+projectColumns+` FROM projects WHERE ($1='' OR status=$1) ORDER BY lower(name),id LIMIT $2 OFFSET $3`, status, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	projects := make([]Project, 0)
	for rows.Next() {
		project, err := scanProject(rows)
		if err != nil {
			return nil, 0, err
		}
		projects = append(projects, *project)
	}
	return projects, total, rows.Err()
}

// Update applies a partial update. Archived projects are read-only except for
// unarchiving; archive/unarchive and field edits commit in one transaction
// with their audit rows.
func (s *Service) Update(ctx context.Context, actor auth.Actor, id string, patch Patch) (*Project, error) {
	if !auth.ValidID(id) {
		return nil, fmt.Errorf("%w: invalid project id", ErrValidation)
	}
	if patch.Name == nil && patch.Description == nil && patch.RepoURL == nil && patch.DefaultBranch == nil && patch.Archived == nil {
		return nil, fmt.Errorf("%w: at least one field is required", ErrValidation)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	current, err := scanProject(tx.QueryRowContext(ctx, `SELECT `+projectColumns+` FROM projects WHERE id=$1 FOR UPDATE`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	updated := *current
	fieldChange := false
	if patch.Name != nil {
		updated.Name = *patch.Name
		fieldChange = true
	}
	if patch.Description != nil {
		updated.Description = *patch.Description
		fieldChange = true
	}
	if patch.RepoURL != nil {
		updated.RepoURL = *patch.RepoURL
		fieldChange = true
	}
	if patch.DefaultBranch != nil {
		updated.DefaultBranch = *patch.DefaultBranch
		fieldChange = true
	}
	// An archived project is read-only; only restoring it (or repeating the
	// archive no-op) is accepted.
	if current.Status == StatusArchived && fieldChange {
		return nil, fmt.Errorf("%w: archived project is read-only until restored", ErrConflict)
	}
	if fieldChange {
		if err = validateInput(Input{Name: updated.Name, Description: updated.Description, RepoURL: updated.RepoURL, DefaultBranch: updated.DefaultBranch}); err != nil {
			return nil, err
		}
	}
	archiveChanged := false
	if patch.Archived != nil {
		want := StatusActive
		if *patch.Archived {
			want = StatusArchived
		}
		if want != current.Status {
			archiveChanged = true
			updated.Status = want
		}
	}
	if !fieldChange && !archiveChanged {
		return current, nil
	}
	if fieldChange && updated.Name != current.Name {
		var conflict int
		if err = tx.QueryRowContext(ctx, `SELECT 1 FROM projects WHERE lower(name)=lower($1) AND id<>$2`, updated.Name, id).Scan(&conflict); errors.Is(err, sql.ErrNoRows) {
			err = nil
		}
		if err != nil {
			return nil, err
		}
		if conflict == 1 {
			return nil, fmt.Errorf("%w: project name already exists", ErrConflict)
		}
	}
	updated.UpdatedAt = time.Now().Unix()
	if _, err = tx.ExecContext(ctx, `UPDATE projects SET name=$2,description=$3,repo_url=$4,default_branch=$5,status=$6,updated_at=$7 WHERE id=$1`,
		id, updated.Name, updated.Description, updated.RepoURL, updated.DefaultBranch, updated.Status, updated.UpdatedAt); err != nil {
		return nil, err
	}
	action, detail := ActionUpdate, ""
	switch {
	case archiveChanged && updated.Status == StatusArchived:
		action = ActionArchive
	case archiveChanged:
		action = ActionUnarchive
	}
	if action == ActionUpdate {
		detail = "fields updated"
	}
	if err = writeAudit(ctx, tx, actor, action, id, detail); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &updated, nil
}
