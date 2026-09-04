package main

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pixperk/giro/storage"
)

// The one failure the rest of the checks cannot see.
//
// Every check in giro proves the book is consistent with itself, and a
// restored database is consistent with itself: conservation holds, the chain
// verifies, the projection agrees. What it is not is consistent with the
// database it replaced. The counters went back, so the next commit reissues
// ids that already name other transactions, and every downstream system
// holding "giro transaction 4291" is now pointing somewhere else.
//
// Detecting that needs something the restore could not reach. `giro recover
// tip` prints a position small enough to paste into a deployment record;
// `giro recover check` compares the ledger against one afterwards.
const recoverUsage = `usage:
  giro recover tip [ledger...]
                          print each ledger's position: ledger:logID:hash.
                          record this after a good verify. it is what proves,
                          later, that a restore came back where you think

  giro recover check <tip> [tip...]
                          compare against positions recorded before a restore.
                          exits non-zero if a ledger is behind one or has
                          forked from it. run it after restoring and BEFORE
                          letting anything write

  giro recover resume <tip> [--note=TEXT]
                          resume above every id the ledger ever issued, and
                          append a RECOVERY entry declaring the gap. the
                          skipped ids are never reissued: they belonged to
                          transactions that really happened

reads DATABASE_URL from the environment.
see deploy/RECOVERY.md for the procedure this belongs to.
`

func recoverCommand(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageErr{recoverUsage}
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

	switch args[0] {
	case "tip":
		return recoverTip(ctx, pool, args[1:])
	case "check":
		return recoverCheck(ctx, pool, args[1:])
	case "resume":
		return recoverResume(ctx, pool, args[1:])
	default:
		return usageErr{fmt.Sprintf("unknown recover command %q\n\n%s", args[0], recoverUsage)}
	}
}

func recoverTip(ctx context.Context, pool *pgxpool.Pool, names []string) error {
	if len(names) == 0 {
		var err error
		if names, err = storage.Ledgers(ctx, pool); err != nil {
			return err
		}
	}
	if len(names) == 0 {
		fmt.Println("no ledgers")
		return nil
	}
	for _, name := range names {
		tip, err := storage.New(pool, name).ChainTip(ctx)
		if err != nil {
			return err
		}
		fmt.Println(tip)
	}
	return nil
}

func recoverCheck(ctx context.Context, pool *pgxpool.Pool, args []string) error {
	if len(args) == 0 {
		return usageErr{recoverUsage}
	}

	// every tip is checked before anything is reported, so an operator sees
	// the whole picture rather than fixing one ledger and discovering the next
	var failures []error
	for _, arg := range args {
		tip, err := storage.ParseTip(arg)
		if err != nil {
			return err
		}
		if err := storage.New(pool, tip.Ledger).CheckTip(ctx, tip); err != nil {
			fmt.Printf("  FAIL  %s\n        %v\n", tip.Ledger, err)
			failures = append(failures, err)
			continue
		}
		fmt.Printf("  ok    %s\n", tip.Ledger)
	}
	if len(failures) > 0 {
		return fmt.Errorf("%d of %d ledgers did not match: do not write to them until this is resolved",
			len(failures), len(args))
	}
	return nil
}

func recoverResume(ctx context.Context, pool *pgxpool.Pool, args []string) error {
	var note string
	var tips []string
	for _, a := range args {
		if after, ok := strings.CutPrefix(a, "--note="); ok {
			note = after
			continue
		}
		tips = append(tips, a)
	}
	if len(tips) != 1 {
		return usageErr{recoverUsage}
	}

	tip, err := storage.ParseTip(tips[0])
	if err != nil {
		return err
	}
	names, err := storage.Ledgers(ctx, pool)
	if err != nil {
		return err
	}
	if !slices.Contains(names, tip.Ledger) {
		return fmt.Errorf("no ledger %q. this database has: %v", tip.Ledger, names)
	}

	// the tx id is not in the text form, and resuming has to clear both
	// counters. the log id is the one an operator records, so the transaction
	// id is taken to be at least as high: it cannot exceed the log id, because
	// every transaction writes an entry.
	watermark := storage.Tip{Ledger: tip.Ledger, LogID: tip.LogID, TxID: tip.LogID}

	store := storage.New(pool, tip.Ledger)
	before, err := store.ChainTip(ctx)
	if err != nil {
		return err
	}
	if err := store.RecordRecovery(ctx, watermark, note); err != nil {
		return err
	}
	after, err := store.ChainTip(ctx)
	if err != nil {
		return err
	}

	if after.LogID == before.LogID {
		fmt.Printf("%s is already at log %d, past the recorded %d. nothing to do.\n",
			tip.Ledger, before.LogID, tip.LogID)
		return nil
	}
	fmt.Printf("%s resumed at log %d, declaring ids %d-%d as issued before the restore.\n",
		tip.Ledger, after.LogID, before.LogID+1, tip.LogID)
	fmt.Printf("those ids are never reissued. record the new tip:\n  %s\n", after)
	return nil
}
