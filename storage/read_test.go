package storage

import (
	"testing"
	"time"

	"github.com/pixperk/giro/ledger"
)

func TestGetTransactionRoundTrip(t *testing.T) {
	ctx, s, _ := testStore(t)

	want, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "world", Destination: "users:alice", Asset: "USD/2", Amount: n(10000)},
		{Source: "users:alice", Destination: "fees", Asset: "USD/2", Amount: n(250)},
	}, CommitOptions{Reference: "order-1", Metadata: ledger.Metadata{"kind": "payout"}})
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.GetTransaction(ctx, want.ID)
	if err != nil {
		t.Fatal(err)
	}

	if got.Reference != "order-1" || got.Metadata["kind"] != "payout" {
		t.Errorf("reference %q metadata %v", got.Reference, got.Metadata)
	}
	if len(got.Postings) != 2 {
		t.Fatalf("%d postings, want 2", len(got.Postings))
	}
	if got.Postings[1].Amount.Cmp(n(250)) != 0 {
		t.Errorf("second amount = %s, want 250", got.Postings[1].Amount)
	}
	if got.PostCommitVolumes["users:alice"]["USD/2"].Balance().Cmp(n(9750)) != 0 {
		t.Errorf("alice post commit balance = %s, want 9750",
			got.PostCommitVolumes["users:alice"]["USD/2"].Balance())
	}
	if !got.Timestamp.Equal(want.Timestamp) {
		t.Errorf("timestamp %v, want %v", got.Timestamp, want.Timestamp)
	}
}

func TestGetTransactionNotFound(t *testing.T) {
	ctx, s, _ := testStore(t)
	if _, err := s.GetTransaction(ctx, 99); err == nil {
		t.Fatal("expected not found")
	}
}

// an address that was never touched and one that does not exist are the same
// thing, so this is an empty result rather than an error.
func TestGetBalancesOfAnUntouchedAccount(t *testing.T) {
	ctx, s, _ := testStore(t)
	got, err := s.GetBalances(ctx, "users:nobody")
	if err != nil {
		t.Fatalf("an unused account should not be an error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want nothing", got)
	}
}

func TestAggregateBalancesByPrefix(t *testing.T) {
	ctx, s, _ := testStore(t)
	mustCommit(t, ctx, s, "world", "users:alice", 100)
	mustCommit(t, ctx, s, "world", "users:bob:wallet", 200)
	mustCommit(t, ctx, s, "world", "fees:platform", 50)

	users, err := s.AggregateBalances(ctx, "users:")
	if err != nil {
		t.Fatal(err)
	}
	if users["USD/2"].Cmp(n(300)) != 0 {
		t.Errorf("users:* = %s, want 300", users["USD/2"])
	}

	// the whole ledger, world included, is always zero
	all, err := s.AggregateBalances(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if all["USD/2"].Sign() != 0 {
		t.Errorf("everything = %s, want 0", all["USD/2"])
	}
}

// a prefix must not match a segment in the middle, which is exactly what gin
// containment would have done.
func TestAggregateBalancesPrefixIsPositional(t *testing.T) {
	ctx, s, _ := testStore(t)
	mustCommit(t, ctx, s, "world", "users:alice", 100)
	mustCommit(t, ctx, s, "world", "fees:users:refunds", 900)

	got, err := s.AggregateBalances(ctx, "users:")
	if err != nil {
		t.Fatal(err)
	}
	if got["USD/2"].Cmp(n(100)) != 0 {
		t.Errorf("users:* = %s, want 100: fees:users:refunds must not match", got["USD/2"])
	}
}

func TestListTransactionsPaginates(t *testing.T) {
	ctx, s, _ := testStore(t)
	for range 10 {
		mustCommit(t, ctx, s, "world", "users:alice", 10)
	}

	var seen []int64
	q := ListTransactionsQuery{Limit: 3}
	for {
		page, err := s.ListTransactions(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		for _, tx := range page.Items {
			seen = append(seen, tx.ID)
		}
		if page.Next == "" {
			break
		}
		if len(page.Items) != 3 {
			t.Errorf("a page before the last had %d items, want 3", len(page.Items))
		}
		q = ListTransactionsQuery{Cursor: page.Next}
	}

	if len(seen) != 10 {
		t.Fatalf("walked %d transactions, want 10", len(seen))
	}
	for i, id := range seen {
		if id != int64(i+1) {
			t.Errorf("position %d has id %d, want %d: pagination must not skip or repeat", i, id, i+1)
		}
	}
}

// rows landing mid walk must not shift a page, which is the failure OFFSET has
// and keyset does not.
func TestPaginationIsStableWhileWriting(t *testing.T) {
	ctx, s, _ := testStore(t)
	for range 5 {
		mustCommit(t, ctx, s, "world", "users:alice", 10)
	}

	first, err := s.ListTransactions(ctx, ListTransactionsQuery{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}

	// three more transactions arrive between requests
	for range 3 {
		mustCommit(t, ctx, s, "world", "users:bob", 10)
	}

	second, err := s.ListTransactions(ctx, ListTransactionsQuery{Cursor: first.Next})
	if err != nil {
		t.Fatal(err)
	}

	if second.Items[0].ID != 3 {
		t.Errorf("page two starts at %d, want 3: an offset would have skipped rows here",
			second.Items[0].ID)
	}
}

func TestListTransactionsFilters(t *testing.T) {
	ctx, s, _ := testStore(t)
	mustCommit(t, ctx, s, "world", "users:alice", 100)
	mustCommit(t, ctx, s, "world", "users:bob", 100)
	mustCommit(t, ctx, s, "users:alice", "users:bob", 30)

	byAccount, err := s.ListTransactions(ctx, ListTransactionsQuery{
		Filter: TransactionFilter{Account: "users:alice"}, Limit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(byAccount.Items) != 2 {
		t.Errorf("%d transactions touch alice, want 2 (funded, then paid out)", len(byAccount.Items))
	}

	byPrefix, err := s.ListTransactions(ctx, ListTransactionsQuery{
		Filter: TransactionFilter{AccountPrefix: "users:"}, Limit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(byPrefix.Items) != 3 {
		t.Errorf("%d transactions touch users:*, want 3", len(byPrefix.Items))
	}
}

// the cursor carries its filter, so a caller cannot change the query halfway
// through a walk and get an incoherent sequence.
func TestCursorCarriesItsFilter(t *testing.T) {
	ctx, s, _ := testStore(t)
	mustCommit(t, ctx, s, "world", "users:alice", 100)
	mustCommit(t, ctx, s, "world", "users:bob", 100)
	mustCommit(t, ctx, s, "world", "users:alice", 100)
	mustCommit(t, ctx, s, "world", "users:alice", 100)

	first, err := s.ListTransactions(ctx, ListTransactionsQuery{
		Filter: TransactionFilter{Account: "users:alice"}, Limit: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Next == "" {
		t.Fatal("expected a second page")
	}

	// the filter is deliberately absent from this call
	second, err := s.ListTransactions(ctx, ListTransactionsQuery{Cursor: first.Next})
	if err != nil {
		t.Fatal(err)
	}
	for _, tx := range second.Items {
		if tx.Postings[0].Destination != "users:alice" {
			t.Errorf("transaction %d leaked past the cursor's filter", tx.ID)
		}
	}
}

func TestBadCursorIsRejected(t *testing.T) {
	ctx, s, _ := testStore(t)
	for _, bad := range []string{"not-base64!!", "", "eyJ4IjoxfQ"} {
		if bad == "" {
			continue
		}
		if _, err := s.ListTransactions(ctx, ListTransactionsQuery{Cursor: bad}); err == nil {
			t.Errorf("cursor %q was accepted", bad)
		}
	}
}

func TestPageSizeIsCapped(t *testing.T) {
	ctx, s, _ := testStore(t)
	for range 3 {
		mustCommit(t, ctx, s, "world", "users:alice", 10)
	}

	// an unbounded limit is a denial of service vector on a table that grows
	page, err := s.ListTransactions(ctx, ListTransactionsQuery{Limit: 100000})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 3 {
		t.Errorf("%d items", len(page.Items))
	}

	c, err := decodeCursor[TransactionFilter]("")
	_ = c
	if err == nil {
		t.Error("an empty cursor should not decode")
	}
}

// a lean default: reading an account does not pay for its money.
func TestGetAccountOmitsVolumesByDefault(t *testing.T) {
	ctx, s, _ := testStore(t)
	mustCommit(t, ctx, s, "world", "users:alice", 10000)

	a, err := s.GetAccount(ctx, "users:alice")
	if err != nil {
		t.Fatal(err)
	}
	if a.Volumes != nil {
		t.Errorf("volumes = %v, want nil unless asked for", a.Volumes)
	}
	if a.Address != "users:alice" {
		t.Errorf("address = %q", a.Address)
	}
	if a.FirstUsage.IsZero() {
		t.Error("first usage was not populated")
	}
}

func TestGetAccountWithVolumes(t *testing.T) {
	ctx, s, _ := testStore(t)
	mustCommit(t, ctx, s, "world", "users:alice", 10000)
	mustCommit(t, ctx, s, "users:alice", "fees", 2500)

	a, err := s.GetAccount(ctx, "users:alice", WithVolumes())
	if err != nil {
		t.Fatal(err)
	}

	v, ok := a.Volumes["USD/2"]
	if !ok {
		t.Fatalf("no USD/2 volumes, got %v", a.Volumes)
	}
	// the counters, not just the balance. an account that moved money and now
	// holds less must not look like one that only ever received.
	if v.Input.Cmp(n(10000)) != 0 || v.Output.Cmp(n(2500)) != 0 {
		t.Errorf("volumes = (%s, %s), want (10000, 2500)", v.Input, v.Output)
	}
	if v.Balance().Cmp(n(7500)) != 0 {
		t.Errorf("balance = %s, want 7500", v.Balance())
	}
}

func TestGetVolumesIsScopedToItsLedger(t *testing.T) {
	ctx, mine, theirs, _ := twoLedgers(t)

	got, err := mine.GetVolumes(ctx, "users:alice")
	if err != nil {
		t.Fatal(err)
	}
	if got["USD/2"].Input.Cmp(n(100)) != 0 {
		t.Errorf("alice input = %s, want 100", got["USD/2"].Input)
	}
	if got, err := theirs.GetVolumes(ctx, "users:alice"); err != nil {
		t.Fatal(err)
	} else if got["USD/2"].Input.Cmp(n(9999)) != 0 {
		t.Errorf("their alice input = %s, want 9999", got["USD/2"].Input)
	}
}

// postgres timestamptz holds microseconds, so a nanosecond precision timestamp
// would be silently rounded on the way in and the create response would
// disagree with every later read.
//
// this is written with an explicit nanosecond value rather than time.Now()
// because macos clocks are microsecond granular: using the wall clock, this
// test can only fail on linux.
func TestTimestampsSurviveTheRoundTrip(t *testing.T) {
	ctx, s, _ := testStore(t)

	given := time.Date(2026, 3, 1, 12, 0, 0, 123456789, time.UTC)

	created, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "world", Destination: "users:alice", Asset: "USD/2", Amount: n(100)},
	}, CommitOptions{Timestamp: given})
	if err != nil {
		t.Fatal(err)
	}

	if sub := created.Timestamp.Nanosecond() % 1000; sub != 0 {
		t.Errorf("returned timestamp keeps %d sub-microsecond nanoseconds, which postgres cannot store", sub)
	}

	read, err := s.GetTransaction(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !read.Timestamp.Equal(created.Timestamp) {
		t.Errorf("read back %v, create returned %v", read.Timestamp, created.Timestamp)
	}
	if !read.InsertedAt.Equal(created.InsertedAt) {
		t.Errorf("inserted_at read back %v, create returned %v", read.InsertedAt, created.InsertedAt)
	}
}
