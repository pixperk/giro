package storage

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// These tests simulate a restore rather than describing one. Winding the
// counters back and committing again is exactly what a point-in-time restore
// does to a ledger, and it is the only way to find out whether anything
// notices.

// rewind puts the ledger back where it was at an earlier moment, the way a
// restore would: the counters go back and the entries after that point are
// gone. It has to go around the guards, because going around them is precisely
// what a restore does -- it replaces the table rather than updating it, so no
// trigger ever sees the change.
func rewind(t *testing.T, ctx context.Context, pool *pgxpool.Pool, toLogID, toTxID int64) {
	t.Helper()
	for _, stmt := range []string{
		"alter table logs disable trigger user",
		"alter table transactions disable trigger user",
		"alter table ledgers disable trigger user",
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Skipf("cannot simulate a restore without the table owner: %v", err)
		}
	}
	t.Cleanup(func() {
		for _, stmt := range []string{
			"alter table logs enable trigger user",
			"alter table transactions enable trigger user",
			"alter table ledgers enable trigger user",
		} {
			_, _ = pool.Exec(context.WithoutCancel(ctx), stmt)
		}
	})

	for _, stmt := range []string{
		"alter table moves disable trigger user",
		"alter table accounts_volumes disable trigger user",
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Skipf("cannot simulate a restore without the table owner: %v", err)
		}
	}
	t.Cleanup(func() {
		for _, stmt := range []string{
			"alter table moves enable trigger user",
			"alter table accounts_volumes enable trigger user",
		} {
			_, _ = pool.Exec(context.WithoutCancel(ctx), stmt)
		}
	})

	// Order matters, and the volumes matter most. A restore takes every table
	// back together, so rewinding the log while leaving the balances where
	// they were would produce a state Postgres could never reach -- and the
	// test would then be proving something about a database that cannot exist.
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{"delete from moves where ledger='main' and tx_id > $1", []any{toTxID}},
		{"delete from logs where ledger='main' and id > $1", []any{toLogID}},
		{"delete from transactions where ledger='main' and id > $1", []any{toTxID}},

		// recompute the balances from what is left, which is what the restored
		// projection would have contained at that moment
		{`update accounts_volumes v
		     set input  = coalesce((select sum(amount) from moves m
		                             where m.ledger = v.ledger and m.address = v.address
		                               and m.asset = v.asset and not m.is_source), 0),
		         output = coalesce((select sum(amount) from moves m
		                             where m.ledger = v.ledger and m.address = v.address
		                               and m.asset = v.asset and m.is_source), 0)
		   where v.ledger = 'main'`, nil},

		{"update ledgers set last_log_id=$1, last_tx_id=$2, " +
			"last_log_hash=(select hash from logs where ledger='main' and id=$1) where name='main'",
			[]any{toLogID, toTxID}},
	} {
		if _, err := pool.Exec(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatalf("%s: %v", stmt.sql, err)
		}
	}
}

func TestATipRoundTripsThroughItsTextForm(t *testing.T) {
	ctx, s, _ := testStore(t)
	fund(t, ctx, s, "users:alice", 10000)

	tip, err := s.ChainTip(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if tip.LogID == 0 || len(tip.Hash) == 0 {
		t.Fatalf("tip = %+v, want a real position", tip)
	}

	// it has to survive being pasted into a deployment record
	parsed, err := ParseTip(tip.String())
	if err != nil {
		t.Fatalf("%q: %v", tip.String(), err)
	}
	if parsed.Ledger != tip.Ledger || parsed.LogID != tip.LogID {
		t.Errorf("parsed = %+v, want %+v", parsed, tip)
	}
	if err := s.CheckTip(ctx, parsed); err != nil {
		t.Errorf("a tip taken from this ledger does not match it: %v", err)
	}
}

// The happy path. A ledger that has moved forward since the tip was recorded
// still contains that entry, unchanged, so the check passes.
func TestMovingForwardDoesNotLookLikeARestore(t *testing.T) {
	ctx, s, _ := testStore(t)
	fund(t, ctx, s, "users:alice", 10000)

	recorded, err := s.ChainTip(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		fund(t, ctx, s, "users:bob", 100)
	}

	if err := s.CheckTip(ctx, recorded); err != nil {
		t.Errorf("ordinary progress reported as a restore: %v", err)
	}
}

// The failure this whole file exists for. Everything else in the package still
// passes after a restore -- that is the point -- so this has to be the thing
// that catches it.
func TestARestoreThatLostEntriesIsCaught(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "users:alice", 10000)

	recorded, err := s.ChainTip(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		fund(t, ctx, s, "users:bob", 100)
	}
	after, err := s.ChainTip(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// somebody records the tip, then a restore takes us back before it
	rewind(t, ctx, pool, recorded.LogID, recorded.TxID)

	// the rest of the suite is happy, which is exactly the danger
	if _, err := s.VerifyConservation(ctx); err != nil {
		t.Errorf("conservation should still hold after a clean restore: %v", err)
	}
	if _, err := s.VerifyLog(ctx); err != nil {
		t.Errorf("the chain should still verify after a clean restore: %v", err)
	}

	// and this is the one that notices
	err = s.CheckTip(ctx, after)
	if !errors.Is(err, ErrChainBehind) {
		t.Fatalf("err = %v, want ErrChainBehind", err)
	}
	var mismatch *TipMismatch
	if !errors.As(err, &mismatch) {
		t.Fatalf("err = %v, want a TipMismatch carrying both positions", err)
	}
	if !strings.Contains(err.Error(), "reuses their ids") {
		t.Errorf("the message does not say what the consequence is: %v", err)
	}
}

// Worse than being behind: having already written over the reused ids. The
// entry exists, it is not the entry it was, and the id now names something
// else.
func TestARestoreThatReusedIdsIsCaught(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "users:alice", 10000)

	recorded, err := s.ChainTip(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fund(t, ctx, s, "users:bob", 100)
	forked, err := s.ChainTip(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// restore back, then commit something different into the same id
	rewind(t, ctx, pool, recorded.LogID, recorded.TxID)
	fund(t, ctx, s, "users:carol", 777)

	now, err := s.ChainTip(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if now.LogID != forked.LogID {
		t.Fatalf("log id = %d, want %d reused: the simulation is wrong", now.LogID, forked.LogID)
	}

	err = s.CheckTip(ctx, forked)
	if !errors.Is(err, ErrChainForked) {
		t.Fatalf("err = %v, want ErrChainForked", err)
	}
	if !strings.Contains(err.Error(), "no longer the entry it was") {
		t.Errorf("the message does not explain the consequence: %v", err)
	}
}

// The repair. After advancing past the watermark, the ids a restore lost are
// never handed out again -- the gap is the point.
func TestAdvancingPastTheWatermarkStopsIdsBeingReused(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "users:alice", 10000)
	before, err := s.ChainTip(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		fund(t, ctx, s, "users:bob", 100)
	}
	lost, err := s.ChainTip(ctx)
	if err != nil {
		t.Fatal(err)
	}

	rewind(t, ctx, pool, before.LogID, before.TxID)

	if err := s.RecordRecovery(ctx, lost, "incident 41"); err != nil {
		t.Fatal(err)
	}

	// the next commit lands above everything that was ever issued
	fund(t, ctx, s, "users:carol", 500)
	now, err := s.ChainTip(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if now.LogID <= lost.LogID {
		t.Errorf("log id = %d, not above the lost watermark %d", now.LogID, lost.LogID)
	}
	if now.TxID <= lost.TxID {
		t.Errorf("tx id = %d, not above the lost watermark %d", now.TxID, lost.TxID)
	}

	// and the book is still sound after the repair
	assertConserved(t, ctx, pool)
	if _, err := s.VerifyLog(ctx); err != nil {
		t.Errorf("the chain broke across the repair: %v", err)
	}
}

// A ledger already past the watermark lost nothing, so recovery must be a
// no-op rather than appending an entry declaring a gap that is not there.
func TestRecoveringALedgerThatLostNothingDoesNothing(t *testing.T) {
	ctx, s, _ := testStore(t)
	fund(t, ctx, s, "users:alice", 10000)
	for range 3 {
		fund(t, ctx, s, "users:bob", 100)
	}
	before, err := s.ChainTip(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.RecordRecovery(ctx, Tip{Ledger: "main", LogID: 1, TxID: 1}, "spurious"); err != nil {
		t.Fatal(err)
	}

	after, err := s.ChainTip(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.LogID != before.LogID || after.TxID != before.TxID {
		t.Errorf("ids moved from %d/%d to %d/%d: a ledger that lost nothing was changed",
			before.TxID, before.LogID, after.TxID, after.LogID)
	}
}

// A ledger nothing has been written to has no position, and a zero must not
// read as one -- otherwise every fresh deployment fails its own check.
func TestAnEmptyLedgerHasNoTipToBeBehind(t *testing.T) {
	ctx, s, _ := testStore(t)

	tip, err := s.ChainTip(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if tip.LogID != 0 {
		t.Fatalf("a fresh ledger is at log %d", tip.LogID)
	}
	if err := s.CheckTip(ctx, tip); err != nil {
		t.Errorf("a fresh ledger failed its own tip check: %v", err)
	}
}

func TestAMalformedTipIsRefusedRatherThanGuessed(t *testing.T) {
	for _, s := range []string{"", "main", "main:notanumber:aaaa", "main:4:!!!!", "main:4"} {
		if _, err := ParseTip(s); err == nil {
			t.Errorf("ParseTip(%q) was accepted", s)
		}
	}
}

// The property that keeps the relaxation honest. A gap is believed only when
// the entry after it declares exactly that gap; everything else is still an
// entry somebody deleted.
func TestOnlyADeclaredGapIsAccepted(t *testing.T) {
	setup := func(t *testing.T) (context.Context, *Store, *pgxpool.Pool, Tip) {
		t.Helper()
		ctx, s, pool := testStore(t)
		fund(t, ctx, s, "users:alice", 10000)
		before, err := s.ChainTip(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for range 3 {
			fund(t, ctx, s, "users:bob", 100)
		}
		lost, err := s.ChainTip(ctx)
		if err != nil {
			t.Fatal(err)
		}
		rewind(t, ctx, pool, before.LogID, before.TxID)
		return ctx, s, pool, lost
	}

	t.Run("a declared gap verifies", func(t *testing.T) {
		ctx, s, _, lost := setup(t)
		if err := s.RecordRecovery(ctx, lost, "incident 41"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.VerifyLog(ctx); err != nil {
			t.Errorf("a declared gap was rejected: %v", err)
		}
	})

	t.Run("a gap in front of an ordinary entry is still broken", func(t *testing.T) {
		ctx, s, pool, lost := setup(t)
		if err := s.RecordRecovery(ctx, lost, "incident 41"); err != nil {
			t.Fatal(err)
		}
		// rewrite the declaration as an ordinary entry: the gap is now
		// unexplained, which is what a deleted entry looks like
		if _, err := pool.Exec(ctx, "alter table logs disable trigger user"); err != nil {
			t.Skip("needs the table owner")
		}
		defer pool.Exec(context.WithoutCancel(ctx), "alter table logs enable trigger user")
		if _, err := pool.Exec(ctx,
			"update logs set type='SET_METADATA' where ledger='main' and type='RECOVERY'"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.VerifyLog(ctx); !errors.Is(err, ErrChainBroken) {
			t.Errorf("err = %v, want ErrChainBroken: an undeclared gap was accepted", err)
		}
	})

	t.Run("a declaration that lies about the range is still broken", func(t *testing.T) {
		ctx, s, pool, lost := setup(t)
		if err := s.RecordRecovery(ctx, lost, "incident 41"); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, "alter table logs disable trigger user"); err != nil {
			t.Skip("needs the table owner")
		}
		defer pool.Exec(context.WithoutCancel(ctx), "alter table logs enable trigger user")

		// claim a smaller gap than the one actually present. this also breaks
		// the hash, so the point is only that the range is checked at all --
		// a declaration is not a blanket permission to be missing entries.
		if _, err := pool.Exec(ctx,
			`update logs set data = jsonb_set(data::jsonb, '{skippedThrough}', '2')::text::json
			  where ledger='main' and type='RECOVERY'`); err != nil {
			t.Fatal(err)
		}
		if _, err := s.VerifyLog(ctx); !errors.Is(err, ErrChainBroken) {
			t.Errorf("err = %v, want ErrChainBroken: a false declaration was believed", err)
		}
	})

	t.Run("deleting an entry with no declaration at all is still broken", func(t *testing.T) {
		ctx, s, pool := testStore(t)
		for range 4 {
			fund(t, ctx, s, "users:alice", 100)
		}
		if _, err := pool.Exec(ctx, "alter table logs disable trigger user"); err != nil {
			t.Skip("needs the table owner")
		}
		defer pool.Exec(context.WithoutCancel(ctx), "alter table logs enable trigger user")
		if _, err := pool.Exec(ctx, "delete from logs where ledger='main' and id=2"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.VerifyLog(ctx); !errors.Is(err, ErrChainBroken) {
			t.Errorf("err = %v, want ErrChainBroken", err)
		}
	})
}
