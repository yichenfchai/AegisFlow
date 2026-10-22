package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
)

type contextKey int

const requestIDKey contextKey = 0
const userKey = "authenticatedUser"

type Server struct {
	auth    *auth.Service
	logger  *slog.Logger
	ready   func(context.Context) error
	origins map[string]bool
	limiter *loginLimiter
}

// New builds an isolated Gin engine; business services depend only on context.Context.
func New(service *auth.Service, logger *slog.Logger, ready func(context.Context) error, origins []string) *gin.Engine {
	s := &Server{auth: service, logger: logger, ready: ready, origins: map[string]bool{}, limiter: &loginLimiter{entries: map[string]attempt{}}}
	for _, o := range origins {
		s.origins[o] = true
	}
	router := gin.New()
	// Explicitly disable proxy trust. ClientIP must not trust arbitrary forwarding headers.
	_ = router.SetTrustedProxies(nil)
	router.RedirectTrailingSlash = false
	router.RedirectFixedPath = false
	router.Use(s.middleware())
	router.GET("/healthz", func(c *gin.Context) { s.respond(c, 200, gin.H{"status": "ok"}) })
	router.GET("/readyz", func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()
		if err := s.ready(ctx); err != nil {
			s.fail(c, 503, "not_ready", "database unavailable")
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
	users := api.Group("/users", s.authenticate(), s.requireAdmin())
	users.GET("", s.listUsers)
	users.POST("", s.createUser)
	router.NoRoute(func(c *gin.Context) { s.fail(c, 404, "not_found", "endpoint not found") })
	return router
}
func (s *Server) login(c *gin.Context) {
	if !s.limiter.allow(c.ClientIP(), time.Now()) {
		c.Header("Retry-After", "60")
		s.fail(c, 429, "rate_limited", "too many login attempts")
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !s.decode(c, &input) {
		return
	}
	ctx := c.Request.Context()
	tokens, err := s.auth.Login(ctx, input.Username, input.Password)
	s.logger.InfoContext(ctx, "auth.login", "requestId", ctx.Value(requestIDKey), "success", err == nil)
	if err != nil {
		s.authError(c, err)
		return
	}
	s.respond(c, 200, tokens)
}
func (s *Server) refresh(c *gin.Context) {
	var input struct {
		RefreshToken string `json:"refreshToken"`
	}
	if !s.decode(c, &input) {
		return
	}
	tokens, err := s.auth.Refresh(c.Request.Context(), input.RefreshToken)
	if err != nil {
		s.authError(c, err)
		return
	}
	s.respond(c, 200, tokens)
}
func (s *Server) logout(c *gin.Context) {
	var input struct {
		RefreshToken string `json:"refreshToken"`
	}
	if !s.decode(c, &input) {
		return
	}
	if err := s.auth.Logout(c.Request.Context(), input.RefreshToken); err != nil {
		s.authError(c, err)
		return
	}
	s.respond(c, 200, gin.H{"loggedOut": true})
}
func (s *Server) authenticate() gin.HandlerFunc {
	return func(c *gin.Context) {
		parts := strings.Fields(c.GetHeader("Authorization"))
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			c.Header("WWW-Authenticate", "Bearer")
			s.fail(c, 401, "unauthorized", "valid bearer token required")
			return
		}
		user, err := s.auth.Authenticate(c.Request.Context(), parts[1])
		if err != nil {
			s.authError(c, err)
			return
		}
		c.Set(userKey, user)
		c.Next()
	}
}
func (s *Server) requireAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		if currentUser(c).Role != "admin" {
			s.fail(c, 403, "forbidden", "administrator role required")
			return
		}
		c.Next()
	}
}
func currentUser(c *gin.Context) *auth.User { return c.MustGet(userKey).(*auth.User) }
func (s *Server) me(c *gin.Context)         { s.respond(c, 200, currentUser(c)) }
func (s *Server) listUsers(c *gin.Context) {
	users, err := s.auth.ListUsers(c.Request.Context())
	if err != nil {
		s.authError(c, err)
		return
	}
	s.respond(c, 200, users)
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
	user, err := s.auth.CreateUser(ctx, input.Username, input.Password, input.DisplayName, input.Role)
	s.logger.InfoContext(ctx, "auth.create_user", "actorId", currentUser(c).ID, "requestId", ctx.Value(requestIDKey), "success", err == nil)
	if err != nil {
		s.authError(c, err)
		return
	}
	s.respond(c, 201, user)
}

// Keep strict per-request decoding rather than changing Gin's global binding settings.
func (s *Server) decode(c *gin.Context, v any) bool {
	media, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || media != "application/json" {
		s.fail(c, 415, "unsupported_media_type", "Content-Type must be application/json")
		return false
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10)
	d := json.NewDecoder(c.Request.Body)
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
		origin := c.GetHeader("Origin")
		if origin != "" {
			c.Writer.Header().Add("Vary", "Origin")
			if !s.origins[origin] {
				s.fail(c, 403, "origin_denied", "origin not allowed")
				return
			}
			c.Header("Access-Control-Allow-Origin", origin)
			c.Header("Access-Control-Expose-Headers", "X-Request-ID")
		}
		if c.Request.Method == http.MethodOptions {
			c.Header("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			c.Header("Access-Control-Allow-Headers", "Authorization, Content-Type")
			c.AbortWithStatus(204)
			return
		}
		c.Next()
	}
}

type attempt struct {
	count int
	start time.Time
}
type loginLimiter struct {
	mu      sync.Mutex
	entries map[string]attempt
}

// The limiter uses the direct peer address. Untrusted forwarding headers are ignored.
func (l *loginLimiter) allow(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, v := range l.entries {
		if now.Sub(v.start) >= time.Minute {
			delete(l.entries, k)
		}
	}
	v := l.entries[ip]
	if v.start.IsZero() {
		if len(l.entries) >= 10000 {
			return false
		}
		v.start = now
	}
	if v.count >= 10 {
		return false
	}
	v.count++
	l.entries[ip] = v
	return true
}
