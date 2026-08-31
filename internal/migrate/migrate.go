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

// a stuck migration should fail the boot rather than hang it forever.
const lockTimeout = "30s"

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

	if _, err := conn.Exec(ctx, "select pg_advisory_lock($1, $2)", lockKeyApp, lockKeyMigrations); err != nil {
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
