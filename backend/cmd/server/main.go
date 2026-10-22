package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
	"github.com/yichenfchai/AegisFlow/backend/internal/config"
	"github.com/yichenfchai/AegisFlow/backend/internal/httpapi"
	_ "modernc.org/sqlite"
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
	if err = os.MkdirAll(filepath.Dir(cfg.DatabasePath), 0700); err != nil {
		return err
	}
	db, err := sql.Open("sqlite", cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer db.Close()
	// One connection serializes SQLite writers and keeps PRAGMAs applied consistently.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err = db.PingContext(ctx); err != nil {
		return err
	}
	for _, statement := range []string{"PRAGMA foreign_keys = ON", "PRAGMA journal_mode = WAL", "PRAGMA busy_timeout = 5000"} {
		if _, err = db.ExecContext(ctx, statement); err != nil {
			return err
		}
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
	server := &http.Server{Addr: cfg.Address, Handler: httpapi.New(service, logger, db.PingContext, cfg.AllowedOrigins), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
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
