package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pixperk/giro/fx"
	"github.com/pixperk/giro/storage"
)

const verifyUsage = `usage:
  giro verify [ledger...]      run every check, all ledgers by default

  --stale-prefix=PREFIX        accounts to check for money sitting still
  --stale-after=DURATION       how long is too long, for example 4h. 0 skips it
  --record=false               do not write a verification_runs row
  --last                       report when each check last ran, and exit

exits 1 if any check reports a finding, so a scheduler notices.
reads DATABASE_URL from the environment.
`

func verifyCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, verifyUsage) }

	stalePrefix := fs.String("stale-prefix", "pending:", "")
	staleAfter := fs.Duration("stale-after", 0, "")
	record := fs.Bool("record", true, "")
	last := fs.Bool("last", false, "")
	if err := fs.Parse(args); err != nil {
		return usageErr{verifyUsage}
	}

	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return fmt.Errorf("DATABASE_URL is not set")
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	names := fs.Args()
	if len(names) == 0 {
		if names, err = storage.Ledgers(ctx, pool); err != nil {
			return err
		}
	}
	if len(names) == 0 {
		fmt.Println("no ledgers")
		return nil
	}

	if *last {
		return reportLastRun(ctx, pool, names)
	}

	var findings int
	for _, name := range names {
		// the composition root, and the only place the layers meet. the engine
		// has no idea two postings are a trade, so a check that they match a
		// stated rate cannot live inside it without teaching it. here it is
		// just another check with a name.
		results, err := storage.New(pool, name).Verify(ctx, storage.VerifyOptions{
			StalePrefix: *stalePrefix,
			StaleAfter:  *staleAfter,
			Record:      *record,
			Extra: []storage.NamedCheck{{
				Name: "conversions",
				Run: func(ctx context.Context) (int, error) {
					return fx.Verify(ctx, pool, name, fx.DefaultTolerance)
				},
			}},
		})
		// print what did run before returning, so a failure to record does not
		// swallow the findings that were the point of running
		findings += report(name, results)
		if err != nil {
			return err
		}
	}

	if findings > 0 {
		return fmt.Errorf("%d check(s) reported findings", findings)
	}
	return nil
}

func report(name string, results []storage.CheckResult) (findings int) {
	fmt.Printf("%s\n", name)
	for _, r := range results {
		mark := "ok  "
		if !r.OK {
			mark, findings = "FAIL", findings+1
		}
		fmt.Printf("  %s %-20s %7d checked  %6s\n",
			mark, r.Name, r.Checked, r.Took.Round(time.Millisecond))
		if r.Detail != "" {
			for line := range strings.SplitSeq(strings.TrimRight(r.Detail, "\n"), "\n") {
				fmt.Printf("       %s\n", line)
			}
		}
	}
	return findings
}

// the other half of alerting. findings above zero is the condition everyone
// writes; the absence of a run is the one that hides a real problem for as
// long as nobody notices the scheduler died.
func reportLastRun(ctx context.Context, pool *pgxpool.Pool, names []string) error {
	for _, name := range names {
		seen, err := storage.New(pool, name).LastVerified(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("%s\n", name)
		if len(seen) == 0 {
			fmt.Println("  never run")
			continue
		}
		for check, at := range seen {
			fmt.Printf("  %-20s %s  (%s ago)\n",
				check, at.Format(time.RFC3339), time.Since(at).Round(time.Second))
		}
	}
	return nil
}
