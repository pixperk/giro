package main

import (
	"context"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pixperk/giro"
	"github.com/pixperk/giro/migrate"
)

const migrateUsage = `usage:
  giro migrate up              apply every pending migration
  giro migrate status          show what has run and what has not
  giro migrate new <name>      create an empty migration
`

func migrateCommand(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageErr{migrateUsage}
	}

	switch args[0] {
	case "up":
		return withConn(ctx, migrateUp)
	case "status":
		return withConn(ctx, migrateStatus)
	case "new":
		if len(args) < 2 {
			return usageErr{"usage: giro migrate new <name>\n"}
		}
		return migrateNew(strings.Join(args[1:], " "))
	default:
		return usageErr{fmt.Sprintf("unknown migrate command %q\n\n%s", args[0], migrateUsage)}
	}
}

func migrateUp(ctx context.Context, conn *pgx.Conn, fsys fs.FS) error {
	// always call Run, never short circuit on a pending count. the checks for
	// drift, missing files and out of order versions live inside it.
	start := time.Now()
	n, err := migrate.Run(ctx, conn, fsys)
	if err != nil {
		return err
	}
	if n == 0 {
		fmt.Println("nothing to do, schema is up to date")
		return nil
	}
	fmt.Printf("applied %d migration(s) in %s\n", n, time.Since(start).Round(time.Millisecond))
	return nil
}

func migrateStatus(ctx context.Context, conn *pgx.Conn, fsys fs.FS) error {
	list, err := migrate.Status(ctx, conn, fsys)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Println("no migrations found")
		return nil
	}

	for _, s := range list {
		state, when := "pending", ""
		if s.Applied {
			state, when = "applied", s.AppliedAt.Local().Format(time.RFC3339)
		}
		flag := ""
		if s.NoTx {
			flag = "  [no-transaction]"
		}
		fmt.Printf("  %-8s %-14d %-32s %s%s\n", state, s.Version, s.Name, when, flag)
	}
	return nil
}

// writes to the real directory, not the embedded copy, which is read only.
// the new file is only visible to migrate up after a rebuild.
func migrateNew(name string) error {
	path, err := migrate.Create(giro.MigrationsDir, name)
	if err != nil {
		return err
	}
	fmt.Println("created", path)
	return nil
}
