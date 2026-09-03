package storage

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pixperk/giro/ledger"
)

// The tenant boundary is the `ledger` column, and it is enforced by every
// query remembering to filter on it. Forgetting fails silently: the query
// returns MORE rows, not an error.
//
// So this test seeds two ledgers with deliberately colliding data, the same
// account addresses carrying different amounts, and runs every read path
// against one of them. A missing predicate shows up as another tenant's money.
//
// Add a case here whenever a read method is added. That is the whole point of
// the file.

func twoLedgers(t *testing.T) (context.Context, *Store, *Store, *pgxpool.Pool) {
	t.Helper()
	ctx, mine, pool := testStore(t)

	if _, err := pool.Exec(ctx, "insert into ledgers (name) values ('theirs')"); err != nil {
		t.Fatal(err)
	}
	theirs := New(pool, "theirs")

	// identical addresses in both ledgers, different amounts, so a leak is
	// unmistakable rather than merely a wrong count
	mustCommit(t, ctx, mine, "world", "users:alice", 100)
	mustCommit(t, ctx, mine, "world", "users:bob", 200)

	mustCommit(t, ctx, theirs, "world", "users:alice", 9999)
	mustCommit(t, ctx, theirs, "world", "users:bob", 8888)
	mustCommit(t, ctx, theirs, "world", "users:carol", 7777)

	return ctx, mine, theirs, pool
}

func mustCommit(t testing.TB, ctx context.Context, s *Store, from, to string, amount int64) *ledger.Transaction {
	t.Helper()
	tx, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: from, Destination: to, Asset: "USD/2", Amount: n(amount)},
	}, CommitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

func TestIsolationGetTransaction(t *testing.T) {
	ctx, mine, theirs, _ := twoLedgers(t)

	// both ledgers have a transaction 3, but only theirs
	if _, err := theirs.GetTransaction(ctx, 3); err != nil {
		t.Fatalf("their own transaction 3 should be readable: %v", err)
	}
	if tx, err := mine.GetTransaction(ctx, 3); err == nil {
		t.Errorf("read another ledger's transaction 3: %+v", tx)
	}

	// and matching ids must be different objects
	a, err := mine.GetTransaction(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	b, err := theirs.GetTransaction(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if a.Postings[0].Amount.Cmp(b.Postings[0].Amount) == 0 {
		t.Errorf("transaction 1 is identical across ledgers, amounts %s and %s",
			a.Postings[0].Amount, b.Postings[0].Amount)
	}
}

func TestIsolationGetBalances(t *testing.T) {
	ctx, mine, theirs, _ := twoLedgers(t)

	got, err := mine.GetBalances(ctx, "users:alice")
	if err != nil {
		t.Fatal(err)
	}
	if got["USD/2"].Cmp(n(100)) != 0 {
		t.Errorf("alice = %s, want 100: anything else is the other ledger's money", got["USD/2"])
	}

	// an account that exists only in the other ledger must look untouched
	if got, err := mine.GetBalances(ctx, "users:carol"); err != nil {
		t.Fatal(err)
	} else if len(got) != 0 {
		t.Errorf("carol has %v in this ledger, want nothing", got)
	}

	if got, err := theirs.GetBalances(ctx, "users:alice"); err != nil {
		t.Fatal(err)
	} else if got["USD/2"].Cmp(n(9999)) != 0 {
		t.Errorf("their alice = %s, want 9999", got["USD/2"])
	}
}

func TestIsolationAggregateBalances(t *testing.T) {
	ctx, mine, theirs, _ := twoLedgers(t)

	got, err := mine.AggregateBalances(ctx, "users:")
	if err != nil {
		t.Fatal(err)
	}
	if got["USD/2"].Cmp(n(300)) != 0 {
		t.Errorf("users:* = %s, want 300 (100 + 200)", got["USD/2"])
	}

	if got, err := theirs.AggregateBalances(ctx, "users:"); err != nil {
		t.Fatal(err)
	} else if got["USD/2"].Cmp(n(26664)) != 0 {
		t.Errorf("their users:* = %s, want 26664", got["USD/2"])
	}

	// the whole ledger, including world, must still balance to zero on its own
	if got, err := mine.AggregateBalances(ctx, ""); err != nil {
		t.Fatal(err)
	} else if got["USD/2"].Sign() != 0 {
		t.Errorf("conservation across one ledger = %s, want 0", got["USD/2"])
	}
}

func TestIsolationGetAccount(t *testing.T) {
	ctx, mine, theirs, _ := twoLedgers(t)

	if _, err := mine.GetAccount(ctx, "users:carol"); err == nil {
		t.Error("read an account that only exists in the other ledger")
	}
	if _, err := theirs.GetAccount(ctx, "users:carol"); err != nil {
		t.Errorf("their own account should be readable: %v", err)
	}
}

func TestIsolationListTransactions(t *testing.T) {
	ctx, mine, theirs, _ := twoLedgers(t)

	page, err := mine.ListTransactions(ctx, ListTransactionsQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 {
		t.Errorf("%d transactions, want 2: this ledger only committed two", len(page.Items))
	}
	for _, tx := range page.Items {
		if tx.Postings[0].Destination == "users:carol" {
			t.Errorf("transaction %d belongs to the other ledger", tx.ID)
		}
	}

	// filtering must not widen the scope either
	filtered, err := mine.ListTransactions(ctx, ListTransactionsQuery{
		Filter: TransactionFilter{Account: "users:alice"}, Limit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.Items) != 1 {
		t.Errorf("%d transactions touching alice, want 1", len(filtered.Items))
	}

	if page, err := theirs.ListTransactions(ctx, ListTransactionsQuery{Limit: 100}); err != nil {
		t.Fatal(err)
	} else if len(page.Items) != 3 {
		t.Errorf("%d transactions in the other ledger, want 3", len(page.Items))
	}
}

func TestIsolationListLogs(t *testing.T) {
	ctx, mine, theirs, _ := twoLedgers(t)

	page, err := mine.ListLogs(ctx, ListLogsQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 {
		t.Errorf("%d log entries, want 2", len(page.Items))
	}

	if page, err := theirs.ListLogs(ctx, ListLogsQuery{Limit: 100}); err != nil {
		t.Fatal(err)
	} else if len(page.Items) != 3 {
		t.Errorf("%d log entries in the other ledger, want 3", len(page.Items))
	}
}

// each ledger keeps its own chain. one must not be able to break the other,
// and verification must not walk across the boundary.
func TestIsolationVerifyLog(t *testing.T) {
	ctx, mine, theirs, pool := twoLedgers(t)

	if n, err := mine.VerifyLog(ctx); err != nil || n != 2 {
		t.Errorf("verified %d entries, err %v, want 2 and nil", n, err)
	}
	if n, err := theirs.VerifyLog(ctx); err != nil || n != 3 {
		t.Errorf("verified %d entries, err %v, want 3 and nil", n, err)
	}

	// tamper with one ledger only
	if _, err := pool.Exec(ctx,
		`update logs set data = replace(data::text, '9999', '1111')::json
		  where ledger = 'theirs' and id = 1`); err != nil {
		t.Fatal(err)
	}

	if _, err := theirs.VerifyLog(ctx); err == nil {
		t.Error("the tampered ledger should fail verification")
	}
	if _, err := mine.VerifyLog(ctx); err != nil {
		t.Errorf("an untouched ledger must still verify: %v", err)
	}
}

// ids restart per ledger, which is only true if allocation is scoped.
func TestIsolationIDsArePerLedger(t *testing.T) {
	ctx, mine, theirs, _ := twoLedgers(t)

	a := mustCommit(t, ctx, mine, "world", "users:dave", 1)
	b := mustCommit(t, ctx, theirs, "world", "users:dave", 1)

	if a.ID != 3 {
		t.Errorf("next id in this ledger = %d, want 3", a.ID)
	}
	if b.ID != 4 {
		t.Errorf("next id in the other ledger = %d, want 4", b.ID)
	}
}
