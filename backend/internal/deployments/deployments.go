// Package deployments is the deployment-target registry: validation,
// admin-only CRUD, an encrypted-at-rest credential, and a connectivity probe.
// It is the foundation for deploy tasks (P4.2) and approvals (P4.3); neither is
// implemented here. Secrets are sealed with AES-256-GCM and an AAD bound to the
// target id, decrypted only for a health probe, and never returned or logged.
// The HTTP layer owns authorization (admin only); this package never depends on
// Gin and never sees the caller's role as authority beyond the audit actor.
package deployments

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
)

var (
	ErrValidation = errors.New("validation failed")
	ErrConflict   = errors.New("conflict")
	ErrNotFound   = errors.New("not found")
)

// Audited deployment-target actions, recorded in the shared auth_audit trail.
// The detail never contains the credential: only the target name and the probe
// outcome are recorded.
const (
	ActionCreate = "deployment_target.create"
	ActionUpdate = "deployment_target.update"
	ActionRemove = "deployment_target.remove"
	ActionCheck  = "deployment_target.check"
)

const (
	// KindHTTP is the only target kind in this slice: a probe over HTTP(S).
	KindHTTP = "http"
)

const (
	EnvironmentDevelopment = "development"
	EnvironmentStaging     = "staging"
	EnvironmentProduction  = "production"
)

const (
	StatusActive   = "active"
	StatusDisabled = "disabled"
)

const (
	defaultHealthPath    = "/"
	defaultDeployPath    = "/deploy"
	defaultCheckTimeout  = 5 * time.Second
	defaultDeployTimeout = 10 * time.Minute
)

// Options is explicit dependency configuration. An empty SecretKey disables
// secret storage: targets without a secret keep working, writing one is a
// validation error rather than a silent plaintext fallback.
type Options struct {
	SecretKey    []byte
	CheckTimeout time.Duration
	// DeployTimeout bounds one deployment execution; zero means 10m.
	DeployTimeout time.Duration
	// ApprovalEnvironments lists target environments that require approval.
	// Nil means ["production"]; an explicitly empty slice disables approvals.
	ApprovalEnvironments []string
	// ApprovalTTL bounds a pending approval; zero means 24h.
	ApprovalTTL time.Duration
	HTTPClient  *http.Client
}

// Target is the public view of a deployment target. Secret material is never
// part of it; HasSecret only reports whether one is stored.
type Target struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Kind            string `json:"kind"`
	Environment     string `json:"environment"`
	Endpoint        string `json:"endpoint"`
	HealthPath      string `json:"healthPath"`
	DeployPath      string `json:"deployPath"`
	HasSecret       bool   `json:"hasSecret"`
	Status          string `json:"status"`
	CreatedBy       string `json:"createdBy"`
	CreatedAt       int64  `json:"createdAt"`
	UpdatedAt       int64  `json:"updatedAt"`
	LastCheckAt     int64  `json:"lastCheckAt"`
	LastCheckOK     bool   `json:"lastCheckOk"`
	LastCheckDetail string `json:"lastCheckDetail"`
}

// Input creates a target. Empty optional fields take their documented default.
type Input struct {
	Name        string
	Kind        string
	Environment string
	Endpoint    string
	HealthPath  string
	DeployPath  string
	Secret      string
	Status      string
}

// Patch updates a target. The Secret pointer distinguishes "leave as is" (nil),
// "clear it" (empty string) and "replace it" (non-empty); every other field is
// only changed when present.
type Patch struct {
	Name        *string
	Environment *string
	Endpoint    *string
	HealthPath  *string
	DeployPath  *string
	Secret      *string
	Status      *string
}

// CheckResult is one connectivity probe outcome.
type CheckResult struct {
	OK         bool   `json:"ok"`
	StatusCode int    `json:"statusCode"`
	LatencyMS  int64  `json:"latencyMs"`
	Detail     string `json:"detail"`
	CheckedAt  int64  `json:"checkedAt"`
}

type Service struct {
	db            *sql.DB
	secretKey     []byte
	checkTimeout  time.Duration
	deployTimeout time.Duration
	httpClient    *http.Client
	// approvalEnvironments is the set of target environments that need a
	// pending approval before the worker may claim the deployment.
	approvalEnvironments map[string]bool
	approvalTTL          time.Duration
	// notify wakes the worker after a deployment is enqueued; buffered so the
	// request path never blocks.
	notify chan struct{}
	// leaseRetry overrides the standby lease retry in tests.
	leaseRetry time.Duration
}

// New builds the registry and deployment service. A nil HTTPClient uses a
// client that refuses cross-host redirects and stops after three hops; zero
// timeouts mean 5s for a probe and 10m for a deployment execution.
func New(db *sql.DB, opts Options) *Service {
	timeout := opts.CheckTimeout
	if timeout <= 0 {
		timeout = defaultCheckTimeout
	}
	deployTimeout := opts.DeployTimeout
	if deployTimeout <= 0 {
		deployTimeout = defaultDeployTimeout
	}
	client := opts.HTTPClient
	if client == nil {
		client = defaultHTTPClient()
	}
	environments := opts.ApprovalEnvironments
	if environments == nil {
		environments = []string{EnvironmentProduction}
	}
	approvalSet := make(map[string]bool, len(environments))
	for _, environment := range environments {
		approvalSet[environment] = true
	}
	approvalTTL := opts.ApprovalTTL
	if approvalTTL <= 0 {
		approvalTTL = defaultApprovalTTL
	}
	return &Service{
		db:                   db,
		secretKey:            append([]byte(nil), opts.SecretKey...),
		checkTimeout:         timeout,
		deployTimeout:        deployTimeout,
		httpClient:           client,
		approvalEnvironments: approvalSet,
		approvalTTL:          approvalTTL,
		notify:               make(chan struct{}, 1),
		leaseRetry:           defaultLeaseRetry,
	}
}

// wake nudges the worker loop without blocking the request path.
func (s *Service) wake() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func defaultHTTPClient() *http.Client {
	return &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return errors.New("stopped after 3 redirects")
		}
		if req.URL.Host != via[0].URL.Host {
			return errors.New("refusing cross-host redirect")
		}
		// Never let a redirect downgrade a TLS probe to cleartext: the stored
		// secret travels in the Authorization header of every hop.
		if via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
			return errors.New("refusing redirect from https to plain http")
		}
		return nil
	}}
}

// secretReady reports whether encryption at rest is configured.
func (s *Service) secretReady() bool { return len(s.secretKey) == 32 }

func secretNotConfigured() error {
	return fmt.Errorf("%w: secret storage is not configured (set AEGIS_SECRET_KEY)", ErrValidation)
}

// targetColumns is the select list of every read; has_secret is derived so the
// ciphertext never leaves the database for a normal read.
const targetColumns = `id,name,kind,environment,endpoint,health_path,deploy_path,(octet_length(secret_cipher)>0)::int,status,created_by,created_at,updated_at,last_check_at,last_check_ok,last_check_detail`

type rowScanner interface{ Scan(dest ...any) error }

func scanTarget(row rowScanner) (Target, error) {
	var t Target
	var hasSecret, lastCheckOK int
	if err := row.Scan(&t.ID, &t.Name, &t.Kind, &t.Environment, &t.Endpoint, &t.HealthPath, &t.DeployPath, &hasSecret, &t.Status, &t.CreatedBy, &t.CreatedAt, &t.UpdatedAt, &t.LastCheckAt, &lastCheckOK, &t.LastCheckDetail); err != nil {
		return Target{}, err
	}
	t.HasSecret = hasSecret != 0
	t.LastCheckOK = lastCheckOK != 0
	return t, nil
}

func newID() (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// writeAudit records a deployment-target operation inside the caller's
// transaction so the state change and its audit row commit together. detail is
// a short safe context string; the credential is never passed in.
func writeAudit(ctx context.Context, tx *sql.Tx, actor auth.Actor, action, objectID, detail string) error {
	return writeAuditObject(ctx, tx, actor, action, "deployment_target", objectID, detail)
}

// writeAuditObject is the shared insert for target and deployment rows; the
// object type distinguishes the two vocabularies in the same audit trail.
func writeAuditObject(ctx context.Context, tx *sql.Tx, actor auth.Actor, action, objectType, objectID, detail string) error {
	id, err := newID()
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO auth_audit(id,created_at,actor_id,actor_username,action,object_type,object_id,result,detail,request_id)
	 VALUES($1,$2,$3,$4,$5,$6,$7,'success',$8,$9)`, id, time.Now().Unix(), actor.ID, actor.Username, action, objectType, objectID, detail, actor.RequestID)
	return err
}

func validateName(name string) error {
	if !utf8.ValidString(name) || len(name) < 3 || len(name) > 64 {
		return fmt.Errorf("%w: name must be 3-64 UTF-8 bytes", ErrValidation)
	}
	if strings.TrimSpace(name) != name || strings.ContainsFunc(name, unicode.IsControl) {
		return fmt.Errorf("%w: name must be trimmed and a single line", ErrValidation)
	}
	return nil
}

func normalizeKind(kind string) (string, error) {
	if kind == "" {
		return KindHTTP, nil
	}
	if kind != KindHTTP {
		return "", fmt.Errorf("%w: kind must be http", ErrValidation)
	}
	return kind, nil
}

func normalizeEnvironment(environment string) (string, error) {
	if environment == "" {
		return EnvironmentDevelopment, nil
	}
	switch environment {
	case EnvironmentDevelopment, EnvironmentStaging, EnvironmentProduction:
		return environment, nil
	default:
		return "", fmt.Errorf("%w: environment must be development, staging or production", ErrValidation)
	}
}

func normalizeStatus(status string) (string, error) {
	if status == "" {
		return StatusActive, nil
	}
	if status != StatusActive && status != StatusDisabled {
		return "", fmt.Errorf("%w: status must be active or disabled", ErrValidation)
	}
	return status, nil
}

func normalizeHealthPath(healthPath string) (string, error) {
	if healthPath == "" {
		return defaultHealthPath, nil
	}
	if !utf8.ValidString(healthPath) || len(healthPath) > 256 {
		return "", fmt.Errorf("%w: healthPath must be valid UTF-8 of at most 256 bytes", ErrValidation)
	}
	if !strings.HasPrefix(healthPath, "/") {
		return "", fmt.Errorf("%w: healthPath must start with /", ErrValidation)
	}
	if strings.ContainsFunc(healthPath, unicode.IsSpace) || strings.ContainsFunc(healthPath, unicode.IsControl) {
		return "", fmt.Errorf("%w: healthPath must not contain whitespace", ErrValidation)
	}
	return healthPath, nil
}

func normalizeDeployPath(deployPath string) (string, error) {
	if deployPath == "" {
		return defaultDeployPath, nil
	}
	if !utf8.ValidString(deployPath) || len(deployPath) > 256 {
		return "", fmt.Errorf("%w: deployPath must be valid UTF-8 of at most 256 bytes", ErrValidation)
	}
	if !strings.HasPrefix(deployPath, "/") {
		return "", fmt.Errorf("%w: deployPath must start with /", ErrValidation)
	}
	if strings.ContainsFunc(deployPath, unicode.IsSpace) || strings.ContainsFunc(deployPath, unicode.IsControl) {
		return "", fmt.Errorf("%w: deployPath must not contain whitespace", ErrValidation)
	}
	return deployPath, nil
}

// validateEndpoint accepts an http(s) URL with a host and no userinfo, query or
// fragment: the probe appends healthPath, so those components would either
// confuse the probe or leak credentials into logs.
func validateEndpoint(endpoint string) error {
	if !utf8.ValidString(endpoint) || endpoint == "" || len(endpoint) > 2048 {
		return fmt.Errorf("%w: endpoint must be a valid URL of at most 2048 bytes", ErrValidation)
	}
	if strings.ContainsFunc(endpoint, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) {
		return fmt.Errorf("%w: endpoint must not contain whitespace", ErrValidation)
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		return fmt.Errorf("%w: endpoint must be an http or https URL with a host", ErrValidation)
	}
	if parsed.User != nil {
		return fmt.Errorf("%w: endpoint must not contain userinfo", ErrValidation)
	}
	if parsed.Fragment != "" || parsed.RawFragment != "" {
		return fmt.Errorf("%w: endpoint must not contain a fragment", ErrValidation)
	}
	if parsed.RawQuery != "" || parsed.ForceQuery {
		return fmt.Errorf("%w: endpoint must not contain a query", ErrValidation)
	}
	return nil
}

// validateSecret bounds the credential that becomes an Authorization header:
// it must stay a single line and fit in a header value.
func validateSecret(secret string) error {
	if secret == "" {
		return nil
	}
	if len(secret) > 4096 {
		return fmt.Errorf("%w: secret must be at most 4096 bytes", ErrValidation)
	}
	if !utf8.ValidString(secret) || strings.ContainsFunc(secret, unicode.IsControl) {
		return fmt.Errorf("%w: secret must be valid UTF-8 without control characters", ErrValidation)
	}
	return nil
}

// validateSecretTransport refuses to pair a stored credential with cleartext
// HTTP unless the host is loopback: the probe sends the secret as a Bearer
// header, and plain http would put it on the wire in the clear. Loopback stays
// allowed so local probes and tests do not need a certificate. It takes
// hasSecret rather than the value because an update must check the stored
// credential without decrypting it.
func validateSecretTransport(endpoint string, hasSecret bool) error {
	if !hasSecret {
		return nil
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("%w: endpoint must be an http or https URL", ErrValidation)
	}
	if parsed.Scheme == "https" {
		return nil
	}
	host := parsed.Hostname()
	if host == "localhost" || host == "::1" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("%w: a secret requires an https endpoint; plain http is only allowed to a loopback host", ErrValidation)
}

// isUniqueViolation reports a PostgreSQL unique-constraint failure so a rare
// concurrent rename is answered with 409 instead of 500.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// isForeignKeyViolation reports a PostgreSQL foreign-key failure: once deploy
// tasks reference a target, deleting it must be a 409 instead of a 500.
func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

func validateTargetFields(name, endpoint string) error {
	if err := validateName(name); err != nil {
		return err
	}
	return validateEndpoint(endpoint)
}

func paginate(limit, offset int) error {
	if limit < 1 || limit > 100 || offset < 0 {
		return fmt.Errorf("%w: limit must be 1-100 and offset non-negative", ErrValidation)
	}
	return nil
}

// Create registers a target. The name is unique case-insensitively; the
// credential, when supplied, must be storable and is sealed with the new id as
// AAD. The audit row commits in the same transaction.
func (s *Service) Create(ctx context.Context, actor auth.Actor, input Input) (Target, error) {
	kind, err := normalizeKind(input.Kind)
	if err != nil {
		return Target{}, err
	}
	environment, err := normalizeEnvironment(input.Environment)
	if err != nil {
		return Target{}, err
	}
	status, err := normalizeStatus(input.Status)
	if err != nil {
		return Target{}, err
	}
	healthPath, err := normalizeHealthPath(input.HealthPath)
	if err != nil {
		return Target{}, err
	}
	deployPath, err := normalizeDeployPath(input.DeployPath)
	if err != nil {
		return Target{}, err
	}
	if err = validateTargetFields(input.Name, input.Endpoint); err != nil {
		return Target{}, err
	}
	if err = validateSecret(input.Secret); err != nil {
		return Target{}, err
	}
	if err = validateSecretTransport(input.Endpoint, input.Secret != ""); err != nil {
		return Target{}, err
	}
	if input.Secret != "" && !s.secretReady() {
		return Target{}, secretNotConfigured()
	}
	id, err := newID()
	if err != nil {
		return Target{}, err
	}
	cipher := []byte{}
	if input.Secret != "" {
		if cipher, err = s.encryptSecret(id, input.Secret); err != nil {
			return Target{}, err
		}
	}
	now := time.Now().Unix()
	target := Target{ID: id, Name: input.Name, Kind: kind, Environment: environment, Endpoint: input.Endpoint,
		HealthPath: healthPath, DeployPath: deployPath, HasSecret: input.Secret != "", Status: status, CreatedBy: actor.ID, CreatedAt: now, UpdatedAt: now}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return Target{}, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO deployment_targets(id,name,kind,environment,endpoint,health_path,deploy_path,secret_cipher,status,created_by,created_at,updated_at)
	 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$11) ON CONFLICT (lower(name)) DO NOTHING`,
		id, target.Name, kind, environment, input.Endpoint, healthPath, deployPath, cipher, status, actor.ID, now)
	if err != nil {
		return Target{}, err
	}
	if n, err := result.RowsAffected(); err != nil {
		return Target{}, err
	} else if n == 0 {
		return Target{}, fmt.Errorf("%w: deployment target name already exists", ErrConflict)
	}
	if err = writeAudit(ctx, tx, actor, ActionCreate, id, "name="+target.Name); err != nil {
		return Target{}, err
	}
	if err = tx.Commit(); err != nil {
		return Target{}, err
	}
	return target, nil
}

// Get returns one target by id. Callers are already authorized by the HTTP
// layer (admin only); a malformed or unknown id is never a 403.
func (s *Service) Get(ctx context.Context, actor auth.Actor, id string) (Target, error) {
	if !auth.ValidID(id) {
		return Target{}, fmt.Errorf("%w: invalid deployment target id", ErrValidation)
	}
	target, err := scanTarget(s.db.QueryRowContext(ctx, `SELECT `+targetColumns+` FROM deployment_targets WHERE id=$1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Target{}, ErrNotFound
	}
	if err != nil {
		return Target{}, err
	}
	return target, nil
}

// List pages every target, ordered by name; totals use the same predicate.
func (s *Service) List(ctx context.Context, actor auth.Actor, limit, offset int) ([]Target, int, error) {
	if err := paginate(limit, offset); err != nil {
		return nil, 0, err
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM deployment_targets`).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+targetColumns+` FROM deployment_targets
	 ORDER BY lower(name),id LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	targets := make([]Target, 0)
	for rows.Next() {
		target, err := scanTarget(rows)
		if err != nil {
			return nil, 0, err
		}
		targets = append(targets, target)
	}
	return targets, total, rows.Err()
}

// Update applies a partial update under a row lock. The stored credential is
// only touched when the patch carries the Secret pointer.
func (s *Service) Update(ctx context.Context, actor auth.Actor, id string, patch Patch) (Target, error) {
	if !auth.ValidID(id) {
		return Target{}, fmt.Errorf("%w: invalid deployment target id", ErrValidation)
	}
	if patch.Name == nil && patch.Environment == nil && patch.Endpoint == nil && patch.HealthPath == nil && patch.DeployPath == nil && patch.Secret == nil && patch.Status == nil {
		return Target{}, fmt.Errorf("%w: at least one field is required", ErrValidation)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return Target{}, err
	}
	defer tx.Rollback()
	current, err := scanTarget(tx.QueryRowContext(ctx, `SELECT `+targetColumns+` FROM deployment_targets WHERE id=$1 FOR UPDATE`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Target{}, ErrNotFound
	}
	if err != nil {
		return Target{}, err
	}
	updated := current
	changed := false
	if patch.Name != nil && *patch.Name != current.Name {
		updated.Name, changed = *patch.Name, true
	}
	if patch.Environment != nil && *patch.Environment != current.Environment {
		environment, err := normalizeEnvironment(*patch.Environment)
		if err != nil {
			return Target{}, err
		}
		updated.Environment, changed = environment, true
	}
	if patch.Endpoint != nil && *patch.Endpoint != current.Endpoint {
		updated.Endpoint, changed = *patch.Endpoint, true
	}
	if patch.HealthPath != nil {
		healthPath, err := normalizeHealthPath(*patch.HealthPath)
		if err != nil {
			return Target{}, err
		}
		if healthPath != current.HealthPath {
			updated.HealthPath, changed = healthPath, true
		}
	}
	if patch.DeployPath != nil {
		deployPath, err := normalizeDeployPath(*patch.DeployPath)
		if err != nil {
			return Target{}, err
		}
		if deployPath != current.DeployPath {
			updated.DeployPath, changed = deployPath, true
		}
	}
	if patch.Status != nil && *patch.Status != current.Status {
		status, err := normalizeStatus(*patch.Status)
		if err != nil {
			return Target{}, err
		}
		updated.Status, changed = status, true
	}
	secretChanged := false
	if patch.Secret != nil {
		if *patch.Secret == "" {
			if current.HasSecret {
				secretChanged = true
			}
		} else {
			if err := validateSecret(*patch.Secret); err != nil {
				return Target{}, err
			}
			secretChanged = true
		}
	}
	if !changed && !secretChanged {
		return current, nil
	}
	if err := validateTargetFields(updated.Name, updated.Endpoint); err != nil {
		return Target{}, err
	}
	if secretChanged && patch.Secret != nil {
		if err := validateSecretTransport(updated.Endpoint, *patch.Secret != ""); err != nil {
			return Target{}, err
		}
	} else if current.HasSecret {
		// Changing only the endpoint must not move a stored credential onto a
		// cleartext non-loopback host.
		if err := validateSecretTransport(updated.Endpoint, true); err != nil {
			return Target{}, err
		}
	}
	if updated.Name != current.Name {
		var conflict int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM deployment_targets WHERE lower(name)=lower($1) AND id<>$2`, updated.Name, id).Scan(&conflict); err == nil {
			return Target{}, fmt.Errorf("%w: deployment target name already exists", ErrConflict)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return Target{}, err
		}
	}
	cipher := []byte{}
	if secretChanged && *patch.Secret != "" {
		if !s.secretReady() {
			return Target{}, secretNotConfigured()
		}
		if cipher, err = s.encryptSecret(id, *patch.Secret); err != nil {
			return Target{}, err
		}
	} else if !secretChanged && current.HasSecret {
		if err := tx.QueryRowContext(ctx, `SELECT secret_cipher FROM deployment_targets WHERE id=$1`, id).Scan(&cipher); err != nil {
			return Target{}, err
		}
	}
	if secretChanged && patch.Secret != nil {
		updated.HasSecret = *patch.Secret != ""
	}
	updated.UpdatedAt = time.Now().Unix()
	if _, err := tx.ExecContext(ctx, `UPDATE deployment_targets SET name=$2,environment=$3,endpoint=$4,health_path=$5,deploy_path=$6,secret_cipher=$7,status=$8,updated_at=$9 WHERE id=$1`,
		id, updated.Name, updated.Environment, updated.Endpoint, updated.HealthPath, updated.DeployPath, cipher, updated.Status, updated.UpdatedAt); err != nil {
		if isUniqueViolation(err) {
			return Target{}, fmt.Errorf("%w: deployment target name already exists", ErrConflict)
		}
		return Target{}, err
	}
	if err := writeAudit(ctx, tx, actor, ActionUpdate, id, "name="+updated.Name); err != nil {
		return Target{}, err
	}
	if err := tx.Commit(); err != nil {
		return Target{}, err
	}
	return updated, nil
}

// Remove deletes a target. Since P4.2 deploy tasks reference targets, a target
// with deployment history cannot be deleted: the foreign key turns that into
// ErrConflict (409) rather than a 500, preserving the deployment snapshots.
func (s *Service) Remove(ctx context.Context, actor auth.Actor, id string) error {
	if !auth.ValidID(id) {
		return fmt.Errorf("%w: invalid deployment target id", ErrValidation)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var name string
	err = tx.QueryRowContext(ctx, `DELETE FROM deployment_targets WHERE id=$1 RETURNING name`, id).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		if isForeignKeyViolation(err) {
			return fmt.Errorf("%w: deployment target has deployment history and cannot be removed", ErrConflict)
		}
		return err
	}
	if err = writeAudit(ctx, tx, actor, ActionRemove, id, "name="+name); err != nil {
		return err
	}
	return tx.Commit()
}
