package storage

import (
	"errors"
	"strings"
	"testing"

	"github.com/pixperk/giro/ledger"
)

// The bug the registry exists for. Before it, this posting was accepted:
// "USDD/2" is a well formed asset, it is a different asset from "USD/2", the
// two never mix, and nothing raised an error. The ledger ended up holding
// dollars in two piles, and conservation passed because each pile balanced on
// its own.
func TestATypoInAnAssetIsRefusedRatherThanBecomingASecondCurrency(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "users:alice", 10000)

	_, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "world", Destination: "users:alice", Asset: "USDD/2", Amount: n(10000)},
	}, CommitOptions{})
	if !errors.Is(err, ErrUnknownAsset) {
		t.Fatalf("err = %v, want ErrUnknownAsset", err)
	}

	// and it left nothing behind, which is the half that would otherwise be
	// invisible
	var rows int
	if err := pool.QueryRow(ctx,
		"select count(*) from accounts_volumes where ledger='main' and asset='USDD/2'").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("%d volume rows for an unregistered asset", rows)
	}
	assertConserved(t, ctx, pool)
}

// A mistyped scale is the likely mistake rather than a mistyped code, because
// the scale is the part a caller has to remember. So the error says what this
// ledger does have.
func TestTheErrorNamesTheSpellingTheLedgerKnows(t *testing.T) {
	ctx, s, _ := testStore(t)

	_, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "world", Destination: "users:alice", Asset: "USD/6", Amount: n(10000)},
	}, CommitOptions{})

	var unknown *UnknownAssetError
	if !errors.As(err, &unknown) {
		t.Fatalf("err = %v, want UnknownAssetError", err)
	}
	if unknown.Registered != "USD/2" {
		t.Errorf("registered = %q, want USD/2: the caller needs to be told what exists", unknown.Registered)
	}
}

// One scale per currency, which is the constraint that closes the bug rather
// than merely reporting it. Without it a ledger could register both spellings
// and be back where it started.
func TestALedgerHoldsOneScalePerCurrency(t *testing.T) {
	ctx, s, _ := testStore(t)

	for _, asset := range []ledger.Asset{"USD/6", "USD", "USD/0"} {
		t.Run(string(asset), func(t *testing.T) {
			err := s.RegisterAsset(ctx, asset)
			if err == nil {
				t.Fatalf("registered %s alongside USD/2", asset)
			}
			if !strings.Contains(err.Error(), "USD/2") {
				t.Errorf("err = %v, want it to name the conflicting registration", err)
			}
		})
	}

	// a different currency at any scale is fine
	if err := s.RegisterAsset(ctx, "GBP/2"); err != nil {
		t.Errorf("GBP/2: %v", err)
	}
}

// A startup routine declaring a chart of accounts runs on every boot, so
// registering what is already there cannot be an error.
func TestRegisteringTwiceIsANoOp(t *testing.T) {
	ctx, s, _ := testStore(t)

	for range 3 {
		if err := s.RegisterAsset(ctx, "GBP/2"); err != nil {
			t.Fatal(err)
		}
	}

	assets, err := s.Assets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	for _, a := range assets {
		if a == "GBP/2" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("GBP/2 appears %d times, want 1", count)
	}
}

// The registry is per ledger, because two entities need not deal in the same
// currencies. It is also the tenant boundary doing its usual job.
func TestTheRegistryIsPerLedger(t *testing.T) {
	ctx, mine, theirs, _ := twoLedgers(t)

	if err := mine.RegisterAsset(ctx, "JPY/0"); err != nil {
		t.Fatal(err)
	}

	// registered on mine, unknown on theirs
	if _, err := theirs.CommitTransaction(ctx, ledger.Postings{
		{Source: "world", Destination: "users:alice", Asset: "JPY/0", Amount: n(100)},
	}, CommitOptions{}); !errors.Is(err, ErrUnknownAsset) {
		t.Errorf("err = %v, want ErrUnknownAsset on the other ledger", err)
	}

	// and the other ledger may register the same currency at a different
	// scale, which is its business and not ours
	if err := theirs.RegisterAsset(ctx, "JPY/2"); err != nil {
		t.Errorf("theirs cannot choose its own scale: %v", err)
	}
}

// A batch is refused as a whole, naming the item, before anything is locked.
func TestABatchNamesTheItemWithTheUnknownAsset(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "users:alice", 10000)

	_, err := s.CommitBatch(ctx, []BatchItem{
		{Postings: ledger.Postings{{Source: "users:alice", Destination: "users:bob", Asset: "USD/2", Amount: n(100)}}},
		{Postings: ledger.Postings{{Source: "users:alice", Destination: "users:bob", Asset: "GBP/2", Amount: n(100)}}},
	}, CommitOptions{})

	var item *BatchItemError
	if !errors.As(err, &item) {
		t.Fatalf("err = %v, want BatchItemError", err)
	}
	if item.Index != 1 {
		t.Errorf("index = %d, want 1", item.Index)
	}
	if !errors.Is(err, ErrUnknownAsset) {
		t.Errorf("err = %v, does not unwrap to ErrUnknownAsset", err)
	}

	// the whole batch is refused before anything is locked, so the valid first
	// item did not land either. bob has no volume row at all rather than a
	// zero one, which is the difference between "nothing happened" and
	// "something happened and netted out".
	var rows int
	if err := pool.QueryRow(ctx,
		"select count(*) from accounts_volumes where ledger='main' and address='users:bob'").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("%d volume rows for bob, want none: the batch was not atomic", rows)
	}
}

// Registration is permanent. Re-registering a currency at a different scale
// would reinterpret every amount already recorded in it, which is a silent
// restatement of the book rather than a correction.
func TestRegistrationCannotBeUndone(t *testing.T) {
	ctx, s, pool := testStore(t)

	refused(t, ctx, pool, "registration is permanent",
		"delete from assets where ledger='main' and asset='USD/2'")
	refused(t, ctx, pool, "registration is permanent",
		"update assets set asset='USD/6' where ledger='main' and asset='USD/2'")
	refused(t, ctx, pool, "append only", "truncate assets cascade")

	// and it is still usable afterwards
	fund(t, ctx, s, "users:alice", 10000)
	assertConserved(t, ctx, pool)
}

// The foreign keys are the enforcement; the Go check exists for the error. So
// raw SQL that skips the application is refused too.
func TestRawSQLCannotUseAnUnregisteredAsset(t *testing.T) {
	ctx, _, pool := testStore(t)

	_, err := pool.Exec(ctx,
		"insert into accounts_volumes (ledger, address, asset) values ('main','users:alice','GBP/2')")
	if err == nil {
		t.Fatal("an unregistered asset was accepted by raw sql")
	}
	if !strings.Contains(err.Error(), "asset_registered") {
		t.Errorf("err = %v, want the foreign key", err)
	}
}
