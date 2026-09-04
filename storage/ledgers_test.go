package storage

import (
	"errors"
	"testing"
)

func TestCreateAndReadLedger(t *testing.T) {
	ctx, s, _ := testStore(t)

	// testStore already created "main"
	got, err := s.GetLedger(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "main" || got.AddedAt.IsZero() {
		t.Errorf("got %+v", got)
	}
	if _, offset := got.AddedAt.Zone(); offset != 0 {
		t.Error("addedAt is not utc")
	}

	// a ledger holds the counters that allocate ids, so creating it twice
	// would be ambiguous rather than harmless
	if _, err := s.CreateLedger(ctx); !errors.Is(err, ErrLedgerExists) {
		t.Errorf("err = %v, want ErrLedgerExists", err)
	}
}

func TestGetLedgerNotFound(t *testing.T) {
	_, _, pool := testStore(t)
	absent := New(pool, "does-not-exist")

	if _, err := absent.GetLedger(t.Context()); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestCreateLedgerIsScoped(t *testing.T) {
	ctx, _, pool := testStore(t)

	other := New(pool, "second")
	if _, err := other.CreateLedger(ctx); err != nil {
		t.Fatal(err)
	}
	registerTestAssets(t, ctx, pool, "second")

	// each keeps its own counters, so both start at id 1
	a := New(pool, "main")
	b := New(pool, "second")
	first := mustCommit(t, ctx, a, "world", "alice", 100)
	second := mustCommit(t, ctx, b, "world", "alice", 100)
	if first.ID != 1 || second.ID != 1 {
		t.Errorf("ids %d and %d, want both 1", first.ID, second.ID)
	}
}
