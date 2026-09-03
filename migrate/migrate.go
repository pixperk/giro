// Package migrate applies ordered sql migrations to postgres.
//
// forward only by design. a down migration cannot un-drop a column that had
// data, so every real rollback is a new migration anyway, and keeping a down
// half means shipping a file nobody tests that lies under pressure.
package migrate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// "giro" in ascii, so the lock is self identifying in pg_locks. the second key
// namespaces this lock to migrations, leaving room for others later.
const (
	lockKeyApp        int32 = 0x6769726F
	lockKeyMigrations int32 = 1
)

// a stuck migration should fail the boot rather than hang it forever. the
// statement timeout bounds waits taken by the migrations themselves, where
// ddl queues behind a long running query; lockWait bounds how long this
// process will wait for another one to finish migrating.
const (
	lockTimeout = "30s"
	lockPoll    = 100 * time.Millisecond
)

// a var so the timeout path can be tested in milliseconds rather than in half
// a minute.
var lockWait = 30 * time.Second

const createTable = `
create table if not exists schema_migrations (
	version     bigint primary key,
	name        text        not null,
	checksum    text        not null,
	applied_at  timestamptz not null default now(),
	duration_ms bigint      not null
)`

var (
	// an already applied migration no longer matches what is on disk. prod and
	// dev have silently diverged and only one of them is right.
	ErrChecksumDrift = errors.New("migration checksum drift")

	// a pending migration sorts before one that already ran, usually a branch
	// merged late. applying it would make "what ran, in what order" unanswerable.
	ErrOutOfOrder = errors.New("migration out of order")

	// a migration that ran is no longer on disk.
	ErrMissing = errors.New("applied migration missing from disk")
)

type applied struct {
	Name      string
	Checksum  string
	AppliedAt time.Time
}

// Run applies every pending migration in version order and reports how many
// it applied. always call it, even when Status says nothing is pending: the
// consistency checks between disk and history live here, so skipping Run
// skips them.
//
// takes a *pgx.Conn rather than a pool on purpose. the advisory lock below is
// session scoped, and releasing it on a different pooled connection silently
// does nothing, leaving the lock held until that connection happens to close.
func Run(ctx context.Context, conn *pgx.Conn, fsys fs.FS) (n int, err error) {
	migrations, err := Load(fsys)
	if err != nil {
		return 0, err
	}

	if _, err := conn.Exec(ctx, "set lock_timeout = "+quote(lockTimeout)); err != nil {
		return 0, fmt.Errorf("set lock_timeout: %w", err)
	}

	if err := acquireAdvisoryLock(ctx, conn); err != nil {
		return 0, fmt.Errorf("acquire advisory lock: %w", err)
	}
	defer func() {
		err = errors.Join(err, releaseAdvisoryLock(ctx, conn))
	}()

	if _, err := conn.Exec(ctx, createTable); err != nil {
		return 0, fmt.Errorf("create schema_migrations: %w", err)
	}

	done, err := loadApplied(ctx, conn)
	if err != nil {
		return 0, err
	}

	if err := verify(migrations, done); err != nil {
		return 0, err
	}

	for _, m := range migrations {
		if _, ok := done[m.Version]; ok {
			continue
		}
		if err := apply(ctx, conn, m); err != nil {
			return n, fmt.Errorf("apply %s: %w", m.Filename, err)
		}
		n++
	}

	return n, nil
}

// polled rather than blocked on, which is not the obvious way to wait.
//
// pg_advisory_lock would park this session in a lock wait, and a session in a
// lock wait still holds a virtual transaction id. create index concurrently,
// which the no-transaction directive exists to allow, waits for every virtual
// transaction that was live when it started. so a runner holding the lock and
// building an index waits for a runner waiting for the lock, and postgres
// breaks the cycle by killing the waiter. two instances booting at once is
// exactly the case the lock is here for, and it would fail there.
//
// polling has no such edge. between attempts this session holds nothing, so
// there is no cycle to detect and the index build sees no transaction to wait
// on. the cost is up to one poll interval of latency after the lock frees.
func acquireAdvisoryLock(ctx context.Context, conn *pgx.Conn) error {
	ctx, cancel := context.WithTimeout(ctx, lockWait)
	defer cancel()

	for {
		var got bool
		err := conn.QueryRow(ctx,
			"select pg_try_advisory_lock($1, $2)", lockKeyApp, lockKeyMigrations).Scan(&got)
		switch {
		case got:
			return nil
		case ctx.Err() != nil:
			return fmt.Errorf("waited %s for another migration to finish", lockWait)
		case err != nil:
			return err
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("waited %s for another migration to finish", lockWait)
		case <-time.After(lockPoll):
		}
	}
}

// releasing is checked rather than fired and forgotten.
//
// pg_advisory_unlock returns false when this session does not hold the lock,
// and raises no error. ignoring that is how a lock ends up held until the
// connection happens to close, with the next deploy blocking on something
// nobody knows about.
func releaseAdvisoryLock(ctx context.Context, conn *pgx.Conn) error {
	var released bool
	if err := conn.QueryRow(ctx,
		"select pg_advisory_unlock($1, $2)", lockKeyApp, lockKeyMigrations).Scan(&released); err != nil {
		return fmt.Errorf("release advisory lock: %w", err)
	}
	if !released {
		return errors.New("advisory lock was not held at release")
	}
	return nil
}

func apply(ctx context.Context, conn *pgx.Conn, m Migration) error {
	start := time.Now()

	// the whole file goes to Exec in one call. splitting on ';' would break
	// dollar quoted function bodies, which the effective volume triggers need.
	if m.NoTx {
		if _, err := conn.Exec(ctx, m.SQL); err != nil {
			return err
		}
		return record(ctx, conn, m, time.Since(start))
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, m.SQL); err != nil {
		return err
	}
	if err := record(ctx, tx, m, time.Since(start)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// satisfied by both *pgx.Conn and pgx.Tx, so a no-transaction migration
// records itself the same way as any other.
type queryer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func record(ctx context.Context, q queryer, m Migration, took time.Duration) error {
	_, err := q.Exec(ctx,
		`insert into schema_migrations (version, name, checksum, duration_ms)
		 values ($1, $2, $3, $4)`,
		m.Version, m.Name, m.Checksum, took.Milliseconds())
	return err
}

func loadApplied(ctx context.Context, conn *pgx.Conn) (map[int64]applied, error) {
	rows, err := conn.Query(ctx, "select version, name, checksum, applied_at from schema_migrations")
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()

	done := map[int64]applied{}
	for rows.Next() {
		var v int64
		var a applied
		if err := rows.Scan(&v, &a.Name, &a.Checksum, &a.AppliedAt); err != nil {
			return nil, err
		}
		done[v] = a
	}
	return done, rows.Err()
}

// three ways history and disk can disagree, all of them fatal.
func verify(migrations []Migration, done map[int64]applied) error {
	onDisk := make(map[int64]Migration, len(migrations))
	var highestApplied int64

	for _, m := range migrations {
		onDisk[m.Version] = m
		if a, ok := done[m.Version]; ok {
			if a.Checksum != m.Checksum {
				return fmt.Errorf("%w: %s was applied at %s with a different body, edit history is not allowed, write a new migration instead",
					ErrChecksumDrift, m.Filename, a.AppliedAt.Format(time.RFC3339))
			}
			highestApplied = max(highestApplied, m.Version)
		}
	}

	for version, a := range done {
		if _, ok := onDisk[version]; !ok {
			return fmt.Errorf("%w: version %d (%s) ran at %s",
				ErrMissing, version, a.Name, a.AppliedAt.Format(time.RFC3339))
		}
	}

	for _, m := range migrations {
		if _, ok := done[m.Version]; ok {
			continue
		}
		if m.Version < highestApplied {
			return fmt.Errorf("%w: %s sorts before applied version %d, rename it to a current timestamp",
				ErrOutOfOrder, m.Filename, highestApplied)
		}
	}

	return nil
}

type MigrationStatus struct {
	Migration
	Applied   bool
	AppliedAt time.Time
}

// Status reports what has run and what has not. takes no lock, so it is safe
// to call while another process is migrating.
func Status(ctx context.Context, conn *pgx.Conn, fsys fs.FS) ([]MigrationStatus, error) {
	migrations, err := Load(fsys)
	if err != nil {
		return nil, err
	}

	var exists bool
	if err := conn.QueryRow(ctx, "select to_regclass('schema_migrations') is not null").Scan(&exists); err != nil {
		return nil, err
	}

	done := map[int64]applied{}
	if exists {
		if done, err = loadApplied(ctx, conn); err != nil {
			return nil, err
		}
	}

	out := make([]MigrationStatus, 0, len(migrations))
	for _, m := range migrations {
		a, ok := done[m.Version]
		out = append(out, MigrationStatus{Migration: m, Applied: ok, AppliedAt: a.AppliedAt})
	}
	return out, nil
}

func quote(s string) string { return "'" + s + "'" }

// a migration this binary carries has not been applied. the schema is behind
// the code, so something the binary expects to exist does not.
var ErrPending = errors.New("migrations pending")

// the database has a migration this binary does not carry. the schema is ahead
// of the code.
//
// separate from ErrMissing because it is not always a fault. the usual deploy
// order is migrate first, then roll the binaries, so between those two steps
// every old instance is running against a schema it has never heard of. that
// is the deploy working, and refusing to start there would mean an old
// instance could not be restarted during a rollout.
//
// it is a fault when it is permanent, which is why it is reported rather than
// ignored, and left to the caller to decide.
var ErrSchemaAhead = errors.New("schema is ahead of this binary")

// RequireUpToDate reports whether the database schema matches what this binary
// expects, and is meant to be called at boot before serving anything.
//
// Without it a binary needing a migration nobody ran starts cleanly, answers
// its health check, and then fails on the first write with a raw SQL error
// from inside a money path. The failure belongs at startup, where a deploy can
// see it, rather than at the first transaction.
//
// Takes no lock, so it is safe to call while another process is migrating,
// and it applies nothing: a process that serves traffic should not also be
// able to change the schema.
//
// Returns ErrPending when the schema is behind, ErrChecksumDrift when a
// migration was applied with a different body, and ErrSchemaAhead when the
// database carries migrations this binary does not. Only the first two are
// unambiguously fatal; see ErrSchemaAhead for why it is handed back rather
// than decided here.
func RequireUpToDate(ctx context.Context, conn *pgx.Conn, fsys fs.FS) error {
	migrations, err := Load(fsys)
	if err != nil {
		return err
	}

	var exists bool
	if err := conn.QueryRow(ctx,
		"select to_regclass('schema_migrations') is not null").Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("%w: nothing has ever been applied, run: giro migrate up", ErrPending)
	}

	done, err := loadApplied(ctx, conn)
	if err != nil {
		return err
	}

	// drift first: a migration applied with a different body means the schema
	// and the code disagree about what ran, and no count of pending versions
	// tells you anything useful after that.
	onDisk := make(map[int64]Migration, len(migrations))
	for _, m := range migrations {
		onDisk[m.Version] = m
		if a, ok := done[m.Version]; ok && a.Checksum != m.Checksum {
			return fmt.Errorf("%w: %s was applied at %s with a different body",
				ErrChecksumDrift, m.Filename, a.AppliedAt.Format(time.RFC3339))
		}
	}

	var pending []string
	for _, m := range migrations {
		if _, ok := done[m.Version]; !ok {
			pending = append(pending, m.Filename)
		}
	}
	if len(pending) > 0 {
		return fmt.Errorf("%w: %d not applied (%s), run: giro migrate up",
			ErrPending, len(pending), strings.Join(pending, ", "))
	}

	var ahead []string
	for version, a := range done {
		if _, ok := onDisk[version]; !ok {
			ahead = append(ahead, fmt.Sprintf("%d_%s", version, a.Name))
		}
	}
	if len(ahead) > 0 {
		slices.Sort(ahead)
		return fmt.Errorf("%w: %d applied and not carried here (%s)",
			ErrSchemaAhead, len(ahead), strings.Join(ahead, ", "))
	}

	return nil
}
