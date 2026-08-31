package storage

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/pixperk/giro/internal/ledger"
)

func item(from, to string, amount int64) BatchItem {
	return BatchItem{Postings: ledger.Postings{
		{Source: from, Destination: to, Asset: "USD/2", Amount: n(amount)},
	}}
}

func TestBatchCommitsEverything(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "alice", 10000)

	out, err := s.CommitBatch(ctx, []BatchItem{
		item("alice", "bob", 100),
		item("alice", "carol", 200),
		item("alice", "dave", 300),
	}, CommitOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if len(out) != 3 {
		t.Fatalf("%d transactions, want 3", len(out))
	}
	// ids are contiguous: nothing can interleave, because allocation holds the
	// ledgers row lock for the whole batch
	for i, tx := range out {
		if tx.ID != int64(i+2) {
			t.Errorf("transaction %d has id %d, want %d", i, tx.ID, i+2)
		}
	}
	if got := balance(t, ctx, pool, "alice", "USD/2"); got.Cmp(n(9400)) != 0 {
		t.Errorf("alice = %s, want 9400", got)
	}
	if got := logCount(t, ctx, pool); got != 4 {
		t.Errorf("%d log entries, want 4: one per transaction", got)
	}
	assertConserved(t, ctx, pool)
	if _, err := s.VerifyProjection(ctx); err != nil {
		t.Errorf("projection: %v", err)
	}
}

// all or nothing. an item that cannot be applied takes the whole batch with it.
func TestBatchIsAtomic(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "alice", 500)

	_, err := s.CommitBatch(ctx, []BatchItem{
		item("alice", "bob", 100),
		item("alice", "carol", 900), // more than alice has
		item("alice", "dave", 100),
	}, CommitOptions{})

	var itemErr *BatchItemError
	if !errors.As(err, &itemErr) {
		t.Fatalf("err = %v, want BatchItemError", err)
	}
	if itemErr.Index != 1 {
		t.Errorf("failed at index %d, want 1", itemErr.Index)
	}
	var insufficient *InsufficientFundsError
	if !errors.As(err, &insufficient) {
		t.Errorf("err does not unwrap to insufficient funds: %v", err)
	}

	// the first item must not have survived
	if got := balance(t, ctx, pool, "alice", "USD/2"); got.Cmp(n(500)) != 0 {
		t.Errorf("alice = %s, want 500: a failed batch must move nothing", got)
	}
	bob, _ := s.GetBalances(ctx, "bob")
	if len(bob) != 0 {
		t.Errorf("bob = %v, want nothing", bob)
	}
	if got := logCount(t, ctx, pool); got != 1 {
		t.Errorf("%d log entries, want 1", got)
	}
}

// an item can spend what an earlier item in the same batch provided.
func TestBatchItemsSeeEachOther(t *testing.T) {
	ctx, s, pool := testStore(t)

	out, err := s.CommitBatch(ctx, []BatchItem{
		item("world", "treasury", 1000),
		item("treasury", "alice", 600),
		item("treasury", "bob", 400),
	}, CommitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 {
		t.Fatalf("%d transactions", len(out))
	}
	if got := balance(t, ctx, pool, "treasury", "USD/2"); got.Sign() != 0 {
		t.Errorf("treasury = %s, want 0", got)
	}
	assertConserved(t, ctx, pool)
}

func TestBatchValidatesBeforeTouchingAnything(t *testing.T) {
	ctx, s, pool := testStore(t)

	_, err := s.CommitBatch(ctx, []BatchItem{
		item("world", "alice", 100),
		{Postings: ledger.Postings{{Source: "world", Destination: "bob", Asset: "usd", Amount: n(1)}}},
	}, CommitOptions{})

	var itemErr *BatchItemError
	if !errors.As(err, &itemErr) || itemErr.Index != 1 {
		t.Fatalf("err = %v, want BatchItemError at index 1", err)
	}
	if got := logCount(t, ctx, pool); got != 0 {
		t.Errorf("%d log entries, want 0", got)
	}
}

func TestBatchSizeIsCapped(t *testing.T) {
	ctx, s, _ := testStore(t)

	items := make([]BatchItem, MaxBatchSize+1)
	for i := range items {
		items[i] = item("world", "alice", 1)
	}
	if _, err := s.CommitBatch(ctx, items, CommitOptions{}); !errors.Is(err, ErrBatchTooLarge) {
		t.Errorf("err = %v, want ErrBatchTooLarge", err)
	}

	if _, err := s.CommitBatch(ctx, nil, CommitOptions{}); !errors.Is(err, ErrNoPostings) {
		t.Errorf("empty batch err = %v, want ErrNoPostings", err)
	}
}

func TestBatchIdempotency(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "alice", 10000)

	items := []BatchItem{item("alice", "bob", 100), item("alice", "carol", 200)}
	opts := CommitOptions{IdempotencyKey: "batch-1"}

	first, err := s.CommitBatch(ctx, items, opts)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CommitBatch(ctx, items, opts)
	if err != nil {
		t.Fatal(err)
	}

	if len(second) != len(first) {
		t.Fatalf("replay returned %d transactions, want %d", len(second), len(first))
	}
	for i := range first {
		if first[i].ID != second[i].ID {
			t.Errorf("item %d: replay gave id %d, original was %d", i, second[i].ID, first[i].ID)
		}
	}
	if got := balance(t, ctx, pool, "alice", "USD/2"); got.Cmp(n(9700)) != 0 {
		t.Errorf("alice = %s, want 9700: the batch must apply once", got)
	}

	// the same key with a different batch is a client bug, not a replay
	_, err = s.CommitBatch(ctx, []BatchItem{item("alice", "bob", 999)}, opts)
	if !errors.Is(err, ErrIdempotencyMismatch) {
		t.Errorf("err = %v, want ErrIdempotencyMismatch", err)
	}
}

func TestBatchDryRun(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "alice", 10000)

	out, err := s.CommitBatch(ctx, []BatchItem{
		item("alice", "bob", 100),
		item("alice", "carol", 200),
	}, CommitOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("%d previews, want 2", len(out))
	}
	if got := balance(t, ctx, pool, "alice", "USD/2"); got.Cmp(n(10000)) != 0 {
		t.Errorf("alice = %s, want 10000: a dry run moves nothing", got)
	}
	if got := logCount(t, ctx, pool); got != 1 {
		t.Errorf("%d log entries, want 1", got)
	}
}

// the reason every lock is taken up front, sorted, before any item runs.
//
// two batches touching the same accounts in opposite item order would
// otherwise each hold half of what the other needs.
func TestOppositelyOrderedBatchesDoNotDeadlock(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "a", 100000)
	fund(t, ctx, s, "b", 100000)
	fund(t, ctx, s, "c", 100000)
	fund(t, ctx, s, "d", 100000)

	const rounds = 25
	var wg sync.WaitGroup
	errs := make([]error, rounds*2)

	for i := range rounds {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, errs[i*2] = s.CommitBatch(ctx, []BatchItem{
				item("a", "b", 1), item("c", "d", 1),
			}, CommitOptions{})
		}()
		go func() {
			defer wg.Done()
			_, errs[i*2+1] = s.CommitBatch(ctx, []BatchItem{
				item("c", "d", 1), item("a", "b", 1),
			}, CommitOptions{})
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("batch %d: %v", i, err)
		}
	}
	if got := s.Retries(); got != 0 {
		t.Errorf("%d retries, want 0: locking the union up front should make deadlock impossible, not survivable", got)
	}
	assertConserved(t, ctx, pool)
}

func TestBatchErrorNamesTheItem(t *testing.T) {
	ctx, s, _ := testStore(t)
	_, err := s.CommitBatch(ctx, []BatchItem{
		item("world", "alice", 100),
		item("world", "bob", 100),
		{Postings: ledger.Postings{{Source: "bad address", Destination: "x", Asset: "USD/2", Amount: n(1)}}},
	}, CommitOptions{})

	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "transactions[2]") {
		t.Errorf("err = %v, want it to name the failing index", err)
	}
}
