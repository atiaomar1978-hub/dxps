// Package svc holds process bootstrap shared by the DxPS service binaries.
package svc

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"dxps/internal/config"
	"dxps/internal/store"
)

// Boot loads configuration, installs a JSON logger and returns a context cancelled on SIGINT/SIGTERM.
func Boot(name string) (context.Context, context.CancelFunc, *config.Config, *slog.Logger) {
	var out io.Writer = os.Stdout
	if dir := os.Getenv("DXPS_LOG_DIR"); dir != "" {
		p := filepath.Join(dir, filepath.Base(name)+".log")
		if f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil { // #nosec G304 G703 -- operator env dir, compile-time service name
			out = f
		}
	}
	log := slog.New(slog.NewJSONHandler(out, &slog.HandlerOptions{Level: slog.LevelInfo})).With("svc", name)
	slog.SetDefault(log)
	cfg, err := config.Load()
	if err != nil {
		Fatal(log, "config", err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	return ctx, cancel, cfg, log
}

func Fatal(log *slog.Logger, what string, err error) {
	log.Error(what, "err", err)
	os.Exit(1)
}

// OpenStore opens the app (RLS) and ops pools, retrying while PostgreSQL starts.
func OpenStore(ctx context.Context, cfg *config.Config, log *slog.Logger) *store.Store {
	var last error
	for i := 0; i < 30 && ctx.Err() == nil; i++ {
		s, err := store.Open(ctx, cfg.PGApp, cfg.PGOps)
		if err == nil {
			return s
		}
		last = err
		time.Sleep(time.Second)
	}
	Fatal(log, "store", last)
	return nil
}

// Server returns an HTTP server with conservative timeouts.
func Server(addr string, h http.Handler) *http.Server {
	return &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 32 << 10}
}

// Shutdown gracefully stops srv when ctx is done.
func Shutdown(ctx context.Context, srv *http.Server) {
	<-ctx.Done()
	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(c)
}

func Ignore(err error) bool { return err == nil || errors.Is(err, http.ErrServerClosed) }
