package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"
	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
	"github.com/yichenfchai/AegisFlow/backend/internal/config"
	"github.com/yichenfchai/AegisFlow/backend/internal/httpapi"
	"github.com/yichenfchai/AegisFlow/backend/internal/ratelimit"
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
	if cfg.BootstrapUsername != "" {
		if err = service.Bootstrap(ctx, cfg.BootstrapUsername, cfg.BootstrapPassword); err != nil {
			return err
		}
	}
	handler := httpapi.New(service, logger, ready, cfg.AllowedOrigins, httpapi.Options{CookieSecure: cfg.CookieSecure, Limiter: ratelimit.New(cache, cfg.RedisNamespace), TrustedProxies: cfg.TrustedProxies})
	server := &http.Server{Addr: cfg.Address, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	result := make(chan error, 1)
	go func() { logger.Info("server listening", "address", cfg.Address); result <- server.ListenAndServe() }()
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
