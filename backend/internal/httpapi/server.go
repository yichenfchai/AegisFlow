package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
	"github.com/yichenfchai/AegisFlow/backend/internal/builds"
	"github.com/yichenfchai/AegisFlow/backend/internal/deployments"
	"github.com/yichenfchai/AegisFlow/backend/internal/knowledge"
	"github.com/yichenfchai/AegisFlow/backend/internal/metrics"
	"github.com/yichenfchai/AegisFlow/backend/internal/oidc"
	"github.com/yichenfchai/AegisFlow/backend/internal/policies"
	"github.com/yichenfchai/AegisFlow/backend/internal/projects"
	"github.com/yichenfchai/AegisFlow/backend/internal/ratelimit"
	"github.com/yichenfchai/AegisFlow/backend/internal/reviews"
	"github.com/yichenfchai/AegisFlow/backend/internal/scans"
	"github.com/yichenfchai/AegisFlow/backend/internal/secrets"
	"github.com/yichenfchai/AegisFlow/backend/internal/securityevents"
)

type contextKey int

const requestIDKey contextKey = 0
const userKey = "authenticatedUser"
const sessionIDKey = "authenticatedSessionID"

func currentSessionID(c *gin.Context) string {
	value, _ := c.Get(sessionIDKey)
	id, _ := value.(string)
	return id
}

func requestID(c *gin.Context) string {
	id, _ := c.Request.Context().Value(requestIDKey).(string)
	return id
}

type Server struct {
	auth        *auth.Service
	projects    *projects.Service
	builds      *builds.Service
	deployments *deployments.Service
	reviews     *reviews.Service
	scans       *scans.Service
	// securityEvents is the admin-only defense-event store.
	securityEvents *securityevents.Service
	// knowledge is the knowledge-base registry: reads for every authenticated
	// role, writes admin-only.
	knowledge *knowledge.Service
	// secrets is the admin-only write-only secret store; values are never
	// returned by any handler.
	secrets *secrets.Service
	// policies is the per-project automation policy store; deployment creation
	// and rollback read it through the deployment service.
	policies *policies.Service
	// oidc is the OIDC single sign-on relying-party flow; nil means the feature
	// is not configured and its data routes stay closed (503).
	oidc         *oidc.Service
	logger       *slog.Logger
	ready        func(context.Context) error
	origins      map[string]bool
	limiter      ratelimit.Limiter
	cookieSecure bool
	// reviewWebhookSecret authenticates inbound external-result deliveries with
	// HMAC-SHA256; empty keeps that endpoint closed (503).
	reviewWebhookSecret []byte
	// ingestWebhookSecret authenticates the signed scan and security-event
	// deliveries (AEGIS_INGEST_WEBHOOK_SECRET). Empty keeps both endpoints closed
	// (503) while their read routes stay available.
	ingestWebhookSecret []byte
	metrics             *metrics.Registry
	// shutdownCtx is never nil: New substitutes context.Background() when the
	// embedder does not pass one, so streaming selects can always wait on it.
	shutdownCtx context.Context
	// readyMu guards the /readyz probe cache. Both outcomes are cached, so
	// unauthenticated readiness polls cannot amplify load on PostgreSQL/Redis.
	readyMu  sync.Mutex
	readyErr error
	readyAt  time.Time
	// sseConnections counts live build-event streams; the cap lives in
	// sseMaxConnections.
	sseConnections atomic.Int64
}

const refreshCookieName = "aegis_refresh"
const refreshCookiePath = "/api/v1/auth"

// readyCacheTTL bounds how often /readyz probes shared dependencies. The
// endpoint is unauthenticated, so a per-request Ping on PostgreSQL and Redis is
// a cheap way for an external caller to amplify load on both.
const readyCacheTTL = time.Second

// Options is explicit deployment configuration; Secure is never inferred from headers.
type Options struct {
	CookieSecure   bool
	Limiter        ratelimit.Limiter
	TrustedProxies []string
	// Projects enables the project registry routes; when nil they fail closed
	// with 503 like any other missing shared dependency.
	Projects *projects.Service
	// Builds enables the build pipeline routes the same way; the caller owns
	// starting the worker.
	Builds *builds.Service
	// Deployments enables the admin-only deployment-target routes; nil fails
	// closed with 503 like any other missing shared dependency.
	Deployments *deployments.Service
	// Reviews enables the review-gate routes; nil fails closed with 503 like any
	// other missing shared dependency.
	Reviews *reviews.Service
	// Scans enables the scan-registry routes; nil fails closed with 503.
	Scans *scans.Service
	// SecurityEvents enables the admin-only defense-event routes; nil fails
	// closed with 503.
	SecurityEvents *securityevents.Service
	// Knowledge enables the knowledge-base routes; nil fails closed with 503.
	Knowledge *knowledge.Service
	// Secrets enables the admin-only secret-store routes; nil fails closed with
	// 503. A non-nil store without an encryption key still serves metadata and
	// answers 503 secrets_not_configured on create/rotate.
	Secrets *secrets.Service
	// Policies enables the per-project automation-policy routes; nil fails
	// closed with 503. The deployment service reads the same rows through its
	// own injected gate, so a deployment created without this wiring simply has
	// the policy gate disabled (the pre-policy behavior).
	Policies *policies.Service
	// OIDC enables the OIDC single sign-on routes when the provider discovery
	// has already succeeded at startup. Nil keeps the routes registered but
	// closed behind a 503, and the config probe answers enabled=false.
	OIDC *oidc.Service
	// ReviewWebhookSecret verifies signed inbound external review results
	// (AEGIS_REVIEW_WEBHOOK_SECRET). Empty keeps the external-results endpoint
	// closed with 503; the read route stays available to authenticated viewers.
	ReviewWebhookSecret []byte
	// IngestWebhookSecret verifies signed inbound scan and security-event
	// deliveries (AEGIS_INGEST_WEBHOOK_SECRET). Empty keeps both machine
	// endpoints closed with 503; their read routes stay available.
	IngestWebhookSecret []byte
	// Metrics, when set, records request counters and latency histograms keyed
	// by the matched route template. Nil disables recording.
	Metrics *metrics.Registry
	// ShutdownContext ends long-lived streams like build-event SSE when the
	// process starts draining. Nil keeps the previous behavior (streams only
	// end with the client or the server-side lifetime cap), which is what
	// tests and embedders without a signal context expect.
	ShutdownContext context.Context
}

// New builds an isolated Gin engine; business services depend only on context.Context.
func New(service *auth.Service, logger *slog.Logger, ready func(context.Context) error, origins []string, options ...Options) *gin.Engine {
	var opts Options
	if len(options) > 0 {
		opts = options[0]
	}
	shutdownCtx := opts.ShutdownContext
	if shutdownCtx == nil {
		shutdownCtx = context.Background()
	}
	s := &Server{auth: service, projects: opts.Projects, builds: opts.Builds, deployments: opts.Deployments, reviews: opts.Reviews,
		scans: opts.Scans, securityEvents: opts.SecurityEvents, knowledge: opts.Knowledge, secrets: opts.Secrets, policies: opts.Policies,
		oidc:   opts.OIDC,
		logger: logger, ready: ready, origins: map[string]bool{}, limiter: opts.Limiter, cookieSecure: opts.CookieSecure,
		reviewWebhookSecret: append([]byte(nil), opts.ReviewWebhookSecret...),
		ingestWebhookSecret: append([]byte(nil), opts.IngestWebhookSecret...),
		metrics:             opts.Metrics, shutdownCtx: shutdownCtx}
	for _, o := range origins {
		s.origins[o] = true
	}
	router := gin.New()
	// The entrypoint validates deployment CIDRs. Invalid input here fails safe:
	// never retain Gin's default trust-all proxies after a configuration error.
	router.RemoteIPHeaders = []string{"X-Forwarded-For"}
	if err := router.SetTrustedProxies(opts.TrustedProxies); err != nil {
		_ = router.SetTrustedProxies(nil)
	}
	router.RedirectTrailingSlash = false
	router.RedirectFixedPath = false
	router.Use(s.middleware())
	router.GET("/healthz", func(c *gin.Context) { s.respond(c, 200, gin.H{"status": "ok"}) })
	router.GET("/readyz", func(c *gin.Context) {
		if err := s.readyError(c.Request.Context()); err != nil {
			s.fail(c, 503, "not_ready", "dependencies unavailable")
			return
		}
		s.respond(c, 200, gin.H{"status": "ready"})
	})
	api := router.Group("/api/v1")
	authRoutes := api.Group("/auth")
	authRoutes.POST("/login", s.login)
	authRoutes.POST("/refresh", s.refresh)
	authRoutes.POST("/logout", s.logout)
	// OIDC single sign-on (P5 batch D): the probe is always registered and
	// answers enabled=false when the feature is unconfigured; start/callback
	// stay registered too but answer 503 while disabled. All three are public
	// browser navigations, so no Origin header applies (GET only).
	authRoutes.GET("/oidc/config", s.oidcConfig)
	authRoutes.GET("/oidc/start", s.oidcStart)
	authRoutes.GET("/oidc/callback", s.oidcCallback)
	authRoutes.GET("/me", s.authenticate(), s.me)
	authRoutes.POST("/password", s.authenticate(), s.changePassword)
	authRoutes.GET("/sessions", s.authenticate(), s.listSessions)
	authRoutes.DELETE("/sessions", s.authenticate(), s.revokeOtherSessions)
	authRoutes.DELETE("/sessions/:id", s.authenticate(), s.revokeSession)
	users := api.Group("/users", s.authenticate(), s.requireAdmin())
	users.GET("", s.listUsers)
	users.POST("", s.createUser)
	users.PATCH("/:id", s.updateUser)
	users.POST("/:id/password-reset", s.resetPassword)
	audit := api.Group("/audit", s.authenticate(), s.requireAdmin())
	audit.GET("", s.listAudit)
	// CSV export reuses the list filters; it is bounded and reports truncation
	// through X-Audit-Truncated instead of silently dropping rows.
	audit.GET("/export.csv", s.exportAuditCSV)
	projectRoutes := api.Group("/projects", s.authenticate())
	projectRoutes.GET("", s.listProjects)
	projectRoutes.POST("", s.requireRole("admin", "member"), s.createProject)
	projectRoutes.GET("/:id", s.getProject)
	projectRoutes.PATCH("/:id", s.requireRole("admin", "member"), s.updateProject)
	projectRoutes.POST("/:id/builds", s.requireRole("admin", "member"), s.triggerBuild)
	// Membership is readable by anyone who can see the project; mutations are
	// bounded by the global role until the maintainer overlay lands.
	projectRoutes.GET("/:id/members", s.listProjectMembers)
	projectRoutes.POST("/:id/members", s.requireRole("admin", "member"), s.addProjectMember)
	projectRoutes.PATCH("/:id/members/:userId", s.requireRole("admin", "member"), s.updateProjectMember)
	projectRoutes.DELETE("/:id/members/:userId", s.requireRole("admin", "member"), s.removeProjectMember)
	// Deployment tasks live under the project for creation/listing; the
	// service authorizes every action against the project write overlay.
	projectRoutes.GET("/:id/deployments", s.listDeployments)
	projectRoutes.POST("/:id/deployments", s.createDeployment)
	// Review requests are created and listed under their project; decisions are
	// on the review itself. The service applies the visibility predicate and the
	// write/maintainer overlays, exactly like the deployment routes.
	projectRoutes.GET("/:id/reviews", s.listReviews)
	projectRoutes.POST("/:id/reviews", s.createReview)
	// Per-project automation policy: readable by anyone who can see the project
	// (it explains why a deployment was gated), writable by an admin or a
	// project maintainer. The service owns both checks.
	projectRoutes.GET("/:id/policy", s.getProjectPolicy)
	projectRoutes.PUT("/:id/policy", s.putProjectPolicy)
	buildRoutes := api.Group("/builds", s.authenticate())
	buildRoutes.GET("", s.listBuilds)
	buildRoutes.GET("/:id", s.getBuild)
	buildRoutes.GET("/:id/logs", s.getBuildLogs)
	buildRoutes.GET("/:id/events", s.streamBuildEvents)
	buildRoutes.POST("/:id/cancel", s.requireRole("admin", "member"), s.cancelBuild)
	buildRoutes.POST("/:id/retry", s.requireRole("admin", "member"), s.retryBuild)
	// Deployment targets and approvals are global administrative objects; the
	// deploy tasks below them are authorized per project by the service.
	// Deployment targets are admin-managed, but the active target options are
	// readable by every authenticated role: the deploy form needs them, and the
	// same values already appear on deployment snapshots.
	api.GET("/deployment-targets/options", s.authenticate(), s.listDeploymentTargetOptions)
	deploymentTargets := api.Group("/deployment-targets", s.authenticate(), s.requireAdmin())
	deploymentTargets.GET("", s.listDeploymentTargets)
	deploymentTargets.POST("", s.createDeploymentTarget)
	deploymentTargets.GET("/:id", s.getDeploymentTarget)
	deploymentTargets.PATCH("/:id", s.updateDeploymentTarget)
	deploymentTargets.DELETE("/:id", s.removeDeploymentTarget)
	deploymentTargets.POST("/:id/check", s.checkDeploymentTarget)
	deployRoutes := api.Group("/deployments", s.authenticate())
	// The global list mirrors the build list: optional projectId and status
	// filters, newest first, restricted to the projects the caller can see.
	deployRoutes.GET("", s.listDeploymentsGlobal)
	deployRoutes.GET("/:id", s.getDeployment)
	deployRoutes.GET("/:id/events", s.getDeploymentEvents)
	deployRoutes.POST("/:id/cancel", s.cancelDeployment)
	deployRoutes.POST("/:id/rollback", s.rollbackDeployment)
	approvalRoutes := api.Group("/approvals", s.authenticate(), s.requireAdmin())
	approvalRoutes.GET("", s.listApprovals)
	approvalRoutes.POST("/:id/approve", s.approveApproval)
	approvalRoutes.POST("/:id/reject", s.rejectApproval)
	// Review decisions are authorized per project by the service (global admin
	// or maintainer), so the route only requires authentication. The global
	// list mirrors /api/v1/deployments: optional projectId/status filters,
	// restricted to the projects the caller can see.
	reviewRoutes := api.Group("/reviews", s.authenticate())
	reviewRoutes.GET("", s.listReviewsGlobal)
	reviewRoutes.GET("/:id", s.getReview)
	reviewRoutes.POST("/:id/approve", s.approveReview)
	reviewRoutes.POST("/:id/reject", s.rejectReview)
	reviewRoutes.POST("/:id/cancel", s.cancelReview)
	// External results: the read side is a normal authenticated route, while the
	// write side is a machine-to-machine webhook authenticated by its HMAC
	// signature headers (no Bearer, no browser session). It is registered
	// outside the authenticated group on purpose; the handler answers 503 when
	// AEGIS_REVIEW_WEBHOOK_SECRET is not configured.
	reviewRoutes.GET("/:id/external-results", s.listReviewExternalResults)
	api.POST("/reviews/:id/external-results", s.ingestReviewExternalResult)
	// Scan results: the write side is a signed machine-to-machine webhook, while
	// the project-scoped and global reads are normal authenticated routes that
	// follow project visibility. The project route reuses Gin's :id wildcard name
	// so it shares the tree with the other /projects/:id routes.
	projectRoutes.GET("/:id/scans", s.listProjectScans)
	api.POST("/projects/:id/scans", s.ingestScan)
	scanRoutes := api.Group("/scans", s.authenticate())
	scanRoutes.GET("", s.listScansGlobal)
	scanRoutes.GET("/:id", s.getScan)
	// Defense events are global administrative objects: reads and acknowledgement
	// require the admin role, while the ingest webhook is trusted through its HMAC
	// signature headers and stays outside the session groups.
	securityEventRoutes := api.Group("/security-events", s.authenticate(), s.requireAdmin())
	securityEventRoutes.GET("", s.listSecurityEvents)
	securityEventRoutes.GET("/:id", s.getSecurityEvent)
	securityEventRoutes.POST("/:id/ack", s.ackSecurityEvent)
	api.POST("/security-events", s.ingestSecurityEvent)
	// Knowledge base: every authenticated role may read documents, while
	// create/update/delete require the admin role and are audited in the same
	// transaction. The stored body is plain text/Markdown the server never
	// renders, so the client is responsible for safe rendering.
	knowledgeRoutes := api.Group("/knowledge", s.authenticate())
	knowledgeRoutes.GET("", s.listKnowledge)
	knowledgeRoutes.GET("/:id", s.getKnowledge)
	knowledgeRoutes.POST("", s.requireRole("admin"), s.createKnowledge)
	knowledgeRoutes.PUT("/:id", s.requireRole("admin"), s.updateKnowledge)
	knowledgeRoutes.DELETE("/:id", s.requireRole("admin"), s.deleteKnowledge)
	// Independent secret store: admin-only metadata reads plus write-only
	// create/rotate/delete. No route ever returns a secret value.
	secretRoutes := api.Group("/secrets", s.authenticate(), s.requireAdmin())
	secretRoutes.GET("", s.listSecrets)
	secretRoutes.POST("", s.createSecret)
	secretRoutes.PUT("/:id", s.rotateSecret)
	secretRoutes.DELETE("/:id", s.deleteSecret)
	router.NoRoute(func(c *gin.Context) { s.fail(c, 404, "not_found", "endpoint not found") })
	return router
}

// readyError probes the shared dependencies behind the readyCacheTTL cache.
// Success and failure are both cached: a healthy dependency is not re-checked on
// every poll, and a failing one is not hammered several times per second. The
// per-probe budget stays at two seconds.
func (s *Server) readyError(ctx context.Context) error {
	s.readyMu.Lock()
	if !s.readyAt.IsZero() && time.Since(s.readyAt) < readyCacheTTL {
		err := s.readyErr
		s.readyMu.Unlock()
		return err
	}
	s.readyMu.Unlock()
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	err := s.ready(probeCtx)
	s.readyMu.Lock()
	s.readyErr = err
	s.readyAt = time.Now()
	s.readyMu.Unlock()
	return err
}

// loginAttempt consumes one slot of the shared Redis login quota for this
// client. Password login and the OIDC start/callback routes deliberately share
// the same fail-closed limiter: no in-process fallback, one quota across
// instances. It writes the 503/429 response and reports false when the request
// must stop.
func (s *Server) loginAttempt(c *gin.Context) bool {
	// No in-process fallback: all instances must share the same quota.
	if s.limiter == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return false
	}
	allowed, retryAfter, err := s.limiter.Allow(c.Request.Context(), c.ClientIP())
	if err != nil {
		s.logger.ErrorContext(c.Request.Context(), "login limiter unavailable", "requestId", c.Request.Context().Value(requestIDKey), "error", err)
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return false
	}
	if !allowed {
		seconds := int64(retryAfter / time.Second)
		if retryAfter%time.Second > 0 {
			seconds++
		}
		if seconds < 1 {
			seconds = 1
		}
		c.Header("Retry-After", strconv.FormatInt(seconds, 10))
		s.fail(c, 429, "rate_limited", "too many login attempts")
		return false
	}
	return true
}

func (s *Server) login(c *gin.Context) {
	if !s.loginAttempt(c) {
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Remember bool   `json:"remember"`
	}
	if !s.decode(c, &input) {
		return
	}
	ctx := c.Request.Context()
	tokens, err := s.auth.LoginWithRemember(ctx, input.Username, input.Password, input.Remember, requestID(c))
	s.logger.InfoContext(ctx, "auth.login", "requestId", ctx.Value(requestIDKey), "success", err == nil)
	if err != nil {
		s.authError(c, err)
		return
	}
	s.setRefreshCookie(c, tokens)
	s.respond(c, 200, tokens)
}
func (s *Server) refresh(c *gin.Context) {
	if !s.decode(c, &struct{}{}) {
		return
	}
	cookie, err := c.Request.Cookie(refreshCookieName)
	if err != nil || cookie.Value == "" {
		s.clearRefreshCookie(c)
		s.authError(c, auth.ErrUnauthorized)
		return
	}
	tokens, err := s.auth.Refresh(c.Request.Context(), cookie.Value)
	if err != nil {
		if errors.Is(err, auth.ErrUnauthorized) {
			s.clearRefreshCookie(c)
		}
		s.authError(c, err)
		return
	}
	s.setRefreshCookie(c, tokens)
	s.respond(c, 200, tokens)
}
func (s *Server) logout(c *gin.Context) {
	if !s.decode(c, &struct{}{}) {
		return
	}
	// Keep the credential on storage failure so a retry can still revoke the session.
	cookie, err := c.Request.Cookie(refreshCookieName)
	if err == nil {
		if err = s.auth.Logout(c.Request.Context(), cookie.Value); err != nil {
			s.authError(c, err)
			return
		}
	}
	s.clearRefreshCookie(c)
	s.respond(c, 200, gin.H{"loggedOut": true})
}
func (s *Server) setRefreshCookie(c *gin.Context, tokens *auth.Tokens) {
	cookie := &http.Cookie{Name: refreshCookieName, Value: tokens.RefreshToken, Path: refreshCookiePath,
		HttpOnly: true, Secure: s.cookieSecure, SameSite: http.SameSiteStrictMode}
	if tokens.Remember {
		// expires_at is stored once at login; rotation must not create a sliding window.
		cookie.MaxAge = int(tokens.SessionExpiresAt - time.Now().Unix())
		if cookie.MaxAge <= 0 {
			cookie.MaxAge = -1
		}
		cookie.Expires = time.Unix(tokens.SessionExpiresAt, 0).UTC()
	}
	http.SetCookie(c.Writer, cookie)
}
func (s *Server) clearRefreshCookie(c *gin.Context) {
	http.SetCookie(c.Writer, &http.Cookie{Name: refreshCookieName, Value: "", Path: refreshCookiePath,
		HttpOnly: true, Secure: s.cookieSecure, SameSite: http.SameSiteStrictMode,
		MaxAge: -1, Expires: time.Unix(1, 0).UTC()})
}
func (s *Server) authenticate() gin.HandlerFunc {
	return func(c *gin.Context) {
		parts := strings.Fields(c.GetHeader("Authorization"))
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			c.Header("WWW-Authenticate", "Bearer")
			s.fail(c, 401, "unauthorized", "valid bearer token required")
			return
		}
		user, sid, err := s.auth.InspectToken(c.Request.Context(), parts[1])
		if err != nil {
			s.authError(c, err)
			return
		}
		c.Set(userKey, user)
		c.Set(sessionIDKey, sid)
		c.Next()
	}
}
func (s *Server) requireAdmin() gin.HandlerFunc {
	return s.requireRole("admin")
}

// requireRole restricts a route to the listed global roles; page guards are
// advisory and never replace this server-side check.
func (s *Server) requireRole(roles ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		role := currentUser(c).Role
		for _, allowed := range roles {
			if role == allowed {
				c.Next()
				return
			}
		}
		s.fail(c, 403, "forbidden", "insufficient role for this operation")
	}
}
func currentUser(c *gin.Context) *auth.User { return c.MustGet(userKey).(*auth.User) }
func (s *Server) me(c *gin.Context)         { s.respond(c, 200, currentUser(c)) }

// actorFrom builds the audit actor of the authenticated caller. Role comes from
// the freshly inspected database user, never from the request body.
func actorFrom(c *gin.Context) auth.Actor {
	user := currentUser(c)
	return auth.Actor{ID: user.ID, Username: user.Username, Role: user.Role, RequestID: requestID(c)}
}

// page parses shared limit/offset pagination; the response reports it back.
func (s *Server) page(c *gin.Context) (int, int, bool) {
	limit, offset := 20, 0
	if value := c.Query("limit"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > 100 {
			s.fail(c, 400, "invalid_request", "limit must be an integer between 1 and 100")
			return 0, 0, false
		}
		limit = n
	}
	if value := c.Query("offset"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 {
			s.fail(c, 400, "invalid_request", "offset must be a non-negative integer")
			return 0, 0, false
		}
		offset = n
	}
	return limit, offset, true
}

func (s *Server) listUsers(c *gin.Context) {
	limit, offset, ok := s.page(c)
	if !ok {
		return
	}
	users, total, err := s.auth.ListUsersPage(c.Request.Context(), limit, offset)
	if err != nil {
		s.authError(c, err)
		return
	}
	s.respond(c, 200, gin.H{"items": users, "total": total, "limit": limit, "offset": offset})
}
func (s *Server) createUser(c *gin.Context) {
	var input struct {
		Username    string `json:"username"`
		Password    string `json:"password"`
		DisplayName string `json:"displayName"`
		Role        string `json:"role"`
	}
	if !s.decode(c, &input) {
		return
	}
	ctx := c.Request.Context()
	user, err := s.auth.CreateUser(ctx, actorFrom(c), input.Username, input.Password, input.DisplayName, input.Role)
	s.logger.InfoContext(ctx, "auth.create_user", "actorId", currentUser(c).ID, "requestId", ctx.Value(requestIDKey), "success", err == nil)
	if err != nil {
		s.authError(c, err)
		return
	}
	s.respond(c, 201, user)
}

func (s *Server) updateUser(c *gin.Context) {
	id := c.Param("id")
	if !auth.ValidID(id) {
		s.fail(c, 400, "invalid_request", "invalid user id")
		return
	}
	var input struct {
		Role     *string `json:"role"`
		Disabled *bool   `json:"disabled"`
	}
	if !s.decode(c, &input) {
		return
	}
	if input.Role == nil && input.Disabled == nil {
		s.fail(c, 400, "invalid_request", "at least one of role or disabled is required")
		return
	}
	ctx := c.Request.Context()
	actor := actorFrom(c)
	// Role and disabled change together in one transaction; a rejection leaves both unchanged.
	if err := s.auth.UpdateUser(ctx, actor, id, input.Role, input.Disabled); err != nil {
		s.authError(c, err)
		return
	}
	s.respond(c, 200, gin.H{"updated": true})
}

func (s *Server) resetPassword(c *gin.Context) {
	id := c.Param("id")
	if !auth.ValidID(id) {
		s.fail(c, 400, "invalid_request", "invalid user id")
		return
	}
	var input struct {
		Password string `json:"password"`
	}
	if !s.decode(c, &input) {
		return
	}
	if err := s.auth.ResetPassword(c.Request.Context(), actorFrom(c), id, input.Password); err != nil {
		s.authError(c, err)
		return
	}
	s.respond(c, 200, gin.H{"reset": true})
}

func (s *Server) listAudit(c *gin.Context) {
	limit, offset, ok := s.page(c)
	if !ok {
		return
	}
	filter, ok := s.auditFilter(c)
	if !ok {
		return
	}
	entries, total, err := s.auth.ListAudit(c.Request.Context(), filter, limit, offset)
	if err != nil {
		s.authError(c, err)
		return
	}
	s.respond(c, 200, gin.H{"items": entries, "total": total, "limit": limit, "offset": offset})
}

// auditFilter reads the optional audit query parameters. from/to accept an
// RFC3339 timestamp or a YYYY-MM-DD date; a date-only "to" includes that whole
// UTC day so the common "up to today" filter does not drop today's rows.
func (s *Server) auditFilter(c *gin.Context) (auth.AuditFilter, bool) {
	filter := auth.AuditFilter{
		Action:     c.Query("action"),
		ObjectType: c.Query("objectType"),
		Result:     c.Query("result"),
		Actor:      c.Query("actor"),
	}
	var err error
	if filter.From, err = auditBoundary(c.Query("from"), false); err != nil {
		s.fail(c, 400, "invalid_request", "from must be an RFC3339 timestamp or YYYY-MM-DD date")
		return auth.AuditFilter{}, false
	}
	if filter.To, err = auditBoundary(c.Query("to"), true); err != nil {
		s.fail(c, 400, "invalid_request", "to must be an RFC3339 timestamp or YYYY-MM-DD date")
		return auth.AuditFilter{}, false
	}
	return filter, true
}

func auditBoundary(value string, endOfDay bool) (int64, error) {
	if value == "" {
		return 0, nil
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed.Unix(), nil
	}
	day, err := time.Parse("2006-01-02", value)
	if err != nil {
		return 0, err
	}
	if endOfDay {
		return day.Add(24*time.Hour - time.Second).Unix(), nil
	}
	return day.Unix(), nil
}

// projectError maps registry errors to the shared error envelope.
func (s *Server) projectError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, projects.ErrValidation):
		s.fail(c, 400, "validation_failed", "name 3-64 bytes without path or meta characters; repoUrl https://, git@, file:/// URL or absolute path; defaultBranch a plain git ref")
	case errors.Is(err, projects.ErrConflict):
		s.fail(c, 409, "conflict", "project name already exists or the project is archived")
	case errors.Is(err, projects.ErrForbidden):
		s.fail(c, 403, "forbidden", "only a project maintainer can modify a private project")
	case errors.Is(err, projects.ErrNotFound):
		s.fail(c, 404, "not_found", "resource not found")
	default:
		s.logger.ErrorContext(c.Request.Context(), "project request failed", "requestId", requestID(c), "error", err)
		s.fail(c, 500, "internal_error", "internal server error")
	}
}

func (s *Server) listProjects(c *gin.Context) {
	if s.projects == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	limit, offset, ok := s.page(c)
	if !ok {
		return
	}
	status := c.Query("status")
	list, total, err := s.projects.List(c.Request.Context(), actorFrom(c), status, limit, offset)
	if err != nil {
		s.projectError(c, err)
		return
	}
	s.respond(c, 200, gin.H{"items": list, "total": total, "limit": limit, "offset": offset})
}

func (s *Server) createProject(c *gin.Context) {
	if s.projects == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	var input struct {
		Name          string `json:"name"`
		Description   string `json:"description"`
		RepoURL       string `json:"repoUrl"`
		DefaultBranch string `json:"defaultBranch"`
		BuildCommand  string `json:"buildCommand"`
		Visibility    string `json:"visibility"`
	}
	if !s.decode(c, &input) {
		return
	}
	project, err := s.projects.Create(c.Request.Context(), actorFrom(c), projects.Input{Name: input.Name, Description: input.Description, RepoURL: input.RepoURL, DefaultBranch: input.DefaultBranch, BuildCommand: input.BuildCommand, Visibility: input.Visibility})
	if err != nil {
		s.projectError(c, err)
		return
	}
	s.respond(c, 201, project)
}

func (s *Server) getProject(c *gin.Context) {
	if s.projects == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	project, err := s.projects.Get(c.Request.Context(), actorFrom(c), c.Param("id"))
	if err != nil {
		s.projectError(c, err)
		return
	}
	s.respond(c, 200, project)
}

func (s *Server) updateProject(c *gin.Context) {
	if s.projects == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	var input struct {
		Name          *string `json:"name"`
		Description   *string `json:"description"`
		RepoURL       *string `json:"repoUrl"`
		DefaultBranch *string `json:"defaultBranch"`
		BuildCommand  *string `json:"buildCommand"`
		Visibility    *string `json:"visibility"`
		Archived      *bool   `json:"archived"`
	}
	if !s.decode(c, &input) {
		return
	}
	if input.Name == nil && input.Description == nil && input.RepoURL == nil && input.DefaultBranch == nil && input.BuildCommand == nil && input.Visibility == nil && input.Archived == nil {
		s.fail(c, 400, "invalid_request", "at least one project field is required")
		return
	}
	project, err := s.projects.Update(c.Request.Context(), actorFrom(c), c.Param("id"), projects.Patch{Name: input.Name, Description: input.Description, RepoURL: input.RepoURL, DefaultBranch: input.DefaultBranch, BuildCommand: input.BuildCommand, Visibility: input.Visibility, Archived: input.Archived})
	if err != nil {
		s.projectError(c, err)
		return
	}
	s.respond(c, 200, project)
}

// memberError maps membership failures; the validation and conflict messages
// describe membership rather than project fields, so a client can tell why the
// request was rejected.
func (s *Server) memberError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, projects.ErrValidation):
		s.fail(c, 400, "validation_failed", "role must be maintainer or viewer; the project creator cannot be removed; ids must be valid")
	case errors.Is(err, projects.ErrConflict):
		s.fail(c, 409, "conflict", "user is already a member of this project")
	case errors.Is(err, projects.ErrForbidden):
		s.fail(c, 403, "forbidden", "only a project maintainer can manage members of a private project")
	case errors.Is(err, projects.ErrNotFound):
		s.fail(c, 404, "not_found", "resource not found")
	default:
		s.logger.ErrorContext(c.Request.Context(), "project member request failed", "requestId", requestID(c), "error", err)
		s.fail(c, 500, "internal_error", "internal server error")
	}
}

// listProjectMembers returns the members of a project the caller can see. The
// visibility check lives in the service, so an invisible project is a 404 here
// as well.
func (s *Server) listProjectMembers(c *gin.Context) {
	if s.projects == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	members, err := s.projects.ListMembers(c.Request.Context(), actorFrom(c), c.Param("id"))
	if err != nil {
		s.memberError(c, err)
		return
	}
	s.respond(c, 200, members)
}

func (s *Server) addProjectMember(c *gin.Context) {
	if s.projects == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	var input struct {
		UserID string `json:"userId"`
		Role   string `json:"role"`
	}
	if !s.decode(c, &input) {
		return
	}
	member, err := s.projects.AddMember(c.Request.Context(), actorFrom(c), c.Param("id"), input.UserID, input.Role)
	if err != nil {
		s.memberError(c, err)
		return
	}
	s.respond(c, 201, member)
}

func (s *Server) updateProjectMember(c *gin.Context) {
	if s.projects == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	var input struct {
		Role string `json:"role"`
	}
	if !s.decode(c, &input) {
		return
	}
	member, err := s.projects.UpdateMemberRole(c.Request.Context(), actorFrom(c), c.Param("id"), c.Param("userId"), input.Role)
	if err != nil {
		s.memberError(c, err)
		return
	}
	s.respond(c, 200, member)
}

func (s *Server) removeProjectMember(c *gin.Context) {
	if s.projects == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	// No request body: the target and the project are both in the path.
	if err := s.projects.RemoveMember(c.Request.Context(), actorFrom(c), c.Param("id"), c.Param("userId")); err != nil {
		s.memberError(c, err)
		return
	}
	s.respond(c, 200, gin.H{"removed": true})
}

// buildError maps pipeline errors; the pipeline is optional infrastructure
// and fails closed when not wired.
func (s *Server) buildError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, builds.ErrValidation):
		s.fail(c, 400, "validation_failed", "project has no build command configured, or invalid ids or branch")
	case errors.Is(err, builds.ErrConflict):
		s.fail(c, 409, "conflict", "archived project cannot start builds, only queued or running builds can be cancelled, and only terminal builds can be retried")
	case errors.Is(err, projects.ErrForbidden):
		s.fail(c, 403, "forbidden", "only a project maintainer can start or cancel builds of a private project")
	case errors.Is(err, builds.ErrNotFound):
		s.fail(c, 404, "not_found", "resource not found")
	default:
		s.logger.ErrorContext(c.Request.Context(), "build request failed", "requestId", requestID(c), "error", err)
		s.fail(c, 500, "internal_error", "internal server error")
	}
}

func (s *Server) triggerBuild(c *gin.Context) {
	if s.builds == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	var input struct {
		Branch string `json:"branch"`
	}
	if !s.decode(c, &input) {
		return
	}
	build, err := s.builds.Trigger(c.Request.Context(), actorFrom(c), c.Param("id"), input.Branch)
	if err != nil {
		s.buildError(c, err)
		return
	}
	s.respond(c, 201, build)
}

func (s *Server) listBuilds(c *gin.Context) {
	if s.builds == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	limit, offset, ok := s.page(c)
	if !ok {
		return
	}
	list, total, err := s.builds.List(c.Request.Context(), actorFrom(c), c.Query("projectId"), limit, offset)
	if err != nil {
		s.buildError(c, err)
		return
	}
	s.respond(c, 200, gin.H{"items": list, "total": total, "limit": limit, "offset": offset})
}

func (s *Server) getBuild(c *gin.Context) {
	if s.builds == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	build, err := s.builds.Get(c.Request.Context(), actorFrom(c), c.Param("id"))
	if err != nil {
		s.buildError(c, err)
		return
	}
	s.respond(c, 200, build)
}

func (s *Server) getBuildLogs(c *gin.Context) {
	if s.builds == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	after := int64(0)
	if value := c.Query("after"); value != "" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed < 0 {
			s.fail(c, 400, "invalid_request", "after must be a non-negative integer")
			return
		}
		after = parsed
	}
	limit := 200
	if value := c.Query("limit"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 1000 {
			s.fail(c, 400, "invalid_request", "limit must be an integer between 1 and 1000")
			return
		}
		limit = parsed
	}
	buildID := c.Param("id")
	events, err := s.builds.Logs(c.Request.Context(), actorFrom(c), buildID, after, limit)
	if err != nil {
		s.buildError(c, err)
		return
	}
	s.respond(c, 200, gin.H{"items": events})
}

// sseMaxConnections caps concurrent build-event streams. Every stream runs a
// goroutine that queries PostgreSQL twice per 500ms poll, so an unbounded count
// lets slow clients exhaust goroutines and database connections. It is a
// variable so tests can shrink it and restore the default.
var sseMaxConnections int64 = 64

const (
	// ssePollInterval is how often a stream checks PostgreSQL for new events.
	ssePollInterval = 500 * time.Millisecond
	// sseHeartbeat keeps proxies and browsers from closing an idle stream.
	sseHeartbeat = 15 * time.Second
	// sseTailQuiet closes a finished build's stream once its output has been
	// quiet this long: the terminal status commits before the worker flushes
	// its last log batch.
	sseTailQuiet = 3 * time.Second
	// sseMaxLifetime forces a reconnect so one connection cannot live forever.
	sseMaxLifetime = 10 * time.Minute
	// sseLogBatch bounds the events carried by one frame.
	sseLogBatch = 500
	// sseWriteTimeout bounds one frame write so a client that stops reading
	// cannot pin the stream goroutine in a blocking write.
	sseWriteTimeout = 10 * time.Second
)

// acquireSSEConnection reserves one of the sseMaxConnections stream slots; false
// means the server is at capacity and the caller should fail fast.
func (s *Server) acquireSSEConnection() bool {
	if s.sseConnections.Add(1) > sseMaxConnections {
		s.sseConnections.Add(-1)
		return false
	}
	return true
}

// releaseSSEConnection returns a slot reserved by acquireSSEConnection.
func (s *Server) releaseSSEConnection() { s.sseConnections.Add(-1) }

// streamBuildEvents pushes log events and status changes as Server-Sent
// Events. Fetch-based clients attach the bearer token, so the normal
// Authorization contract applies; Last-Event-ID (or ?after=) resumes a stream
// from the last event id the client saw.
func (s *Server) streamBuildEvents(c *gin.Context) {
	if s.builds == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	actor := actorFrom(c)
	build, err := s.builds.Get(c.Request.Context(), actor, c.Param("id"))
	if err != nil {
		s.buildError(c, err)
		return
	}
	after, ok := s.streamCursor(c)
	if !ok {
		return
	}
	// Reserve a slot only after the caller has been validated, and before any
	// response bytes are committed, so an over-capacity request still gets a
	// clean 503 envelope instead of a truncated event stream.
	if !s.acquireSSEConnection() {
		s.fail(c, 503, "not_ready", "build log stream capacity is full; retry later")
		return
	}
	defer s.releaseSSEConnection()
	// The server-wide WriteTimeout would kill a long-lived stream; clear it for
	// this response only. ResponseController unwraps Gin's writer.
	controller := http.NewResponseController(c.Writer)
	if err := controller.SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
		s.fail(c, 500, "internal_error", "streaming unavailable")
		return
	}
	c.Header("Content-Type", "text/event-stream; charset=utf-8")
	c.Header("X-Accel-Buffering", "no")
	c.Status(200)
	c.Writer.Flush()

	status := build.Status
	if !writeSSE(c, controller, "status", "", mustJSON(build)) {
		return
	}
	var quietSince time.Time
	lifetime := time.NewTimer(sseMaxLifetime)
	defer lifetime.Stop()
	poll := time.NewTicker(ssePollInterval)
	defer poll.Stop()
	heartbeat := time.NewTicker(sseHeartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case <-c.Request.Context().Done():
			return
		case <-s.shutdownCtx.Done():
			// http.Server.Shutdown does not cancel in-flight request contexts,
			// so without this branch an open stream would hold the drain until
			// the shutdown deadline expires and the process exits non-zero.
			// Tell the client why the stream ended and return immediately.
			writeSSE(c, controller, "done", "", []byte(`{"reason":"shutdown"}`))
			return
		case <-lifetime.C:
			writeSSE(c, controller, "done", "", []byte(`{"reason":"reconnect"}`))
			return
		case <-heartbeat.C:
			sseArmWriteDeadline(controller)
			if _, err := c.Writer.WriteString(": ping\n\n"); err != nil {
				return
			}
			c.Writer.Flush()
		case <-poll.C:
			events, err := s.builds.Logs(c.Request.Context(), actor, build.ID, after, sseLogBatch)
			if err != nil {
				return
			}
			if len(events) > 0 {
				after = events[len(events)-1].ID
				if !writeSSE(c, controller, "logs", strconv.FormatInt(after, 10), mustJSON(events)) {
					return
				}
				quietSince = time.Time{}
			}
			current, err := s.builds.Get(c.Request.Context(), actor, build.ID)
			if err != nil {
				return
			}
			if current.Status != status {
				status = current.Status
				if !writeSSE(c, controller, "status", "", mustJSON(current)) {
					return
				}
				quietSince = time.Time{}
			}
			if builds.Terminal(status) && len(events) == 0 {
				if quietSince.IsZero() {
					quietSince = time.Now()
				} else if time.Since(quietSince) >= sseTailQuiet {
					writeSSE(c, controller, "done", "", []byte(`{"reason":"terminal"}`))
					return
				}
			}
		}
	}
}

// sseArmWriteDeadline bounds the next frame write. A ResponseWriter that cannot
// carry a deadline (HTTP/2 in some setups, or a bare recorder) reports
// http.ErrNotSupported; that degrades to the previous unbounded write instead of
// failing the stream.
func sseArmWriteDeadline(controller *http.ResponseController) {
	_ = controller.SetWriteDeadline(time.Now().Add(sseWriteTimeout))
}

// writeSSE writes one Server-Sent Events frame; false means the client is gone.
func writeSSE(c *gin.Context, controller *http.ResponseController, event, id string, data []byte) bool {
	sseArmWriteDeadline(controller)
	var frame strings.Builder
	if id != "" {
		frame.WriteString("id: ")
		frame.WriteString(id)
		frame.WriteString("\n")
	}
	if event != "" {
		frame.WriteString("event: ")
		frame.WriteString(event)
		frame.WriteString("\n")
	}
	if data != nil {
		for _, line := range strings.Split(string(data), "\n") {
			frame.WriteString("data: ")
			frame.WriteString(line)
			frame.WriteString("\n")
		}
	}
	frame.WriteString("\n")
	if _, err := c.Writer.WriteString(frame.String()); err != nil {
		return false
	}
	c.Writer.Flush()
	return true
}

func mustJSON(value any) []byte {
	data, err := json.Marshal(value)
	if err != nil {
		return []byte("null")
	}
	return data
}

// streamCursor reads the resume position from ?after= or Last-Event-ID.
func (s *Server) streamCursor(c *gin.Context) (int64, bool) {
	raw := c.Query("after")
	if raw == "" {
		raw = c.GetHeader("Last-Event-ID")
	}
	if raw == "" {
		return 0, true
	}
	after, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || after < 0 {
		s.fail(c, 400, "validation_failed", "after must be a non-negative event id")
		return 0, false
	}
	return after, true
}

func (s *Server) cancelBuild(c *gin.Context) {
	if s.builds == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	if !s.decode(c, &struct{}{}) {
		return
	}
	build, err := s.builds.Cancel(c.Request.Context(), actorFrom(c), c.Param("id"))
	if err != nil {
		s.buildError(c, err)
		return
	}
	s.respond(c, 200, build)
}

// retryBuild enqueues a new build for a terminal build's branch, using the
// project's current command; the original row stays as the historical record.
func (s *Server) retryBuild(c *gin.Context) {
	if s.builds == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	if !s.decode(c, &struct{}{}) {
		return
	}
	build, err := s.builds.Retry(c.Request.Context(), actorFrom(c), c.Param("id"))
	if err != nil {
		s.buildError(c, err)
		return
	}
	s.respond(c, 201, build)
}

// deploymentTargetError maps registry errors to the shared error envelope.
func (s *Server) deploymentTargetError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, deployments.ErrValidation):
		s.fail(c, 400, "validation_failed", "name 3-64 bytes on one line; kind http; environment development, staging or production; endpoint an http(s) URL without userinfo, query or fragment; healthPath and deployPath starting with /; secret at most 4096 bytes without control characters")
	case errors.Is(err, deployments.ErrConflict):
		s.fail(c, 409, "conflict", "deployment target name already exists, or the target has deployment history and cannot be removed")
	case errors.Is(err, deployments.ErrNotFound):
		s.fail(c, 404, "not_found", "resource not found")
	default:
		s.logger.ErrorContext(c.Request.Context(), "deployment target request failed", "requestId", requestID(c), "error", err)
		s.fail(c, 500, "internal_error", "internal server error")
	}
}

// listDeploymentTargetOptions returns the active targets a deployment can use.
// It is the only deployment-target read available to non-admin roles; the full
// list, mutations and probes stay admin-only.
func (s *Server) listDeploymentTargetOptions(c *gin.Context) {
	if s.deployments == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	options, err := s.deployments.TargetOptions(c.Request.Context())
	if err != nil {
		s.deploymentTargetError(c, err)
		return
	}
	s.respond(c, 200, gin.H{"items": options})
}

func (s *Server) listDeploymentTargets(c *gin.Context) {
	if s.deployments == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	limit, offset, ok := s.page(c)
	if !ok {
		return
	}
	targets, total, err := s.deployments.List(c.Request.Context(), actorFrom(c), limit, offset)
	if err != nil {
		s.deploymentTargetError(c, err)
		return
	}
	s.respond(c, 200, gin.H{"items": targets, "total": total, "limit": limit, "offset": offset})
}

func (s *Server) createDeploymentTarget(c *gin.Context) {
	if s.deployments == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	var input struct {
		Name        string `json:"name"`
		Kind        string `json:"kind"`
		Environment string `json:"environment"`
		Endpoint    string `json:"endpoint"`
		HealthPath  string `json:"healthPath"`
		DeployPath  string `json:"deployPath"`
		Secret      string `json:"secret"`
		Status      string `json:"status"`
	}
	if !s.decode(c, &input) {
		return
	}
	target, err := s.deployments.Create(c.Request.Context(), actorFrom(c), deployments.Input{
		Name: input.Name, Kind: input.Kind, Environment: input.Environment, Endpoint: input.Endpoint,
		HealthPath: input.HealthPath, DeployPath: input.DeployPath, Secret: input.Secret, Status: input.Status,
	})
	if err != nil {
		s.deploymentTargetError(c, err)
		return
	}
	s.respond(c, 201, target)
}

func (s *Server) getDeploymentTarget(c *gin.Context) {
	if s.deployments == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	target, err := s.deployments.Get(c.Request.Context(), actorFrom(c), c.Param("id"))
	if err != nil {
		s.deploymentTargetError(c, err)
		return
	}
	s.respond(c, 200, target)
}

func (s *Server) updateDeploymentTarget(c *gin.Context) {
	if s.deployments == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	var input struct {
		Name        *string `json:"name"`
		Environment *string `json:"environment"`
		Endpoint    *string `json:"endpoint"`
		HealthPath  *string `json:"healthPath"`
		DeployPath  *string `json:"deployPath"`
		Secret      *string `json:"secret"`
		Status      *string `json:"status"`
	}
	if !s.decode(c, &input) {
		return
	}
	if input.Name == nil && input.Environment == nil && input.Endpoint == nil && input.HealthPath == nil && input.DeployPath == nil && input.Secret == nil && input.Status == nil {
		s.fail(c, 400, "invalid_request", "at least one deployment target field is required")
		return
	}
	target, err := s.deployments.Update(c.Request.Context(), actorFrom(c), c.Param("id"), deployments.Patch{
		Name: input.Name, Environment: input.Environment, Endpoint: input.Endpoint,
		HealthPath: input.HealthPath, DeployPath: input.DeployPath, Secret: input.Secret, Status: input.Status,
	})
	if err != nil {
		s.deploymentTargetError(c, err)
		return
	}
	s.respond(c, 200, target)
}

func (s *Server) removeDeploymentTarget(c *gin.Context) {
	if s.deployments == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	// No request body: the target is in the path.
	if err := s.deployments.Remove(c.Request.Context(), actorFrom(c), c.Param("id")); err != nil {
		s.deploymentTargetError(c, err)
		return
	}
	s.respond(c, 200, gin.H{"removed": true})
}

func (s *Server) checkDeploymentTarget(c *gin.Context) {
	if s.deployments == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	if !s.decode(c, &struct{}{}) {
		return
	}
	result, err := s.deployments.Check(c.Request.Context(), actorFrom(c), c.Param("id"))
	if err != nil {
		s.deploymentTargetError(c, err)
		return
	}
	s.respond(c, 200, result)
}

// deploymentError maps deploy-task errors to the shared error envelope.
func (s *Server) deploymentError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, deployments.ErrValidation):
		s.fail(c, 400, "validation_failed", "project, target, version, buildId, reviewId or approval reason is invalid, the target is disabled, an approved review of this project is required (environment or project policy), the project policy may require the latest scan verdict to be passed, no earlier succeeded version can be rolled back to, or the request is not acceptable")
	case errors.Is(err, deployments.ErrConflict):
		s.fail(c, 409, "conflict", "deployment or approval is already terminal, is not in the expected state for this operation, or the deployment is no longer awaiting approval")
	case errors.Is(err, deployments.ErrNotFound):
		s.fail(c, 404, "not_found", "resource not found")
	case errors.Is(err, deployments.ErrForbidden):
		s.fail(c, 403, "forbidden", "the caller may see this project but not deploy to it")
	default:
		s.logger.ErrorContext(c.Request.Context(), "deployment request failed", "requestId", requestID(c), "error", err)
		s.fail(c, 500, "internal_error", "internal server error")
	}
}

func (s *Server) createDeployment(c *gin.Context) {
	if s.deployments == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	var input struct {
		TargetID string `json:"targetId"`
		Version  string `json:"version"`
		BuildID  string `json:"buildId"`
		ReviewID string `json:"reviewId"`
	}
	if !s.decode(c, &input) {
		return
	}
	deployment, err := s.deployments.CreateDeployment(c.Request.Context(), actorFrom(c), c.Param("id"), deployments.DeployInput{
		TargetID: input.TargetID, Version: input.Version, BuildID: input.BuildID, ReviewID: input.ReviewID,
	})
	if err != nil {
		s.deploymentError(c, err)
		return
	}
	s.respond(c, 201, deployment)
}

func (s *Server) listDeployments(c *gin.Context) {
	if s.deployments == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	limit, offset, ok := s.page(c)
	if !ok {
		return
	}
	list, total, err := s.deployments.ListDeployments(c.Request.Context(), actorFrom(c), c.Param("id"), c.Query("status"), limit, offset)
	if err != nil {
		s.deploymentError(c, err)
		return
	}
	s.respond(c, 200, gin.H{"items": list, "total": total, "limit": limit, "offset": offset})
}

// listDeploymentsGlobal is the admin-wide deployment list. It mirrors the build
// list shape so the deployments page can paginate server-side; projectId and
// status are optional filters.
func (s *Server) listDeploymentsGlobal(c *gin.Context) {
	if s.deployments == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	limit, offset, ok := s.page(c)
	if !ok {
		return
	}
	list, total, err := s.deployments.ListDeployments(c.Request.Context(), actorFrom(c), c.Query("projectId"), c.Query("status"), limit, offset)
	if err != nil {
		s.deploymentError(c, err)
		return
	}
	s.respond(c, 200, gin.H{"items": list, "total": total, "limit": limit, "offset": offset})
}

func (s *Server) getDeployment(c *gin.Context) {
	if s.deployments == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	deployment, err := s.deployments.GetDeployment(c.Request.Context(), actorFrom(c), c.Param("id"))
	if err != nil {
		s.deploymentError(c, err)
		return
	}
	s.respond(c, 200, deployment)
}

func (s *Server) getDeploymentEvents(c *gin.Context) {
	if s.deployments == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	after := int64(0)
	if value := c.Query("after"); value != "" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed < 0 {
			s.fail(c, 400, "invalid_request", "after must be a non-negative integer")
			return
		}
		after = parsed
	}
	limit := 200
	if value := c.Query("limit"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 1000 {
			s.fail(c, 400, "invalid_request", "limit must be an integer between 1 and 1000")
			return
		}
		limit = parsed
	}
	events, err := s.deployments.DeploymentEvents(c.Request.Context(), actorFrom(c), c.Param("id"), after, limit)
	if err != nil {
		s.deploymentError(c, err)
		return
	}
	s.respond(c, 200, gin.H{"items": events})
}

func (s *Server) cancelDeployment(c *gin.Context) {
	if s.deployments == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	if !s.decode(c, &struct{}{}) {
		return
	}
	deployment, err := s.deployments.CancelDeployment(c.Request.Context(), actorFrom(c), c.Param("id"))
	if err != nil {
		s.deploymentError(c, err)
		return
	}
	s.respond(c, 200, deployment)
}

func (s *Server) rollbackDeployment(c *gin.Context) {
	if s.deployments == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	// reviewId is optional here for the same reason as on create: environments
	// without the review gate keep the previous empty-body rollback behavior.
	var input struct {
		ReviewID string `json:"reviewId"`
	}
	if !s.decode(c, &input) {
		return
	}
	deployment, err := s.deployments.RollbackDeployment(c.Request.Context(), actorFrom(c), c.Param("id"), input.ReviewID)
	if err != nil {
		s.deploymentError(c, err)
		return
	}
	s.respond(c, 201, deployment)
}

func (s *Server) listApprovals(c *gin.Context) {
	if s.deployments == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	limit, offset, ok := s.page(c)
	if !ok {
		return
	}
	list, total, err := s.deployments.ListApprovals(c.Request.Context(), actorFrom(c), c.Query("status"), limit, offset)
	if err != nil {
		s.deploymentError(c, err)
		return
	}
	s.respond(c, 200, gin.H{"items": list, "total": total, "limit": limit, "offset": offset})
}

func (s *Server) approveApproval(c *gin.Context) {
	if s.deployments == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	if !s.decode(c, &struct{}{}) {
		return
	}
	approval, err := s.deployments.Approve(c.Request.Context(), actorFrom(c), c.Param("id"))
	if err != nil {
		s.deploymentError(c, err)
		return
	}
	s.respond(c, 200, approval)
}

func (s *Server) rejectApproval(c *gin.Context) {
	if s.deployments == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	var input struct {
		Reason string `json:"reason"`
	}
	if !s.decode(c, &input) {
		return
	}
	approval, err := s.deployments.Reject(c.Request.Context(), actorFrom(c), c.Param("id"), input.Reason)
	if err != nil {
		s.deploymentError(c, err)
		return
	}
	s.respond(c, 200, approval)
}

// reviewError maps review-center errors to the shared error envelope. The
// messages describe the contract (subject/reason bounds, reviewer matrix, state
// machine) without echoing user content.
func (s *Server) reviewError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, reviews.ErrValidation):
		s.fail(c, 400, "validation_failed", "subject 3-200 bytes on one line, description at most 2000 bytes, version at most 64 bytes on one line, externalUrl an http(s) URL without userinfo or fragment, and reject requires a reason of at most 512 bytes on one line")
	case errors.Is(err, reviews.ErrConflict):
		s.fail(c, 409, "conflict", "review is already terminal or is not in the expected state for this operation")
	case errors.Is(err, reviews.ErrForbidden):
		s.fail(c, 403, "forbidden", "the caller may see this project but may not write it, or is not a project maintainer or global admin for review decisions")
	case errors.Is(err, reviews.ErrNotFound):
		s.fail(c, 404, "not_found", "resource not found")
	default:
		s.logger.ErrorContext(c.Request.Context(), "review request failed", "requestId", requestID(c), "error", err)
		s.fail(c, 500, "internal_error", "internal server error")
	}
}

func (s *Server) createReview(c *gin.Context) {
	if s.reviews == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	var input struct {
		Subject     string `json:"subject"`
		Description string `json:"description"`
		Version     string `json:"version"`
		ExternalURL string `json:"externalUrl"`
	}
	if !s.decode(c, &input) {
		return
	}
	review, err := s.reviews.Create(c.Request.Context(), actorFrom(c), c.Param("id"), reviews.Input{
		Subject: input.Subject, Description: input.Description, Version: input.Version, ExternalURL: input.ExternalURL,
	})
	if err != nil {
		s.reviewError(c, err)
		return
	}
	s.respond(c, 201, review)
}

// listReviews pages the reviews of one visible project, newest first; status is
// an optional filter.
func (s *Server) listReviews(c *gin.Context) {
	if s.reviews == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	limit, offset, ok := s.page(c)
	if !ok {
		return
	}
	list, total, err := s.reviews.List(c.Request.Context(), actorFrom(c), c.Param("id"), c.Query("status"), limit, offset)
	if err != nil {
		s.reviewError(c, err)
		return
	}
	s.respond(c, 200, gin.H{"items": list, "total": total, "limit": limit, "offset": offset})
}

// listReviewsGlobal pages every review the caller may see, newest first, with
// optional status and projectId filters. It mirrors the global deployment list:
// the visibility predicate lives in the service, so an invisible project filter
// yields an empty page instead of an error and review ids stay non-enumerable.
func (s *Server) listReviewsGlobal(c *gin.Context) {
	if s.reviews == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	limit, offset, ok := s.page(c)
	if !ok {
		return
	}
	list, total, err := s.reviews.ListVisible(c.Request.Context(), actorFrom(c), reviews.ListFilter{
		ProjectID: c.Query("projectId"), Status: c.Query("status"), Limit: limit, Offset: offset,
	})
	if err != nil {
		s.reviewError(c, err)
		return
	}
	s.respond(c, 200, gin.H{"items": list, "total": total, "limit": limit, "offset": offset})
}

func (s *Server) getReview(c *gin.Context) {
	if s.reviews == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	review, err := s.reviews.Get(c.Request.Context(), actorFrom(c), c.Param("id"))
	if err != nil {
		s.reviewError(c, err)
		return
	}
	s.respond(c, 200, review)
}

func (s *Server) approveReview(c *gin.Context) {
	if s.reviews == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	var input struct {
		Reason string `json:"reason"`
	}
	if !s.decode(c, &input) {
		return
	}
	review, err := s.reviews.Approve(c.Request.Context(), actorFrom(c), c.Param("id"), input.Reason)
	if err != nil {
		s.reviewError(c, err)
		return
	}
	s.respond(c, 200, review)
}

func (s *Server) rejectReview(c *gin.Context) {
	if s.reviews == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	var input struct {
		Reason string `json:"reason"`
	}
	if !s.decode(c, &input) {
		return
	}
	review, err := s.reviews.Reject(c.Request.Context(), actorFrom(c), c.Param("id"), input.Reason)
	if err != nil {
		s.reviewError(c, err)
		return
	}
	s.respond(c, 200, review)
}

func (s *Server) cancelReview(c *gin.Context) {
	if s.reviews == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	if !s.decode(c, &struct{}{}) {
		return
	}
	review, err := s.reviews.Cancel(c.Request.Context(), actorFrom(c), c.Param("id"))
	if err != nil {
		s.reviewError(c, err)
		return
	}
	s.respond(c, 200, review)
}

func (s *Server) changePassword(c *gin.Context) {
	var input struct {
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
	}
	if !s.decode(c, &input) {
		return
	}
	user := currentUser(c)
	if err := s.auth.ChangePassword(c.Request.Context(), actorFrom(c), user.ID, input.CurrentPassword, input.NewPassword, currentSessionID(c)); err != nil {
		s.authError(c, err)
		return
	}
	s.respond(c, 200, gin.H{"changed": true})
}

func (s *Server) listSessions(c *gin.Context) {
	user := currentUser(c)
	sessions, err := s.auth.ListSessions(c.Request.Context(), user.ID, currentSessionID(c))
	if err != nil {
		s.authError(c, err)
		return
	}
	s.respond(c, 200, sessions)
}

func (s *Server) revokeSession(c *gin.Context) {
	id := c.Param("id")
	if !auth.ValidID(id) {
		s.fail(c, 400, "invalid_request", "invalid session id")
		return
	}
	if err := s.auth.RevokeSession(c.Request.Context(), actorFrom(c), currentUser(c).ID, id); err != nil {
		s.authError(c, err)
		return
	}
	s.respond(c, 200, gin.H{"revoked": true})
}

func (s *Server) revokeOtherSessions(c *gin.Context) {
	user := currentUser(c)
	revoked, err := s.auth.RevokeOtherSessions(c.Request.Context(), actorFrom(c), user.ID, currentSessionID(c))
	if err != nil {
		s.authError(c, err)
		return
	}
	s.respond(c, 200, gin.H{"revoked": revoked})
}

// decodeBodyLimit is the shared request-body bound of most JSON endpoints.
const decodeBodyLimit = 16 << 10

// Keep strict per-request decoding rather than changing Gin's global binding settings.
func (s *Server) decode(c *gin.Context, v any) bool {
	return s.decodeWithLimit(c, v, decodeBodyLimit, "16 KiB")
}

// decodeWithLimit applies the shared strict JSON rules with an explicit
// transport bound. Endpoints that legitimately accept a larger document (the
// knowledge base stores bodies up to 100 KiB, whose JSON escaping can exceed
// the default) use it; the label appears in the shared error messages. The
// service layer still enforces the business bound on the decoded value.
func (s *Server) decodeWithLimit(c *gin.Context, v any, limit int64, label string) bool {
	media, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || media != "application/json" {
		s.fail(c, 415, "unsupported_media_type", "Content-Type must be application/json")
		return false
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		s.fail(c, 400, "invalid_request", "body must contain one JSON object of at most "+label)
		return false
	}
	return s.decodeJSON(c, raw, v, label)
}

// decodeRawJSON applies the shared strict JSON rules to an already-read body.
// The signed external-result endpoint must verify its HMAC over the raw bytes
// before decoding, so both entry points share the exact same acceptance rules:
// one JSON object, no unknown or null fields, and no second document.
func (s *Server) decodeRawJSON(c *gin.Context, raw []byte, v any) bool {
	return s.decodeJSON(c, raw, v, "16 KiB")
}

// decodeJSON is decodeRawJSON with the transport label used in its error
// messages, so a larger-body endpoint can report its real bound.
func (s *Server) decodeJSON(c *gin.Context, raw []byte, v any, label string) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		s.fail(c, 400, "invalid_request", "body must contain one JSON object of at most "+label)
		return false
	}
	// Decode via RawMessage first so null/arrays/scalars cannot masquerade as objects.
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var document json.RawMessage
	if err := decoder.Decode(&document); err != nil {
		s.fail(c, 400, "invalid_request", "body must contain one JSON object of at most "+label)
		return false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		s.fail(c, 400, "invalid_request", "body must contain one JSON object")
		return false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil {
		s.fail(c, 400, "invalid_request", "invalid JSON object")
		return false
	}
	for _, value := range fields {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			s.fail(c, 400, "invalid_request", "fields must not be null")
			return false
		}
	}
	strict := json.NewDecoder(bytes.NewReader(raw))
	strict.DisallowUnknownFields()
	if err := strict.Decode(v); err != nil {
		s.fail(c, 400, "invalid_request", "invalid JSON body or body exceeds "+label)
		return false
	}
	if err := strict.Decode(new(any)); err != io.EOF {
		s.fail(c, 400, "invalid_request", "body must contain one JSON object")
		return false
	}
	return true
}
func (s *Server) authError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, auth.ErrUnauthorized):
		c.Header("WWW-Authenticate", "Bearer")
		s.fail(c, 401, "unauthorized", "invalid credentials or expired session")
	case errors.Is(err, auth.ErrForbidden):
		s.fail(c, 403, "forbidden", "permission denied")
	case errors.Is(err, auth.ErrConflict):
		s.fail(c, 409, "conflict", "username already exists")
	case errors.Is(err, auth.ErrValidation):
		s.fail(c, 400, "validation_failed", "username must be 3-64 ASCII letters, digits, underscore, dot or hyphen; password 12-72 bytes; role admin/member/viewer")
	case errors.Is(err, auth.ErrNotFound):
		s.fail(c, 404, "not_found", "resource not found")
	case errors.Is(err, auth.ErrAccountDisabled):
		s.fail(c, 403, "account_disabled", "account is disabled; contact an administrator")
	default:
		s.logger.ErrorContext(c.Request.Context(), "request failed", "requestId", c.Request.Context().Value(requestIDKey), "error", err)
		s.fail(c, 500, "internal_error", "internal server error")
	}
}
func (s *Server) respond(c *gin.Context, status int, data any) {
	c.JSON(status, gin.H{"data": data, "requestId": c.Request.Context().Value(requestIDKey)})
}
func (s *Server) fail(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{"code": code, "message": message}, "requestId": c.Request.Context().Value(requestIDKey)})
}
func (s *Server) middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		var bytes [16]byte
		if _, err := rand.Read(bytes[:]); err != nil {
			s.fail(c, 500, "internal_error", "internal server error")
			return
		}
		id := hex.EncodeToString(bytes[:])
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), requestIDKey, id))
		c.Header("X-Request-ID", id)
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("Cache-Control", "no-store")
		c.Header("Referrer-Policy", "no-referrer")
		start := time.Now()
		defer func() {
			if p := recover(); p != nil {
				s.logger.Error("http panic", "requestId", id)
				c.Abort()
				if !c.Writer.Written() {
					s.fail(c, 500, "internal_error", "internal server error")
				}
			}
			// Metrics carry the matched route template, never the raw path, so
			// ids and free-form segments cannot leak into labels.
			route := c.FullPath()
			if route == "" {
				route = "unmatched"
			}
			s.metrics.ObserveHTTP(c.Request.Method, route, c.Writer.Status(), time.Since(start))
			s.logger.Info("http request", "requestId", id, "method", c.Request.Method, "path", c.Request.URL.Path, "status", c.Writer.Status(), "durationMs", time.Since(start).Milliseconds())
		}()
		// Vary also on denied/missing origins so intermediaries cannot reuse CORS decisions.
		c.Writer.Header().Add("Vary", "Origin")
		c.Writer.Header().Add("Vary", "Sec-Fetch-Site")
		// Cross-site requests are rejected globally, with one documented
		// exception: the OIDC provider redirects the browser back with a
		// top-level cross-site GET navigation, which is the normal SSO return
		// path. The callback is protected by the one-time state cookie plus the
		// atomically consumed state row instead, and it mutates nothing before
		// both validate.
		if strings.EqualFold(strings.TrimSpace(c.GetHeader("Sec-Fetch-Site")), "cross-site") && !isOIDCCallbackNavigation(c.Request) {
			s.fail(c, 403, "origin_denied", "cross-site requests are not allowed")
			return
		}
		origin := c.GetHeader("Origin")
		if c.Request.Method == http.MethodPost && (c.Request.URL.Path == refreshCookiePath || strings.HasPrefix(c.Request.URL.Path, refreshCookiePath+"/")) && (origin == "" || len(c.Request.Header.Values("Origin")) != 1 || !s.origins[origin]) {
			s.fail(c, 403, "origin_denied", "an allowed Origin is required")
			return
		}
		if origin != "" {
			if !s.origins[origin] {
				s.fail(c, 403, "origin_denied", "origin not allowed")
				return
			}
			c.Header("Access-Control-Allow-Origin", origin)
			c.Header("Access-Control-Allow-Credentials", "true")
			// X-Audit-Truncated must be readable by the audit page's blob download;
			// Content-Disposition stays server-side because the client names the file.
			c.Header("Access-Control-Expose-Headers", "X-Request-ID, X-Audit-Truncated")
		}
		if c.Request.Method == http.MethodOptions {
			c.Writer.Header().Add("Vary", "Access-Control-Request-Method")
			c.Writer.Header().Add("Vary", "Access-Control-Request-Headers")
			c.Header("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
			c.Header("Access-Control-Allow-Headers", "Authorization, Content-Type")
			c.AbortWithStatus(204)
			return
		}
		c.Next()
	}
}
