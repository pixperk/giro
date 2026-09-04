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
// one variable, DATABASE_URL, read the same way by every command.
//
// migrating and serving do want different roles: migrations need the one that
// owns the tables, and serving must hold one that cannot alter them. that
// separation lives in the environment rather than in the variable name. the
// migration step is its own job with its own environment, and giving it a
// second name would not stop a deployment putting the owner in both -- while
// costing every operator a variable to get right. giro serve warns at boot if
// the connection it was handed can disable its own guards, which is the check
// that actually catches the mistake.
func migrateURL() (string, error) {
	if url := os.Getenv("DATABASE_URL"); url != "" {
		return url, nil
	}
	return "", fmt.Errorf("DATABASE_URL is not set, " +
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
