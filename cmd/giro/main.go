package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

const usage = `giro - a double entry ledger

usage:
  giro <command> [args]

commands:
  serve      run the http api
  migrate    apply and inspect database migrations
  verify     run the invariant checks, and record that they ran

run a command with no arguments to see its own usage.
`

// usageErr means the command was invoked wrongly rather than failing at its
// job. printed without an "error:" prefix and exits 2, the conventional code.
type usageErr struct{ text string }

func (e usageErr) Error() string { return e.text }

func main() {
	// cancelled on ctrl-c, so a run blocked on the migration advisory lock
	// stops when asked instead of sitting there until lock_timeout.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := dispatch(ctx, os.Args[1:]); err != nil {
		var u usageErr
		if errors.As(err, &u) {
			fmt.Fprint(os.Stderr, u.text)
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func dispatch(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageErr{usage}
	}

	switch args[0] {
	case "migrate":
		return migrateCommand(ctx, args[1:])
	case "serve":
		return serveCommand(ctx, args[1:])
	case "verify":
		return verifyCommand(ctx, args[1:])
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	default:
		return usageErr{fmt.Sprintf("unknown command %q\n\n%s", args[0], usage)}
	}
}
