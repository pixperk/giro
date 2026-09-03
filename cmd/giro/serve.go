package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	giro "github.com/pixperk/giro"
	"github.com/pixperk/giro/internal/api"
	"github.com/pixperk/giro/migrate"
	"github.com/pixperk/giro/storage"
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

	if err := checkSchema(ctx, pool); err != nil {
		return err
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

// refuses to serve against a schema this binary does not match.
//
// the failure this prevents is a binary that boots cleanly, passes its health
// check, and then fails on the first commit with a raw SQL error, in a money
// path, in front of a caller. a deploy can act on a process that would not
// start. it cannot act on one that started and lies.
//
// a schema ahead of this binary is warned about rather than refused. the usual
// deploy order is migrate first, then roll the binaries, so between those steps
// every instance still on the old build is running against migrations it does
// not carry. refusing there would mean an old instance could not restart
// during a rollout, which turns a routine deploy into an outage.
func checkSchema(ctx context.Context, pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire: %w", err)
	}
	defer conn.Release()

	sub, err := fs.Sub(giro.MigrationsFS, giro.MigrationsDir)
	if err != nil {
		return err
	}

	switch err := migrate.RequireUpToDate(ctx, conn.Conn(), sub); {
	case errors.Is(err, migrate.ErrSchemaAhead):
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
		fmt.Fprintln(os.Stderr, "  expected mid-rollout. investigate if it persists after the deploy.")
		return nil
	case err != nil:
		return fmt.Errorf("schema check: %w", err)
	}
	return nil
}
