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
func withConn(ctx context.Context, fn func(context.Context, *pgx.Conn, fs.FS) error) error {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return fmt.Errorf("DATABASE_URL is not set, for example postgres://user@localhost:5432/giro")
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
