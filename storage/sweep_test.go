package storage

import (
	"errors"
	"sync"
	"testing"

	"github.com/pixperk/giro/ledger"
)

// Moving whatever is there.
//
// A sweep takes what landed in a client's sub-wallet into the treasury, and
// the amount is not something the caller can know: reading the balance and
// then posting it is two operations with a gap. The balance grows in between
// and the sweep is short; it shrinks and the commit is refused. Neither
// corrupts anything and neither is the operation anybody wanted.

func sweepAll(from, to ledger.Address, asset ledger.Asset) ledger.Postings {
	return ledger.Postings{{Source: from, Destination: to, Asset: asset, UpTo: true}}
}

func TestASweepMovesWhateverIsThere(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "client:acme:wallet", 42_317)

	tx, err := s.CommitTransaction(ctx, sweepAll("client:acme:wallet", "treasury:usd", "USD/2"),
		CommitOptions{Reference: "sweep"})
	if err != nil {
		t.Fatal(err)
	}

	// the recorded posting is the figure that moved, not the ceiling that was
	// asked for: what the log says happened is what happened
	if got := tx.Postings[0].Amount; got.Int64() != 42_317 {
		t.Errorf("recorded %s, want 42317", got)
	}
	if tx.Postings[0].UpTo {
		t.Error("the transaction still says it was a ceiling")
	}
	if got := balance(t, ctx, pool, "client:acme:wallet", "USD/2"); got.Sign() != 0 {
		t.Errorf("wallet = %s, want empty", got)
	}
	if got := balance(t, ctx, pool, "treasury:usd", "USD/2"); got.Int64() != 42_317 {
		t.Errorf("treasury = %s, want 42317", got)
	}
	assertConserved(t, ctx, pool)
	assertAllVerifiersPass(t, ctx, s)
}

// A ceiling caps it. Sweeping "up to 10,000" from an account holding more
// leaves the rest.
func TestACeilingCapsTheSweep(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "client:acme:wallet", 42_317)

	if _, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "client:acme:wallet", Destination: "treasury:usd", Asset: "USD/2",
			Amount: n(10_000), UpTo: true},
	}, CommitOptions{}); err != nil {
		t.Fatal(err)
	}

	if got := balance(t, ctx, pool, "treasury:usd", "USD/2"); got.Int64() != 10_000 {
		t.Errorf("treasury = %s, want 10000", got)
	}
	if got := balance(t, ctx, pool, "client:acme:wallet", "USD/2"); got.Int64() != 32_317 {
		t.Errorf("wallet = %s, want 32317 left", got)
	}
}

// Sweeping an empty account records that the sweep ran and found nothing,
// rather than failing. A job that reports an error when there is simply no
// work makes "nothing to do" indistinguishable from "broken".
func TestSweepingAnEmptyAccountIsNotAnError(t *testing.T) {
	ctx, s, pool := testStore(t)

	tx, err := s.CommitTransaction(ctx, sweepAll("client:quiet:wallet", "treasury:usd", "USD/2"),
		CommitOptions{Reference: "quiet-sweep"})
	if err != nil {
		t.Fatalf("sweeping an empty account failed: %v", err)
	}
	if got := tx.Postings[0].Amount; got.Sign() != 0 {
		t.Errorf("moved %s from an empty account", got)
	}
	assertConserved(t, ctx, pool)
}

// The whole point: the amount is decided under the lock, so a commit racing
// the sweep cannot make it move the wrong figure. Every caller either sees
// its money swept or sweeps it itself, and the totals add up either way.
func TestConcurrentSweepsCannotMoveMoreThanExists(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "client:acme:wallet", 100_000)

	var wg sync.WaitGroup
	swept := make([]int64, 8)
	for i := range swept {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tx, err := s.CommitTransaction(ctx,
				sweepAll("client:acme:wallet", "treasury:usd", "USD/2"), CommitOptions{})
			if err == nil {
				swept[i] = tx.Postings[0].Amount.Int64()
			}
		}()
	}
	wg.Wait()

	var total int64
	for _, n := range swept {
		total += n
	}
	if total != 100_000 {
		t.Errorf("eight concurrent sweeps moved %d in total, want exactly 100000", total)
	}
	if got := balance(t, ctx, pool, "client:acme:wallet", "USD/2"); got.Sign() != 0 {
		t.Errorf("wallet = %s, want empty", got)
	}
	assertConserved(t, ctx, pool)
	assertAllVerifiersPass(t, ctx, s)
}

// Two sweeps of the same account in one transaction see each other. The first
// drains it and the second finds nothing, which is the same rule as any other
// posting: order decides what is there when each one runs.
func TestTwoSweepsInOneTransactionSeeEachOther(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "client:acme:wallet", 5_000)

	tx, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "client:acme:wallet", Destination: "treasury:a", Asset: "USD/2", UpTo: true},
		{Source: "client:acme:wallet", Destination: "treasury:b", Asset: "USD/2", UpTo: true},
	}, CommitOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if got := tx.Postings[0].Amount.Int64(); got != 5_000 {
		t.Errorf("first swept %d, want 5000", got)
	}
	if got := tx.Postings[1].Amount.Int64(); got != 0 {
		t.Errorf("second swept %d, want 0: the first took it", got)
	}
	assertConserved(t, ctx, pool)
}

// An account permitted a negative balance holds no determinate amount, so
// "everything it has" is not a number. world is the whole outside world.
func TestSweepingAnUnboundedAccountIsRefused(t *testing.T) {
	ctx, s, _ := testStore(t)
	fund(t, ctx, s, "users:alice", 1000)

	_, err := s.CommitTransaction(ctx, sweepAll("world", "users:alice", "USD/2"), CommitOptions{})

	var unbounded *UnboundedSweepError
	if !errors.As(err, &unbounded) {
		t.Fatalf("err = %v, want UnboundedSweepError", err)
	}
	if unbounded.Account != "world" {
		t.Errorf("named %s, want world", unbounded.Account)
	}
}

// A sweep alongside ordinary postings resolves against what the earlier ones
// left behind, not against the balance at the start.
func TestASweepSeesEarlierPostingsInTheSameTransaction(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "client:acme:wallet", 1_000)

	tx, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "world", Destination: "client:acme:wallet", Asset: "USD/2", Amount: n(500)},
		{Source: "client:acme:wallet", Destination: "treasury:usd", Asset: "USD/2", UpTo: true},
	}, CommitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := tx.Postings[1].Amount.Int64(); got != 1_500 {
		t.Errorf("swept %d, want 1500: the deposit above it counts", got)
	}
	assertConserved(t, ctx, pool)
}

// A ceiling is still validated like any other amount.
func TestANegativeCeilingIsRefused(t *testing.T) {
	ctx, s, _ := testStore(t)
	_, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "a", Destination: "b", Asset: "USD/2", Amount: n(-1), UpTo: true},
	}, CommitOptions{})

	var posting *PostingError
	if !errors.As(err, &posting) {
		t.Errorf("err = %v, want a validation error", err)
	}
}
