// Command gowait is a virtual waiting room reverse proxy: it admits up to N
// concurrent users to the backend and queues the rest on a self-updating
// waiting page.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/davidaparicio/gowait/internal/config"
	"github.com/davidaparicio/gowait/internal/queue"
	"github.com/davidaparicio/gowait/internal/server"
	"github.com/davidaparicio/gowait/internal/store"
	"github.com/davidaparicio/gowait/internal/store/memory"
	"github.com/davidaparicio/gowait/internal/store/valkeystore"
	"github.com/davidaparicio/gowait/internal/ticket"
)

// version is set at release time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "-version" {
		fmt.Println("gowait", version)
		return
	}
	if err := run(); err != nil {
		slog.Error("gowait exited", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Args[1:])
	if err != nil {
		return err
	}

	secret := cfg.CookieSecret
	if secret == "" {
		secret, err = ticket.NewSecret()
		if err != nil {
			return err
		}
		slog.Warn("GOWAIT_COOKIE_SECRET not set; generated a random one — cookies will not survive a restart")
		if cfg.Store == "valkey" {
			slog.Warn("valkey store with a per-instance random cookie secret: other gowait replicas will not recognize this instance's tickets — set GOWAIT_COOKIE_SECRET on all replicas")
		}
	}

	var st store.Store
	switch cfg.Store {
	case "valkey":
		vs, err := valkeystore.New(cfg.ValkeyURL, cfg.ValkeyPrefix)
		if err != nil {
			return err
		}
		defer vs.Close()
		slog.Info("using valkey store", "url", cfg.ValkeyURL, "prefix", cfg.ValkeyPrefix)
		st = vs
	default:
		st = memory.New()
	}

	ctrl := queue.New(st, queue.Config{
		Capacity:  cfg.Capacity,
		ActiveTTL: cfg.InactivityTTL,
		QueueTTL:  cfg.QueueTTL,
	}, nil)

	srv, err := server.New(cfg, ctrl, ticket.NewSigner(secret), nil)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go ctrl.Run(ctx) // janitor: drains the queue even with zero traffic

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("gowait listening",
			"version", version,
			"addr", cfg.Listen,
			"backend", cfg.BackendURL.String(),
			"capacity", cfg.Capacity,
			"inactivity_ttl", cfg.InactivityTTL,
			"admin_bypass", cfg.AdminKey != "")
		errCh <- httpSrv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		slog.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return nil
	}
}
