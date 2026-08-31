package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pixperk/giro/internal/api"
	"github.com/pixperk/giro/internal/storage"
)

const serveUsage = `usage:
  giro serve [addr]     listen address, default :8080

reads DATABASE_URL from the environment.
`

func serveCommand(ctx context.Context, args []string) error {
	addr := ":8080"
	switch {
	case len(args) > 1:
		return usageErr{serveUsage}
	case len(args) == 1:
		if args[0] == "-h" || args[0] == "--help" {
			return usageErr{serveUsage}
		}
		addr = args[0]
	}

	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return fmt.Errorf("DATABASE_URL is not set")
	}

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return err
	}
	// small beats large against postgres: past a point more connections only
	// add context switches to a server with fixed parallelism.
	cfg.MaxConns = 20

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping: %w", err)
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           api.NewServer(func(name string) *storage.Store { return storage.New(pool, name) }),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		// the parent context is already cancelled, so give shutdown its own
		// deadline rather than one that has already expired.
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	fmt.Printf("giro listening on %s\n  docs  http://localhost%s/docs\n  spec  http://localhost%s/openapi.yaml\n", addr, addr, addr)

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	fmt.Println("stopped")
	return nil
}
