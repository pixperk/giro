package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"

	"github.com/jackc/pgx/v5"

	"github.com/pixperk/giro"
)

// withConn opens a dedicated connection, never a pool.
//
// the migration advisory lock is session scoped, and releasing it on a
// different pooled connection silently returns false and does nothing, leaving
// the lock held until that connection happens to close.
// GIRO_MIGRATE_DATABASE_URL wins over DATABASE_URL, because migrating and
// serving want different roles. migrations need the one that owns the tables;
// serving should hold a role that cannot alter them. keeping the owner's
// credential out of the serving environment is the whole point of separating
// them, and it only works if the two are read from different places.
func migrateURL() (string, error) {
	if url := os.Getenv("GIRO_MIGRATE_DATABASE_URL"); url != "" {
		return url, nil
	}
	if url := os.Getenv("DATABASE_URL"); url != "" {
		return url, nil
	}
	return "", fmt.Errorf("neither GIRO_MIGRATE_DATABASE_URL nor DATABASE_URL is set, " +
		"for example postgres://user@localhost:5432/giro")
}

func withConn(ctx context.Context, fn func(context.Context, *pgx.Conn, fs.FS) error) error {
	url, err := migrateURL()
	if err != nil {
		return err
	}

	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()

	sub, err := fs.Sub(giro.MigrationsFS, giro.MigrationsDir)
	if err != nil {
		return err
	}
	return fn(ctx, conn, sub)
}
