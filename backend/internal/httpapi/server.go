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
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
	"github.com/yichenfchai/AegisFlow/backend/internal/builds"
	"github.com/yichenfchai/AegisFlow/backend/internal/projects"
	"github.com/yichenfchai/AegisFlow/backend/internal/ratelimit"
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
	auth         *auth.Service
	projects     *projects.Service
	builds       *builds.Service
	logger       *slog.Logger
	ready        func(context.Context) error
	origins      map[string]bool
	limiter      ratelimit.Limiter
	cookieSecure bool
}

const refreshCookieName = "aegis_refresh"
const refreshCookiePath = "/api/v1/auth"

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
}

// New builds an isolated Gin engine; business services depend only on context.Context.
func New(service *auth.Service, logger *slog.Logger, ready func(context.Context) error, origins []string, options ...Options) *gin.Engine {
	var opts Options
	if len(options) > 0 {
		opts = options[0]
	}
	s := &Server{auth: service, projects: opts.Projects, builds: opts.Builds, logger: logger, ready: ready, origins: map[string]bool{}, limiter: opts.Limiter, cookieSecure: opts.CookieSecure}
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
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()
		if err := s.ready(ctx); err != nil {
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
	projectRoutes := api.Group("/projects", s.authenticate())
	projectRoutes.GET("", s.listProjects)
	projectRoutes.POST("", s.requireRole("admin", "member"), s.createProject)
	projectRoutes.GET("/:id", s.getProject)
	projectRoutes.PATCH("/:id", s.requireRole("admin", "member"), s.updateProject)
	projectRoutes.POST("/:id/builds", s.requireRole("admin", "member"), s.triggerBuild)
	buildRoutes := api.Group("/builds", s.authenticate())
	buildRoutes.GET("", s.listBuilds)
	buildRoutes.GET("/:id", s.getBuild)
	buildRoutes.GET("/:id/logs", s.getBuildLogs)
	buildRoutes.GET("/:id/events", s.streamBuildEvents)
	buildRoutes.POST("/:id/cancel", s.requireRole("admin", "member"), s.cancelBuild)
	buildRoutes.POST("/:id/retry", s.requireRole("admin", "member"), s.retryBuild)
	router.NoRoute(func(c *gin.Context) { s.fail(c, 404, "not_found", "endpoint not found") })
	return router
}
func (s *Server) login(c *gin.Context) {
	// No in-process fallback: all instances must share the same quota.
	if s.limiter == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	allowed, retryAfter, err := s.limiter.Allow(c.Request.Context(), c.ClientIP())
	if err != nil {
		s.logger.ErrorContext(c.Request.Context(), "login limiter unavailable", "requestId", c.Request.Context().Value(requestIDKey), "error", err)
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
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

// actorFrom builds the audit actor of the authenticated caller.
func actorFrom(c *gin.Context) auth.Actor {
	user := currentUser(c)
	return auth.Actor{ID: user.ID, Username: user.Username, RequestID: requestID(c)}
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
	entries, total, err := s.auth.ListAudit(c.Request.Context(), limit, offset)
	if err != nil {
		s.authError(c, err)
		return
	}
	s.respond(c, 200, gin.H{"items": entries, "total": total, "limit": limit, "offset": offset})
}

// projectError maps registry errors to the shared error envelope.
func (s *Server) projectError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, projects.ErrValidation):
		s.fail(c, 400, "validation_failed", "name 3-64 bytes without path or meta characters; repoUrl https://, git@, file:/// URL or absolute path; defaultBranch a plain git ref")
	case errors.Is(err, projects.ErrConflict):
		s.fail(c, 409, "conflict", "project name already exists or the project is archived")
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
	list, total, err := s.projects.List(c.Request.Context(), status, limit, offset)
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
	}
	if !s.decode(c, &input) {
		return
	}
	project, err := s.projects.Create(c.Request.Context(), actorFrom(c), projects.Input{Name: input.Name, Description: input.Description, RepoURL: input.RepoURL, DefaultBranch: input.DefaultBranch, BuildCommand: input.BuildCommand})
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
	project, err := s.projects.Get(c.Request.Context(), c.Param("id"))
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
		Archived      *bool   `json:"archived"`
	}
	if !s.decode(c, &input) {
		return
	}
	if input.Name == nil && input.Description == nil && input.RepoURL == nil && input.DefaultBranch == nil && input.BuildCommand == nil && input.Archived == nil {
		s.fail(c, 400, "invalid_request", "at least one project field is required")
		return
	}
	project, err := s.projects.Update(c.Request.Context(), actorFrom(c), c.Param("id"), projects.Patch{Name: input.Name, Description: input.Description, RepoURL: input.RepoURL, DefaultBranch: input.DefaultBranch, BuildCommand: input.BuildCommand, Archived: input.Archived})
	if err != nil {
		s.projectError(c, err)
		return
	}
	s.respond(c, 200, project)
}

// buildError maps pipeline errors; the pipeline is optional infrastructure
// and fails closed when not wired.
func (s *Server) buildError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, builds.ErrValidation):
		s.fail(c, 400, "validation_failed", "project has no build command configured, or invalid ids or branch")
	case errors.Is(err, builds.ErrConflict):
		s.fail(c, 409, "conflict", "archived project cannot start builds, only queued or running builds can be cancelled, and only terminal builds can be retried")
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
	list, total, err := s.builds.List(c.Request.Context(), c.Query("projectId"), limit, offset)
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
	build, err := s.builds.Get(c.Request.Context(), c.Param("id"))
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
	if _, err := s.builds.Get(c.Request.Context(), buildID); err != nil {
		s.buildError(c, err)
		return
	}
	events, err := s.builds.Logs(c.Request.Context(), buildID, after, limit)
	if err != nil {
		s.buildError(c, err)
		return
	}
	s.respond(c, 200, gin.H{"items": events})
}

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
)

// streamBuildEvents pushes log events and status changes as Server-Sent
// Events. Fetch-based clients attach the bearer token, so the normal
// Authorization contract applies; Last-Event-ID (or ?after=) resumes a stream
// from the last event id the client saw.
func (s *Server) streamBuildEvents(c *gin.Context) {
	if s.builds == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	build, err := s.builds.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		s.buildError(c, err)
		return
	}
	after, ok := s.streamCursor(c)
	if !ok {
		return
	}
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
	if !writeSSE(c, "status", "", mustJSON(build)) {
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
		case <-lifetime.C:
			writeSSE(c, "done", "", []byte(`{"reason":"reconnect"}`))
			return
		case <-heartbeat.C:
			if _, err := c.Writer.WriteString(": ping\n\n"); err != nil {
				return
			}
			c.Writer.Flush()
		case <-poll.C:
			events, err := s.builds.Logs(c.Request.Context(), build.ID, after, sseLogBatch)
			if err != nil {
				return
			}
			if len(events) > 0 {
				after = events[len(events)-1].ID
				if !writeSSE(c, "logs", strconv.FormatInt(after, 10), mustJSON(events)) {
					return
				}
				quietSince = time.Time{}
			}
			current, err := s.builds.Get(c.Request.Context(), build.ID)
			if err != nil {
				return
			}
			if current.Status != status {
				status = current.Status
				if !writeSSE(c, "status", "", mustJSON(current)) {
					return
				}
				quietSince = time.Time{}
			}
			if builds.Terminal(status) && len(events) == 0 {
				if quietSince.IsZero() {
					quietSince = time.Now()
				} else if time.Since(quietSince) >= sseTailQuiet {
					writeSSE(c, "done", "", []byte(`{"reason":"terminal"}`))
					return
				}
			}
		}
	}
}

// writeSSE writes one Server-Sent Events frame; false means the client is gone.
func writeSSE(c *gin.Context, event, id string, data []byte) bool {
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

// Keep strict per-request decoding rather than changing Gin's global binding settings.
func (s *Server) decode(c *gin.Context, v any) bool {
	media, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || media != "application/json" {
		s.fail(c, 415, "unsupported_media_type", "Content-Type must be application/json")
		return false
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10)
	// Decode via RawMessage first so null/arrays/scalars cannot masquerade as objects.
	var raw json.RawMessage
	d := json.NewDecoder(c.Request.Body)
	if err := d.Decode(&raw); err != nil || len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		s.fail(c, 400, "invalid_request", "body must contain one JSON object of at most 16 KiB")
		return false
	}
	if err := d.Decode(new(any)); err != io.EOF {
		s.fail(c, 400, "invalid_request", "body must contain one JSON object")
		return false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		s.fail(c, 400, "invalid_request", "invalid JSON object")
		return false
	}
	for _, value := range fields {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			s.fail(c, 400, "invalid_request", "fields must not be null")
			return false
		}
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		s.fail(c, 400, "invalid_request", "invalid JSON body or body exceeds 16 KiB")
		return false
	}
	if err := d.Decode(new(any)); err != io.EOF {
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
			s.logger.Info("http request", "requestId", id, "method", c.Request.Method, "path", c.Request.URL.Path, "status", c.Writer.Status(), "durationMs", time.Since(start).Milliseconds())
		}()
		// Vary also on denied/missing origins so intermediaries cannot reuse CORS decisions.
		c.Writer.Header().Add("Vary", "Origin")
		c.Writer.Header().Add("Vary", "Sec-Fetch-Site")
		if strings.EqualFold(strings.TrimSpace(c.GetHeader("Sec-Fetch-Site")), "cross-site") {
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
			c.Header("Access-Control-Expose-Headers", "X-Request-ID")
		}
		if c.Request.Method == http.MethodOptions {
			c.Writer.Header().Add("Vary", "Access-Control-Request-Method")
			c.Writer.Header().Add("Vary", "Access-Control-Request-Headers")
			c.Header("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			c.Header("Access-Control-Allow-Headers", "Authorization, Content-Type")
			c.AbortWithStatus(204)
			return
		}
		c.Next()
	}
}
