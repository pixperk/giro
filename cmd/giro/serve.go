package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	giro "github.com/pixperk/giro"
	"github.com/pixperk/giro/internal/api"
	"github.com/pixperk/giro/migrate"
	"github.com/pixperk/giro/storage"
)

const serveUsage = `usage:
  giro serve [addr]     listen address, default :8080

  --allow-unkeyed-writes
        accept a transaction with no Idempotency-Key. off by default: a
        connection lost after the commit but before the response leaves the
        caller unable to tell whether it landed, and retrying without a key
        pays twice. only set this if you deduplicate upstream.

reads DATABASE_URL from the environment.
`

func serveCommand(ctx context.Context, args []string) error {
	addr := ":8080"
	var positional []string
	for _, a := range args {
		switch {
		case a == "-h" || a == "--help":
			return usageErr{serveUsage}
		case a == "--allow-unkeyed-writes":
			// consumed by serverOptions
		case strings.HasPrefix(a, "-"):
			return usageErr{fmt.Sprintf("unknown flag %q\n\n%s", a, serveUsage)}
		default:
			positional = append(positional, a)
		}
	}
	switch len(positional) {
	case 0:
	case 1:
		addr = positional[0]
	default:
		return usageErr{serveUsage}
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
	warnIfPrivileged(ctx, pool)

	newStore := func(name string) *storage.Store { return storage.New(pool, name) }

	// These routes move money and authenticate nothing, so say so at boot
	// rather than in a document somebody may not have read. Reaching this port
	// is equivalent to holding a database credential.
	fmt.Println("  note  no authentication. keep this port on a private network,")
	fmt.Println("        behind whatever already terminates auth for your services.")

	srv := &http.Server{
		Addr:    addr,
		Handler: api.NewServer(newStore, serverOptions(args)...),

		// ReadHeaderTimeout alone leaves a connection able to dribble a body
		// forever; the others bound the rest of the exchange. Generous, because
		// a commit waits on row locks and a slow one is not an attack.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
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

// warns when the serving connection can switch off the guards protecting the
// tables it is about to write to.
//
// the database enforces the invariants with triggers, and a table's owner
// outranks its own triggers: "alter table logs disable trigger user" needs no
// privilege beyond ownership. a superuser outranks everything. so connecting as
// either means the guards are advisory, and the restricted role exists to stop
// that being true.
//
// a warning rather than a refusal. a single-machine deployment, a local
// database and a first run all legitimately connect as the owner, and refusing
// would make the safe configuration the hard one to reach. saying so at every
// boot is enough: it is in the logs, it is at the moment somebody is looking,
// and it names what to do.
func warnIfPrivileged(ctx context.Context, pool *pgxpool.Pool) {
	var privileged bool
	err := pool.QueryRow(ctx, `
		select current_setting('is_superuser')::bool
		    or pg_has_role(current_user, relowner, 'USAGE')
		  from pg_class where oid = to_regclass('logs')`).Scan(&privileged)
	if err != nil || !privileged {
		return
	}

	var user string
	_ = pool.QueryRow(ctx, "select current_user").Scan(&user)

	fmt.Fprintf(os.Stderr,
		"warning: connected as %q, which owns these tables or is a superuser.\n"+
			"  the database guards can be disabled by this connection, so they are\n"+
			"  advisory here. serve as a role with no privileges of its own that is a\n"+
			"  member of giro_app instead. see: just db-app-role\n", user)
}

// serverOptions turns the flags that change how the API behaves into options.
//
// --allow-unkeyed-writes is the only one, and it is off by default because a
// write with no Idempotency-Key cannot be safely retried: a connection lost
// after the commit but before the response leaves the caller unable to tell
// what happened, and retrying then pays twice. Turning it off is a decision
// somebody should make out loud.
func serverOptions(args []string) []func(*api.Server) {
	var opts []func(*api.Server)
	for _, a := range args {
		if a == "--allow-unkeyed-writes" {
			opts = append(opts, api.AllowUnkeyedWrites())
			fmt.Println("  note  --allow-unkeyed-writes: a write with no Idempotency-Key")
			fmt.Println("        cannot be retried safely. deduplicate upstream.")
		}
	}
	return opts
}
