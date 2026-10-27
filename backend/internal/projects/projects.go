// Package projects is the project registry: validation, active/archived
// lifecycle, visibility and membership, pagination, and audit rows committed
// atomically with each mutation. Reads are filtered by project visibility: a
// private project is invisible (404, never 403) to anyone who is not a global
// admin, its creator, or a member. Writes are still bounded by the caller's
// global role; the project-maintainer overlay lands with the next slice of the
// permissions design. The HTTP layer owns role checks; this package never
// depends on Gin.
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
	// ErrForbidden means the caller may see the project but not modify it: the
	// global role is a ceiling and a private project additionally requires the
	// caller to be one of its maintainers.
	ErrForbidden = errors.New("forbidden")
)

// Audited project actions, recorded in the shared auth_audit trail.
const (
	ActionCreate    = "project.create"
	ActionUpdate    = "project.update"
	ActionArchive   = "project.archive"
	ActionUnarchive = "project.unarchive"
	// Membership changes are audited on the project object, inside the same
	// transaction as the membership row.
	ActionMemberAdd    = "project.member_add"
	ActionMemberUpdate = "project.member_update"
	ActionMemberRemove = "project.member_remove"
)

const (
	StatusActive   = "active"
	StatusArchived = "archived"
)

const (
	// VisibilityPublic is the default: every authenticated role can read the
	// project, exactly like the behaviour before membership existed.
	VisibilityPublic = "public"
	// VisibilityPrivate restricts reads to global admins, the creator and
	// project members.
	VisibilityPrivate = "private"
)

const (
	// RoleMaintainer can read the project and, once the write overlay lands,
	// modify it and its builds.
	RoleMaintainer = "maintainer"
	// RoleViewer can only read the project, its builds and its logs.
	RoleViewer = "viewer"
)

// globalAdminRole is the role that is never subject to project visibility or
// membership; it mirrors the auth package's role vocabulary.
const globalAdminRole = "admin"

// globalMemberRole is the only non-admin role that may write at all; whether it
// may write a specific project is decided by AuthorizeWrite.
const globalMemberRole = "member"

type Project struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	RepoURL       string `json:"repoUrl"`
	DefaultBranch string `json:"defaultBranch"`
	BuildCommand  string `json:"buildCommand"`
	Status        string `json:"status"`
	Visibility    string `json:"visibility"`
	CreatedBy     string `json:"createdBy"`
	CreatedAt     int64  `json:"createdAt"`
	UpdatedAt     int64  `json:"updatedAt"`
}

// Member is one project membership row. Username is resolved from auth_users
// for display; userID is the stable identity used by the API.
type Member struct {
	ProjectID string `json:"projectId"`
	UserID    string `json:"userId"`
	Username  string `json:"username"`
	Role      string `json:"role"`
	CreatedBy string `json:"createdBy"`
	CreatedAt int64  `json:"createdAt"`
}

type Input struct {
	Name          string
	Description   string
	RepoURL       string
	DefaultBranch string
	BuildCommand  string
	Visibility    string
}

type Patch struct {
	Name          *string
	Description   *string
	RepoURL       *string
	DefaultBranch *string
	BuildCommand  *string
	Visibility    *string
	Archived      *bool
}

type Service struct {
	db *sql.DB
}

func New(db *sql.DB) *Service { return &Service{db: db} }

const projectColumns = `p.id,p.name,p.description,p.repo_url,p.default_branch,p.build_command,p.status,p.visibility,p.created_by,p.created_at,p.updated_at`

// VisiblePredicate renders the SQL condition that restricts a query to projects
// the actor may see. It is the single definition of project visibility; other
// packages embed it so a joined query filters and paginates identically.
//
// alias is the projects table alias in the caller's query, adminParam is the
// 1-based index of the caller's "is global admin" boolean parameter, and
// actorParam is the index of the caller's user id. Both parameters appear more
// than once, so PostgreSQL requires their placeholders to be reused verbatim.
func VisiblePredicate(alias string, adminParam, actorParam int) string {
	return fmt.Sprintf(`(%[1]s.visibility='%[2]s' OR $%[3]d OR %[1]s.created_by=$%[4]d OR EXISTS (
		SELECT 1 FROM project_members pm WHERE pm.project_id=%[1]s.id AND pm.user_id=$%[4]d))`,
		alias, VisibilityPublic, adminParam, actorParam)
}

// VisibilityArgs returns the parameter pair VisiblePredicate expects for this
// actor: whether the actor is a global admin, and the actor's user id.
func VisibilityArgs(actor auth.Actor) (isAdmin bool, userID string) {
	return actor.Role == globalAdminRole, actor.ID
}

// RowQuerier is satisfied by both *sql.DB and *sql.Tx, so a caller that already
// holds the project row in a transaction can pass that transaction and see the
// same snapshot.
type RowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// AuthorizeWrite applies the project-maintainer overlay on top of the global
// role. The global role stays a ceiling: a viewer never writes, an admin always
// may. A private project additionally requires the caller to be one of its
// maintainers; a public project keeps the pre-membership rule where any member
// may write. An invisible project is reported as not found, never as forbidden.
func (s *Service) AuthorizeWrite(ctx context.Context, q RowQuerier, actor auth.Actor, projectID string) error {
	if actor.Role == globalAdminRole {
		return nil
	}
	if actor.Role != globalMemberRole {
		return fmt.Errorf("%w: read-only roles cannot modify projects", ErrForbidden)
	}
	var visibility string
	err := q.QueryRowContext(ctx, `SELECT visibility FROM projects WHERE id=$1`, projectID).Scan(&visibility)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if visibility != VisibilityPrivate {
		return nil
	}
	role, ok, err := memberRole(ctx, q, actor.ID, projectID)
	if err != nil {
		return err
	}
	if !ok || role != RoleMaintainer {
		return fmt.Errorf("%w: only a project maintainer can modify a private project", ErrForbidden)
	}
	return nil
}

// memberRole reads one membership row; ok is false when the user is not a member.
func memberRole(ctx context.Context, q RowQuerier, userID, projectID string) (role string, ok bool, err error) {
	err = q.QueryRowContext(ctx, `SELECT role FROM project_members WHERE project_id=$1 AND user_id=$2`, projectID, userID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return role, true, nil
}

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

func validateBuildCommand(command string) error {
	if err := validateText("buildCommand", command, 512); err != nil {
		return err
	}
	if strings.ContainsAny(command, "\r\n") {
		return fmt.Errorf("%w: buildCommand must be a single line", ErrValidation)
	}
	return nil
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

// normalizeVisibility maps an omitted visibility to the backward-compatible
// default and rejects values outside the documented enumeration.
func normalizeVisibility(value string) (string, error) {
	if value == "" {
		return VisibilityPublic, nil
	}
	if value != VisibilityPublic && value != VisibilityPrivate {
		return "", fmt.Errorf("%w: visibility must be public or private", ErrValidation)
	}
	return value, nil
}

// validateMemberRole bounds project roles to the two documented values.
func validateMemberRole(role string) error {
	if role != RoleMaintainer && role != RoleViewer {
		return fmt.Errorf("%w: role must be maintainer or viewer", ErrValidation)
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
	if err := validateBuildCommand(input.BuildCommand); err != nil {
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
	if err := row.Scan(&p.ID, &p.Name, &p.Description, &p.RepoURL, &p.DefaultBranch, &p.BuildCommand, &p.Status, &p.Visibility, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	return &p, nil
}

// Create registers a new active project; names are unique case-insensitively.
// The creator becomes the first maintainer in the same transaction, so owner
// access survives a later switch to private visibility.
func (s *Service) Create(ctx context.Context, actor auth.Actor, input Input) (*Project, error) {
	if err := validateInput(input); err != nil {
		return nil, err
	}
	visibility, err := normalizeVisibility(input.Visibility)
	if err != nil {
		return nil, err
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	project := &Project{ID: id, Name: input.Name, Description: input.Description, RepoURL: input.RepoURL, DefaultBranch: input.DefaultBranch, BuildCommand: input.BuildCommand, Status: StatusActive, Visibility: visibility, CreatedBy: actor.ID, CreatedAt: now, UpdatedAt: now}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO projects(id,name,description,repo_url,default_branch,build_command,status,visibility,created_by,created_at,updated_at)
	 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$10) ON CONFLICT (lower(name)) DO NOTHING`, id, input.Name, input.Description, input.RepoURL, input.DefaultBranch, input.BuildCommand, StatusActive, visibility, actor.ID, now)
	if err != nil {
		return nil, err
	}
	if n, err := result.RowsAffected(); err != nil {
		return nil, err
	} else if n == 0 {
		return nil, fmt.Errorf("%w: project name already exists", ErrConflict)
	}
	if actor.ID != "" {
		if _, err = tx.ExecContext(ctx, `INSERT INTO project_members(project_id,user_id,role,created_by,created_at)
		 VALUES($1,$2,$3,$2,$4) ON CONFLICT DO NOTHING`, id, actor.ID, RoleMaintainer, now); err != nil {
			return nil, err
		}
	}
	if err = writeAudit(ctx, tx, actor, ActionCreate, id, input.Name); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return project, nil
}

// Get returns one project by id when the actor may see it. An invisible
// project is reported as ErrNotFound so private ids cannot be enumerated.
func (s *Service) Get(ctx context.Context, actor auth.Actor, id string) (*Project, error) {
	if !auth.ValidID(id) {
		return nil, fmt.Errorf("%w: invalid project id", ErrValidation)
	}
	isAdmin, userID := VisibilityArgs(actor)
	project, err := scanProject(s.db.QueryRowContext(ctx, `SELECT `+projectColumns+` FROM projects p
	 WHERE p.id=$1 AND `+VisiblePredicate("p", 2, 3), id, isAdmin, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return project, nil
}

// List pages the projects the actor may see, ordered by name; status must be
// empty, active or archived.
func (s *Service) List(ctx context.Context, actor auth.Actor, status string, limit, offset int) ([]Project, int, error) {
	if status != "" && status != StatusActive && status != StatusArchived {
		return nil, 0, fmt.Errorf("%w: status filter must be active or archived", ErrValidation)
	}
	if limit < 1 || limit > 100 || offset < 0 {
		return nil, 0, fmt.Errorf("%w: limit must be 1-100 and offset non-negative", ErrValidation)
	}
	isAdmin, userID := VisibilityArgs(actor)
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects p WHERE ($1='' OR p.status=$1) AND `+VisiblePredicate("p", 2, 3), status, isAdmin, userID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+projectColumns+` FROM projects p
	 WHERE ($1='' OR p.status=$1) AND `+VisiblePredicate("p", 4, 5)+`
	 ORDER BY lower(p.name),p.id LIMIT $2 OFFSET $3`, status, limit, offset, isAdmin, userID)
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
	if patch.Name == nil && patch.Description == nil && patch.RepoURL == nil && patch.DefaultBranch == nil && patch.BuildCommand == nil && patch.Visibility == nil && patch.Archived == nil {
		return nil, fmt.Errorf("%w: at least one field is required", ErrValidation)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	isAdmin, userID := VisibilityArgs(actor)
	// The visibility predicate runs inside the locking read, so an actor who
	// cannot see the project cannot update it either (404, not 403).
	current, err := scanProject(tx.QueryRowContext(ctx, `SELECT `+projectColumns+` FROM projects p
	 WHERE p.id=$1 AND `+VisiblePredicate("p", 2, 3)+` FOR UPDATE`, id, isAdmin, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	// Visibility decides whether the actor can see the row; the maintainer
	// overlay then decides whether it may change it.
	if err = s.AuthorizeWrite(ctx, tx, actor, id); err != nil {
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
	if patch.BuildCommand != nil {
		updated.BuildCommand = *patch.BuildCommand
		fieldChange = true
	}
	if patch.Visibility != nil {
		visibility, err := normalizeVisibility(*patch.Visibility)
		if err != nil {
			return nil, err
		}
		updated.Visibility = visibility
		fieldChange = true
	}
	// An archived project is read-only; only restoring it (or repeating the
	// archive no-op) is accepted.
	if current.Status == StatusArchived && fieldChange {
		return nil, fmt.Errorf("%w: archived project is read-only until restored", ErrConflict)
	}
	if fieldChange {
		if err = validateInput(Input{Name: updated.Name, Description: updated.Description, RepoURL: updated.RepoURL, DefaultBranch: updated.DefaultBranch, BuildCommand: updated.BuildCommand}); err != nil {
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
	if _, err = tx.ExecContext(ctx, `UPDATE projects SET name=$2,description=$3,repo_url=$4,default_branch=$5,build_command=$6,visibility=$7,status=$8,updated_at=$9 WHERE id=$1`,
		id, updated.Name, updated.Description, updated.RepoURL, updated.DefaultBranch, updated.BuildCommand, updated.Visibility, updated.Status, updated.UpdatedAt); err != nil {
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

const memberColumns = `pm.project_id,pm.user_id,COALESCE(u.username,''),pm.role,pm.created_by,pm.created_at`

func scanMember(row rowScanner) (*Member, error) {
	var m Member
	if err := row.Scan(&m.ProjectID, &m.UserID, &m.Username, &m.Role, &m.CreatedBy, &m.CreatedAt); err != nil {
		return nil, err
	}
	return &m, nil
}

// ListMembers returns the members of a project the actor may see, maintainers
// first. Membership is never readable for an invisible project, so the caller
// cannot probe a private project's member list.
func (s *Service) ListMembers(ctx context.Context, actor auth.Actor, projectID string) ([]Member, error) {
	if _, err := s.Get(ctx, actor, projectID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+memberColumns+` FROM project_members pm
	 LEFT JOIN auth_users u ON u.id=pm.user_id
	 WHERE pm.project_id=$1
	 ORDER BY CASE pm.role WHEN '`+RoleMaintainer+`' THEN 0 ELSE 1 END, COALESCE(u.username,''), pm.user_id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	members := make([]Member, 0)
	for rows.Next() {
		member, err := scanMember(rows)
		if err != nil {
			return nil, err
		}
		members = append(members, *member)
	}
	return members, rows.Err()
}

// AddMember grants a user a role on the project. Adding an existing member is a
// conflict; the target account must exist so membership cannot hold dangling ids.
// Membership changes are audited inside the same transaction as the row.
func (s *Service) AddMember(ctx context.Context, actor auth.Actor, projectID, userID, role string) (*Member, error) {
	if !auth.ValidID(projectID) {
		return nil, fmt.Errorf("%w: invalid project id", ErrValidation)
	}
	if !auth.ValidID(userID) {
		return nil, fmt.Errorf("%w: invalid user id", ErrValidation)
	}
	if err := validateMemberRole(role); err != nil {
		return nil, err
	}
	if _, err := s.Get(ctx, actor, projectID); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = s.AuthorizeWrite(ctx, tx, actor, projectID); err != nil {
		return nil, err
	}
	var username string
	err = tx.QueryRowContext(ctx, `SELECT username FROM auth_users WHERE id=$1`, userID).Scan(&username)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: user not found", ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	result, err := tx.ExecContext(ctx, `INSERT INTO project_members(project_id,user_id,role,created_by,created_at)
	 VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, projectID, userID, role, actor.ID, now)
	if err != nil {
		return nil, err
	}
	if n, err := result.RowsAffected(); err != nil {
		return nil, err
	} else if n == 0 {
		return nil, fmt.Errorf("%w: user is already a member of this project", ErrConflict)
	}
	if err = writeAudit(ctx, tx, actor, ActionMemberAdd, projectID, fmt.Sprintf("user=%s role=%s", username, role)); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &Member{ProjectID: projectID, UserID: userID, Username: username, Role: role, CreatedBy: actor.ID, CreatedAt: now}, nil
}

// UpdateMemberRole changes one membership role; a missing membership is not
// found rather than silently created.
func (s *Service) UpdateMemberRole(ctx context.Context, actor auth.Actor, projectID, userID, role string) (*Member, error) {
	if !auth.ValidID(projectID) {
		return nil, fmt.Errorf("%w: invalid project id", ErrValidation)
	}
	if !auth.ValidID(userID) {
		return nil, fmt.Errorf("%w: invalid user id", ErrValidation)
	}
	if err := validateMemberRole(role); err != nil {
		return nil, err
	}
	if _, err := s.Get(ctx, actor, projectID); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = s.AuthorizeWrite(ctx, tx, actor, projectID); err != nil {
		return nil, err
	}
	member, err := scanMember(tx.QueryRowContext(ctx, `UPDATE project_members pm SET role=$3
	 WHERE pm.project_id=$1 AND pm.user_id=$2
	 RETURNING pm.project_id,pm.user_id,(SELECT username FROM auth_users WHERE id=pm.user_id),pm.role,pm.created_by,pm.created_at`,
		projectID, userID, role))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: membership not found", ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	if err = writeAudit(ctx, tx, actor, ActionMemberUpdate, projectID, fmt.Sprintf("user=%s role=%s", member.Username, member.Role)); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return member, nil
}

// RemoveMember deletes one membership. The project creator keeps owner access
// for the lifetime of the project and cannot be removed.
func (s *Service) RemoveMember(ctx context.Context, actor auth.Actor, projectID, userID string) error {
	if !auth.ValidID(projectID) {
		return fmt.Errorf("%w: invalid project id", ErrValidation)
	}
	if !auth.ValidID(userID) {
		return fmt.Errorf("%w: invalid user id", ErrValidation)
	}
	project, err := s.Get(ctx, actor, projectID)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = s.AuthorizeWrite(ctx, tx, actor, projectID); err != nil {
		return err
	}
	if userID == project.CreatedBy {
		return fmt.Errorf("%w: the project creator cannot be removed", ErrValidation)
	}
	var role string
	err = tx.QueryRowContext(ctx, `DELETE FROM project_members WHERE project_id=$1 AND user_id=$2 RETURNING role`, projectID, userID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: membership not found", ErrNotFound)
	}
	if err != nil {
		return err
	}
	var username string
	_ = tx.QueryRowContext(ctx, `SELECT username FROM auth_users WHERE id=$1`, userID).Scan(&username)
	if err = writeAudit(ctx, tx, actor, ActionMemberRemove, projectID, fmt.Sprintf("user=%s role=%s", username, role)); err != nil {
		return err
	}
	return tx.Commit()
}
