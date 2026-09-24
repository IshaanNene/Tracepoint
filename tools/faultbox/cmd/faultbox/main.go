// Command faultbox runs the fault-injection demo application. See package faultbox.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/IshaanNene/Tracepoint/tools/faultbox"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "faultbox:", err)
		os.Exit(1)
	}
}

func run() error {
	listen := flag.String("listen", "127.0.0.1:8080", "application address")
	admin := flag.String("admin", "127.0.0.1:8081", "fault-injection address; keep it private")
	dsn := flag.String("dsn", os.Getenv("DATABASE_URL"), "Postgres connection string")
	redisAddr := flag.String("redis", envOr("REDIS_ADDR", "127.0.0.1:6379"), "Redis address")
	pool := flag.Int("pool", 100, "connection pool size for Postgres and Redis")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	app, err := faultbox.New(ctx, faultbox.Config{DSN: *dsn, RedisAddr: *redisAddr, PoolSize: *pool, Logger: log})
	if err != nil {
		return err
	}
	defer func() { _ = app.Close() }()
	if err := app.Setup(ctx); err != nil {
		return err
	}

	servers := []*http.Server{
		{Addr: *listen, Handler: app.Handler(), ReadHeaderTimeout: 5 * time.Second},
		{Addr: *admin, Handler: app.AdminHandler(), ReadHeaderTimeout: 5 * time.Second},
	}
	errCh := make(chan error, len(servers))
	for _, s := range servers {
		go func(s *http.Server) {
			log.Info("listening", "addr", s.Addr)
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}(s)
	}
	select {
	case <-ctx.Done():
	case err := <-errCh:
		return err
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var errs []error
	for _, s := range servers {
		if err := s.Shutdown(shutdown); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
