package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"
	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
	"github.com/yichenfchai/AegisFlow/backend/internal/builds"
	"github.com/yichenfchai/AegisFlow/backend/internal/config"
	"github.com/yichenfchai/AegisFlow/backend/internal/deployments"
	"github.com/yichenfchai/AegisFlow/backend/internal/httpapi"
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

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}
func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	switch cfg.BuildSandbox {
	case config.BuildSandboxNone:
		// Not an error: the controlled execution environment is not enabled.
		// The warning keeps an operator from assuming isolation that does not exist.
		logger.Warn("build commands run as the service account without isolation", "sandbox", string(cfg.BuildSandbox))
	case config.BuildSandboxContainer:
		// Fail closed: an operator selecting the container sandbox must not get
		// a server that silently falls back to the service account.
		probeCtx, cancelProbe := context.WithTimeout(context.Background(), 15*time.Second)
		err = builds.ProbeContainerRuntime(probeCtx)
		cancelProbe()
		if err != nil {
			return err
		}
		logger.Info("build commands run in a one-shot container",
			"sandbox", string(cfg.BuildSandbox), "image", cfg.BuildImage,
			"cpus", cfg.BuildCPU, "memory", cfg.BuildMemory, "pids", cfg.BuildPids)
	case config.BuildSandboxUser:
		// The user sandbox can only honour its promise when the host can drop
		// privileges; anything else must stop the server at startup.
		if err = builds.ProbeUserSandbox(cfg.BuildUser); err != nil {
			return err
		}
		logger.Info("build commands run as a dedicated OS account",
			"sandbox", string(cfg.BuildSandbox), "user", cfg.BuildUser)
	}
	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		return errors.New("invalid PostgreSQL connection configuration")
	}
	defer db.Close()
	db.SetMaxOpenConns(cfg.DBMaxOpen)
	db.SetMaxIdleConns(cfg.DBMaxIdle)
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)
	redisOptions, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return errors.New("invalid Redis configuration")
	}
	redisOptions.DialTimeout = 3 * time.Second
	redisOptions.ReadTimeout = 2 * time.Second
	redisOptions.WriteTimeout = 2 * time.Second
	redisOptions.MaxRetries = 0
	cache := redis.NewClient(redisOptions)
	defer cache.Close()
	ready := func(ctx context.Context) error {
		if err := db.PingContext(ctx); err != nil {
			return errors.New("PostgreSQL unavailable")
		}
		if err := cache.Ping(ctx).Err(); err != nil {
			return errors.New("Redis unavailable")
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err = ready(ctx); err != nil {
		return err
	}
	service := auth.New(db, cfg.JWTSecret, cfg.AccessTTL, cfg.RefreshTTL)
	if err = service.Migrate(ctx); err != nil {
		return err
	}
	// Guarantee the deployment's default tenant before anything can reference
	// it: the first start after migration may rename the seeded slug to
	// AEGIS_DEFAULT_TENANT_SLUG, and a configured slug with no tenant fails
	// startup instead of leaving the deployment unable to log anyone in.
	defaultTenant, err := service.EnsureDefaultTenant(ctx, cfg.DefaultTenantSlug)
	if err != nil {
		return err
	}
	if cfg.BootstrapUsername != "" {
		if err = service.Bootstrap(ctx, cfg.BootstrapUsername, cfg.BootstrapPassword); err != nil {
			return err
		}
	}
	registry := projects.New(db)
	// Per-project automation policy (P5 batch C): gate rules stored in the
	// database, not the environment. The deployment service reads them through
	// the injected gate interface; the review environment gate above stays.
	policyStore := policies.New(db, registry)
	// The review center is the gate record for change reviews: creation follows
	// the project write overlay, decisions need a maintainer or a global admin.
	// Review notifications reuse the approval webhook channel: one receiver gets
	// both approval.* and review.* events. The inbound external-result endpoint
	// is authenticated by AEGIS_REVIEW_WEBHOOK_SECRET instead (HTTP layer).
	reviewCenter := reviews.New(db, registry, reviews.Options{
		WebhookURL:     cfg.ApprovalWebhookURL,
		WebhookSecret:  cfg.ApprovalWebhookSecret,
		WebhookTimeout: cfg.ApprovalWebhookTimeout,
	})
	// Scan results and defense events are external evidence: the services only
	// store and serve rows, while the HTTP layer authenticates the signed ingest
	// webhook with AEGIS_INGEST_WEBHOOK_SECRET. Neither module runs a scanner or
	// a detection engine.
	scanRegistry := scans.New(db)
	defenseEvents := securityevents.New(db)
	// Knowledge documents are plain text/Markdown stored verbatim; the shared
	// AEGIS_SECRET_KEY seals the independent write-only secret store, which has
	// no read path for values (create/rotate answer 503 without the key).
	knowledgeBase := knowledge.New(db)
	secretStore := secrets.New(db, secrets.Options{SecretKey: cfg.SecretKey})
	pipeline := builds.New(db, registry).WithSandbox(builds.Sandbox{
		Mode:   builds.SandboxMode(cfg.BuildSandbox),
		Image:  cfg.BuildImage,
		CPU:    cfg.BuildCPU,
		Memory: cfg.BuildMemory,
		Pids:   cfg.BuildPids,
		User:   cfg.BuildUser,
	}).WithSource(cfg.BuildClone, cfg.BuildAllowLocalRepo).WithDiskLimit(cfg.BuildDisk).WithTimeout(cfg.BuildTimeout)
	// OIDC single sign-on is opt-in and fail-closed: when AEGIS_OIDC_ISSUER is
	// configured, discovery runs before the listener opens, so a misconfigured
	// or unreachable provider refuses startup instead of failing every login.
	// Without the issuer the service stays nil and the feature has no effect.
	var oidcService *oidc.Service
	if cfg.OIDC.Enabled() {
		oidcService, err = oidc.New(context.Background(), db, service, oidc.Options{
			Issuer:           cfg.OIDC.Issuer,
			ClientID:         cfg.OIDC.ClientID,
			ClientSecret:     cfg.OIDC.ClientSecret,
			RedirectURL:      cfg.OIDC.RedirectURL,
			Scopes:           cfg.OIDC.Scopes,
			AutoProvision:    cfg.OIDC.AutoProvision,
			Label:            cfg.OIDC.Label,
			DiscoveryTimeout: cfg.OIDC.DiscoveryTimeout,
			Logger:           logger,
		})
		if err != nil {
			return fmt.Errorf("OIDC single sign-on: %w", err)
		}
		logger.Info("OIDC single sign-on enabled", "issuer", cfg.OIDC.Issuer, "autoProvision", cfg.OIDC.AutoProvision)
	}
	// Deployment-target secrets are sealed with AEGIS_SECRET_KEY; when it is
	// unset the registry still serves targets without credentials. The same
	// service owns the deploy-task store, the approval queue and the worker.
	deploymentsService := deployments.New(db, deployments.Options{
		SecretKey:              cfg.SecretKey,
		PreviousSecretKey:      cfg.PreviousSecretKey,
		Projects:               registry,
		CheckTimeout:           cfg.DeployCheckTimeout,
		DeployTimeout:          cfg.DeployTimeout,
		HealthCheckAttempts:    cfg.DeployHealthAttempts,
		HealthCheckInterval:    cfg.DeployHealthInterval,
		ApprovalEnvironments:   cfg.ApprovalEnvironments,
		ApprovalTTL:            cfg.ApprovalTTL,
		ApprovalWebhookURL:     cfg.ApprovalWebhookURL,
		ApprovalWebhookSecret:  cfg.ApprovalWebhookSecret,
		ApprovalWebhookTimeout: cfg.ApprovalWebhookTimeout,
		// The review gate is opt-in: an empty slice keeps deployment creation
		// unchanged, and its list is independent of the approval policy.
		RequireReviewEnvironments: cfg.RequireReviewEnvironments,
		// The per-project automation policy composes on top of the environment
		// gate; projects without a stored policy stay exactly as before.
		PolicyGate: policyStore,
	})
	// Metrics are opt-in and live on their own listener: the public API origin
	// must not expose deployment-wide counters, and scraping needs no session.
	var monitoring *metrics.Registry
	if cfg.MetricsAddr != "" {
		monitoring = metrics.New()
	}
	// Register the termination signal before building the handler: streaming
	// handlers end their connections on this context, while server.Shutdown
	// alone would wait out its 10s drain whenever an SSE client is connected.
	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	handler := httpapi.New(service, logger, ready, cfg.AllowedOrigins, httpapi.Options{CookieSecure: cfg.CookieSecure, Limiter: ratelimit.NewWithLimit(cache, cfg.RedisNamespace, cfg.LoginRateLimit), TrustedProxies: cfg.TrustedProxies, Projects: registry, Builds: pipeline, Deployments: deploymentsService, Reviews: reviewCenter, Scans: scanRegistry, SecurityEvents: defenseEvents, Knowledge: knowledgeBase, Secrets: secretStore, Policies: policyStore, OIDC: oidcService, ReviewWebhookSecret: cfg.ReviewWebhookSecret, IngestWebhookSecret: cfg.IngestWebhookSecret, DefaultTenantID: defaultTenant.ID, Metrics: monitoring, ShutdownContext: signalCtx})
	server := &http.Server{Addr: cfg.Address, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	if monitoring != nil {
		mux := http.NewServeMux()
		mux.Handle("/metrics", monitoring.Handler(func(ctx context.Context) []metrics.Gauge {
			counts, err := pipeline.StatusCounts(ctx)
			if err != nil {
				logger.Error("metrics build gauges unavailable", "error", err)
				return nil
			}
			gauges := make([]metrics.Gauge, 0, len(counts))
			for status, count := range counts {
				gauges = append(gauges, metrics.Gauge{Name: "aegisflow_builds", Help: "Builds by status.", Labels: map[string]string{"status": status}, Value: float64(count)})
			}
			return gauges
		}))
		metricsServer := &http.Server{Addr: cfg.MetricsAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second}
		metricsListener, listenErr := net.Listen("tcp", cfg.MetricsAddr)
		if listenErr != nil {
			return listenErr
		}
		logger.Info("metrics listening", "address", metricsListener.Addr().String())
		go func() {
			if serveErr := metricsServer.Serve(metricsListener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
				logger.Error("metrics server stopped", "error", serveErr)
			}
		}()
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = metricsServer.Shutdown(shutdownCtx)
		}()
	}
	// The worker finalizes its in-flight build as interrupted when this
	// context is cancelled by the shutdown path.
	workerCtx, stopWorker := context.WithCancel(signalCtx)
	defer stopWorker()
	// Only the replica holding the PostgreSQL worker lease recovers interrupted
	// builds and executes queued ones; the others stand by and take over when
	// the holder stops (see builds.RunWorker). The deployment worker uses a
	// separate lease key so both pipelines run on every replica independently.
	go pipeline.RunWorker(workerCtx, logger)
	go deploymentsService.RunWorker(workerCtx, logger)
	// The notifier is independent of the deployment lease: every replica may
	// claim distinct outbox rows, and a slow webhook must not delay executions.
	go deploymentsService.RunNotifier(workerCtx, logger)
	// Review events share the same approval webhook receiver, so the review
	// notifier is another independent outbox consumer on the same signal ctx.
	go reviewCenter.RunNotifier(workerCtx, logger)
	// Expired sessions and their refresh history are pruned hourly; replay
	// detection never needs history beyond the session deadline.
	cleanupCtx, stopCleanup := context.WithCancel(signalCtx)
	defer stopCleanup()
	go func() {
		run := func() {
			removed, err := service.CleanupExpired(cleanupCtx)
			switch {
			case err != nil && cleanupCtx.Err() == nil:
				logger.Error("session cleanup failed", "error", err)
			case removed > 0:
				logger.Info("expired sessions cleaned", "count", removed)
			}
			// Audit retention is opt-in: AEGIS_AUDIT_RETENTION=0 keeps every row.
			if cfg.AuditRetention <= 0 || cleanupCtx.Err() != nil {
				return
			}
			purged, purgeErr := service.PurgeAudit(cleanupCtx, time.Now().Add(-cfg.AuditRetention).Unix())
			switch {
			case purgeErr != nil && cleanupCtx.Err() == nil:
				logger.Error("audit retention cleanup failed", "error", purgeErr)
			case purged > 0:
				logger.Info("expired audit rows purged", "count", purged, "retention", cfg.AuditRetention.String())
			}
		}
		run()
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-cleanupCtx.Done():
				return
			case <-ticker.C:
				run()
			}
		}
	}()
	result := make(chan error, 1)
	listener, err := net.Listen("tcp", cfg.Address)
	if err != nil {
		return err
	}
	// Log the bound address: with a :0 config port this is the only place the
	// real port is reported.
	go func() {
		logger.Info("server listening", "address", listener.Addr().String())
		result <- server.Serve(listener)
	}()
	select {
	case err = <-result:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-signalCtx.Done():
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		if err = server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			return err
		}
		return nil
	}
}
