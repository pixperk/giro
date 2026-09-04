package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pixperk/giro/fx"
	"github.com/pixperk/giro/recon"
	"github.com/pixperk/giro/storage"
)

const verifyUsage = `usage:
  giro verify [ledger...]      run every check, all ledgers by default

  --stale-prefix=PREFIX        accounts to check for money sitting still
  --stale-after=DURATION       how long is too long, for example 4h. 0 skips it
  --recon-after=DURATION       match staged statement lines, then report any
                               still unmatched after this. 0 skips it
  --boundary=PREFIX            which accounts face outward, default external:
  --record=false               do not write a verification_runs row
  --last                       report when each check last ran, and exit
  --max-age=DURATION           with --last, fail if any check has not run since
  --json                       machine readable output

exits 1 if any check reports a finding, so a scheduler notices.
reads DATABASE_URL from the environment.
`

func verifyCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, verifyUsage) }

	stalePrefix := fs.String("stale-prefix", "pending:", "")
	staleAfter := fs.Duration("stale-after", 0, "")
	reconAfter := fs.Duration("recon-after", 0, "")
	boundary := fs.String("boundary", "", "")
	record := fs.Bool("record", true, "")
	last := fs.Bool("last", false, "")
	maxAge := fs.Duration("max-age", 0, "")
	asJSON := fs.Bool("json", false, "")
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
		return reportLastRun(ctx, pool, names, *maxAge, *asJSON)
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
			Extra:       extraChecks(pool, name, *reconAfter, *boundary),
		})
		// print what did run before returning, so a failure to record does not
		// swallow the findings that were the point of running
		if *asJSON {
			findings += reportJSON(name, results)
		} else {
			findings += report(name, results)
		}
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
//
// With --max-age this is not a report but a check, and a monitor can treat it
// exactly like the findings one: run it, look at the exit code. That is the
// whole reason it takes a duration rather than leaving an operator to compare
// timestamps by eye, which is a thing nobody does at three in the morning.
func reportLastRun(ctx context.Context, pool *pgxpool.Pool, names []string, maxAge time.Duration, asJSON bool) error {
	type staleness struct {
		Ledger string    `json:"ledger"`
		Check  string    `json:"check"`
		LastAt time.Time `json:"lastAt"`
		AgeSec float64   `json:"ageSeconds"`
		Stale  bool      `json:"stale"`
	}

	var out []staleness
	var stale int

	for _, name := range names {
		seen, err := storage.New(pool, name).LastVerified(ctx)
		if err != nil {
			return err
		}
		if !asJSON {
			fmt.Printf("%s\n", name)
		}
		if len(seen) == 0 {
			if !asJSON {
				fmt.Println("  never run")
			}
			// never run is only a failure when somebody asked for a bound.
			// otherwise it is a fresh ledger, which is not a problem.
			if maxAge > 0 {
				stale++
				out = append(out, staleness{Ledger: name, Check: "*", Stale: true})
			}
			continue
		}
		for _, check := range sortedKeys(seen) {
			at := seen[check]
			age := time.Since(at)
			isStale := maxAge > 0 && age > maxAge
			if isStale {
				stale++
			}
			out = append(out, staleness{name, check, at, age.Seconds(), isStale})
			if !asJSON {
				mark := "  "
				if isStale {
					mark = "! "
				}
				fmt.Printf("%s%-20s %s  (%s ago)\n",
					mark, check, at.Format(time.RFC3339), age.Round(time.Second))
			}
		}
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			return err
		}
	}
	if stale > 0 {
		return fmt.Errorf("%d check(s) have not run in %s", stale, maxAge)
	}
	return nil
}

// results as json, for a monitor rather than a person.
func reportJSON(name string, results []storage.CheckResult) (findings int) {
	type line struct {
		Ledger  string `json:"ledger"`
		Check   string `json:"check"`
		OK      bool   `json:"ok"`
		Checked int    `json:"checked"`
		TookMs  int64  `json:"tookMs"`
		Detail  string `json:"detail,omitempty"`
	}
	out := make([]line, len(results))
	for i, r := range results {
		out[i] = line{name, r.Name, r.OK, r.Checked, r.Took.Milliseconds(), r.Detail}
		if !r.OK {
			findings++
		}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
	return findings
}

func sortedKeys(m map[string]time.Time) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// Checks contributed by layers above the engine.
//
// The engine has no idea two postings are a trade or that a statement line
// describes one of its movements, so neither check can live inside it without
// teaching it what those things are. Here they are just checks with names, and
// they are recorded alongside the rest because an operator wants one answer to
// "is the book sound" rather than one per package.
func extraChecks(pool *pgxpool.Pool, name string, reconAfter time.Duration, boundary string) []storage.NamedCheck {
	checks := []storage.NamedCheck{{
		Name: "conversions",
		Run: func(ctx context.Context) (int, error) {
			return fx.Verify(ctx, pool, name, fx.DefaultTolerance)
		},
	}}

	// off unless asked for. a ledger with no sources registered has nothing to
	// reconcile against, and a check that always reports nothing teaches an
	// operator to ignore it.
	if reconAfter > 0 {
		checks = append(checks, storage.NamedCheck{
			Name: "reconciliation",
			Run: func(ctx context.Context) (int, error) {
				return recon.Check(ctx, pool, name,
					recon.Config{BoundaryPrefix: boundary}, reconAfter)
			},
		})
	}
	return checks
}
