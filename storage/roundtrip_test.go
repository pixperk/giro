package storage

import (
	"bytes"
	"errors"
	"testing"

	"github.com/pixperk/giro/ledger"
)

// Two round trips were removed from the commit path, and both removals traded
// a query for an assumption. These test the assumptions rather than the
// numbers: a commit that is faster and wrong is not an improvement.
//
//   - checkAssets remembers which assets are registered, so it stops asking.
//     The assumption is that registration is permanent.
//   - insertLog writes the entry and advances the chain tip in one statement.
//     The assumption is that a data modifying CTE is still one atomic unit
//     with its triggers intact.

// The cache holds positive answers only. An asset it has never seen is still
// asked about, so a typo is refused exactly as before -- and would be even if
// the cache were wrong, because the foreign key is the actual enforcement.
func TestRememberingRegisteredAssetsNeverHidesAnUnregisteredOne(t *testing.T) {
	ctx, s, pool := testStore(t)

	// warm the cache the ordinary way
	fund(t, ctx, s, "users:alice", 10000)
	if _, ok := s.registered.Load(ledger.Asset("USD/2")); !ok {
		t.Fatal("USD/2 was not remembered, so this test is not exercising the cache")
	}

	// the bug the registry exists for, with a warm cache in front of it
	_, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "world", Destination: "users:alice", Asset: "USDD/2", Amount: n(10000)},
	}, CommitOptions{})
	if !errors.Is(err, ErrUnknownAsset) {
		t.Fatalf("err = %v, want ErrUnknownAsset", err)
	}

	// and a mixed transaction, where one asset is cached and the other is not
	_, err = s.CommitTransaction(ctx, ledger.Postings{
		{Source: "world", Destination: "users:alice", Asset: "USD/2", Amount: n(1)},
		{Source: "world", Destination: "users:alice", Asset: "GBP/9", Amount: n(1)},
	}, CommitOptions{})
	if !errors.Is(err, ErrUnknownAsset) {
		t.Errorf("err = %v, want ErrUnknownAsset for the uncached half", err)
	}

	var rows int
	if err := pool.QueryRow(ctx,
		"select count(*) from accounts_volumes where ledger='main' and asset in ('USDD/2','GBP/9')").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("%d volume rows for unregistered assets", rows)
	}
	assertConserved(t, ctx, pool)
}

// Negatives are never remembered, which is what makes the cache safe across
// processes: another Store registering an asset is visible on the next commit
// rather than after a restart.
func TestAnAssetRegisteredElsewhereIsUsableImmediately(t *testing.T) {
	ctx, mine, theirs, _ := twoLedgers(t)

	// warm mine, and let it learn that JPY/0 is unknown
	fund(t, ctx, mine, "users:alice", 10000)
	if _, err := mine.CommitTransaction(ctx, ledger.Postings{
		{Source: "world", Destination: "users:alice", Asset: "JPY/0", Amount: n(100)},
	}, CommitOptions{}); !errors.Is(err, ErrUnknownAsset) {
		t.Fatalf("err = %v, want ErrUnknownAsset before registration", err)
	}

	// a different Store registers it on the same ledger, the way a second
	// process or a startup routine would
	other := New(theirs.pool, mine.ledger)
	if err := other.RegisterAsset(ctx, "JPY/0"); err != nil {
		t.Fatal(err)
	}

	// no restart, no invalidation call: the negative was never cached
	if _, err := mine.CommitTransaction(ctx, ledger.Postings{
		{Source: "world", Destination: "users:alice", Asset: "JPY/0", Amount: n(100)},
	}, CommitOptions{}); err != nil {
		t.Errorf("an asset registered by another store was still refused: %v", err)
	}
}

// The ledger's existence rides in the same query. A Store pointed at a ledger
// that does not exist must keep saying so rather than caching its way past it.
func TestAMissingLedgerIsNeverCachedAsPresent(t *testing.T) {
	ctx, s, pool := testStore(t)
	absent := New(pool, "no-such-ledger")

	for range 3 {
		_, err := absent.CommitTransaction(ctx, ledger.Postings{
			{Source: "world", Destination: "users:alice", Asset: "USD/2", Amount: n(1)},
		}, CommitOptions{})
		if !errors.Is(err, ErrLedgerNotFound) {
			t.Fatalf("err = %v, want ErrLedgerNotFound every time", err)
		}
	}
	if absent.ledgerSeen.Load() {
		t.Error("a ledger that does not exist was remembered as existing")
	}
	_ = s
}

// The combined statement has to leave exactly what two statements left: the
// entry written, and the chain tip pointing at it.
func TestTheEntryAndTheChainTipLandTogether(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "users:alice", 10000)

	for range 3 {
		if _, err := s.CommitTransaction(ctx, ledger.Postings{
			{Source: "users:alice", Destination: "users:bob", Asset: "USD/2", Amount: n(1)},
		}, CommitOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	var tipHash, newestHash []byte
	var tipID, newestID int64
	if err := pool.QueryRow(ctx,
		"select last_log_id, last_log_hash from ledgers where name='main'").Scan(&tipID, &tipHash); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		"select id, hash from logs where ledger='main' order by id desc limit 1").Scan(&newestID, &newestHash); err != nil {
		t.Fatal(err)
	}
	if tipID != newestID {
		t.Errorf("ledger tip is at %d, newest entry is %d", tipID, newestID)
	}
	if !bytes.Equal(tipHash, newestHash) {
		t.Errorf("the chain tip does not match the entry it should point at:\n  tip %x\n  log %x",
			tipHash, newestHash)
	}
	if _, err := s.VerifyLog(ctx); err != nil {
		t.Errorf("the chain does not verify: %v", err)
	}
}

// A refused transaction must leave neither half behind. If the CTE's update
// escaped the rollback, the chain tip would advance past an entry that was
// never written -- and every later verification would fail.
func TestARefusedCommitLeavesNeitherHalfOfTheCombinedStatement(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "users:alice", 10000)

	var beforeID int64
	var beforeHash []byte
	if err := pool.QueryRow(ctx,
		"select last_log_id, last_log_hash from ledgers where name='main'").Scan(&beforeID, &beforeHash); err != nil {
		t.Fatal(err)
	}

	if _, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "users:alice", Destination: "users:bob", Asset: "USD/2", Amount: n(99999999)},
	}, CommitOptions{}); err == nil {
		t.Fatal("expected a refusal")
	}

	var afterID int64
	var afterHash []byte
	if err := pool.QueryRow(ctx,
		"select last_log_id, last_log_hash from ledgers where name='main'").Scan(&afterID, &afterHash); err != nil {
		t.Fatal(err)
	}
	if afterID != beforeID || !bytes.Equal(afterHash, beforeHash) {
		t.Errorf("a refused commit moved the chain tip from %d/%x to %d/%x",
			beforeID, beforeHash, afterID, afterHash)
	}
	if _, err := s.VerifyLog(ctx); err != nil {
		t.Errorf("the chain does not verify after a refusal: %v", err)
	}
}

// The append-only guard sits on the logs table, and a CTE is still an insert
// as far as a trigger is concerned. This pins that, because "we combined two
// statements and the guards stopped firing" is the failure worth ruling out.
func TestTheGuardsStillFireThroughTheCombinedStatement(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "users:alice", 10000)

	refused(t, ctx, pool, "append only",
		"update logs set data = '{}' where ledger='main' and id=1")
	refused(t, ctx, pool, "append only",
		"delete from logs where ledger='main' and id=1")
	refused(t, ctx, pool, "ids would be reused",
		"update ledgers set last_log_id = 0 where name='main'")

	// still usable, and still sound
	fund(t, ctx, s, "users:carol", 100)
	if _, err := s.VerifyLog(ctx); err != nil {
		t.Errorf("the chain broke: %v", err)
	}
	assertConserved(t, ctx, pool)
}

// Concurrency is where a shared cache would show a race the single threaded
// tests cannot. sync.Map is safe, but the assertion worth making is that the
// commits still all land and the book still balances.
func TestTheAssetCacheIsSafeUnderConcurrentCommits(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "treasury", 1_000_000)

	const workers = 8
	errs := make(chan error, workers)
	for w := range workers {
		go func() {
			var last error
			for i := range 20 {
				dst := ledger.Address("payee:" + string(rune('a'+w)) + ":" + string(rune('a'+i)))
				if _, err := s.CommitTransaction(ctx, ledger.Postings{
					{Source: "treasury", Destination: dst, Asset: "USD/2", Amount: n(1)},
				}, CommitOptions{}); err != nil {
					last = err
				}
			}
			errs <- last
		}()
	}
	for range workers {
		if err := <-errs; err != nil {
			t.Errorf("concurrent commit failed: %v", err)
		}
	}

	assertConserved(t, ctx, pool)
	if _, err := s.VerifyLog(ctx); err != nil {
		t.Errorf("the chain broke under concurrency: %v", err)
	}
	if _, err := s.VerifyProjection(ctx); err != nil {
		t.Errorf("the projection drifted: %v", err)
	}
}
