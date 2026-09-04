package storage

import (
	"errors"
	"testing"
	"time"

	"github.com/pixperk/giro/ledger"
)

// Money in flight, modelled as a holding account. The wire is submitted at one
// moment and confirmed at another, and between the two the money is in neither
// place.
//
// Nothing here needs anything the engine did not already have. That is the
// argument for the pattern: a status column would need a mutable money table,
// and after the append-only guards there is no such thing.

const wire = "pending:wire:WR-2026-0142"

func TestMoneyInFlightIsInNeitherAccount(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "client:acme", 100_000_00)

	// submitted. it has left the client and has not reached the bank.
	commit(t, ctx, s, "wire-submitted", ledger.Postings{
		{Source: "client:acme", Destination: wire, Asset: "USD/2", Amount: n(99_725_00)},
	})

	if got := balance(t, ctx, pool, "client:acme", "USD/2"); got.Int64() != 275_00 {
		t.Errorf("acme = %s, want 27500: the wire has left", got)
	}
	if got := balance(t, ctx, pool, wire, "USD/2"); got.Int64() != 99_725_00 {
		t.Errorf("holding = %s, want 9972500", got)
	}
	// the bank has no row at all rather than a zero one: nothing has reached
	// it, which is a stronger statement than a balance of nothing
	atBank, err := s.GetBalances(ctx, "external:bank:infinitus:USD")
	if err != nil {
		t.Fatal(err)
	}
	if len(atBank) != 0 {
		t.Errorf("bank = %v, want nothing: it has not arrived", atBank)
	}

	// settled.
	commit(t, ctx, s, "wire-settled", ledger.Postings{
		{Source: wire, Destination: "external:bank:infinitus:USD", Asset: "USD/2", Amount: n(99_725_00)},
	})

	if got := balance(t, ctx, pool, wire, "USD/2"); got.Sign() != 0 {
		t.Errorf("holding = %s, want 0: the wire landed", got)
	}
	assertConserved(t, ctx, pool)
	assertAllVerifiersPass(t, ctx, s)
}

// The other ending. Nothing about the client's balance was ever a lie: the
// money left when it left and came back when it came back.
func TestAReturnedWireGoesBackToThePayer(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "client:acme", 100_000_00)

	commit(t, ctx, s, "wire-submitted", ledger.Postings{
		{Source: "client:acme", Destination: wire, Asset: "USD/2", Amount: n(99_725_00)},
	})
	commit(t, ctx, s, "wire-returned", ledger.Postings{
		{Source: wire, Destination: "client:acme", Asset: "USD/2", Amount: n(99_725_00)},
	})

	if got := balance(t, ctx, pool, "client:acme", "USD/2"); got.Int64() != 100_000_00 {
		t.Errorf("acme = %s, want 10000000: made whole", got)
	}
	if got := balance(t, ctx, pool, wire, "USD/2"); got.Sign() != 0 {
		t.Errorf("holding = %s, want 0", got)
	}
	assertConserved(t, ctx, pool)
}

// The property that makes the pattern safe rather than merely tidy, and it is
// not a new mechanism: the holding account holds the amount exactly once, so
// paying it out twice is an overdraw of an account nobody permitted.
func TestAWireCannotBeSettledTwice(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "client:acme", 100_000_00)

	commit(t, ctx, s, "wire-submitted", ledger.Postings{
		{Source: "client:acme", Destination: wire, Asset: "USD/2", Amount: n(99_725_00)},
	})
	commit(t, ctx, s, "wire-settled", ledger.Postings{
		{Source: wire, Destination: "external:bank:infinitus:USD", Asset: "USD/2", Amount: n(99_725_00)},
	})

	_, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: wire, Destination: "external:bank:infinitus:USD", Asset: "USD/2", Amount: n(99_725_00)},
	}, CommitOptions{Reference: "wire-settled-again"})

	var insufficient *InsufficientFundsError
	if !errors.As(err, &insufficient) {
		t.Fatalf("err = %v, want the second settlement refused", err)
	}
	if insufficient.Account != wire {
		t.Errorf("refused %s, want the holding account", insufficient.Account)
	}
	assertConserved(t, ctx, pool)
}

// Total value in transit is a prefix read rather than a report, which is the
// operational payoff of holding the money somewhere rather than labelling it.
func TestValueInTransitIsOneRead(t *testing.T) {
	ctx, s, _ := testStore(t)
	fund(t, ctx, s, "client:acme", 100_000_00)
	fund(t, ctx, s, "client:beta", 50_000_00)

	commit(t, ctx, s, "acme-wire", ledger.Postings{
		{Source: "client:acme", Destination: "pending:wire:A1", Asset: "USD/2", Amount: n(99_725_00)},
	})
	commit(t, ctx, s, "beta-wire", ledger.Postings{
		{Source: "client:beta", Destination: "pending:wire:B1", Asset: "USD/2", Amount: n(12_400_00)},
	})
	commit(t, ctx, s, "acme-settled", ledger.Postings{
		{Source: "pending:wire:A1", Destination: "external:bank:infinitus:USD", Asset: "USD/2", Amount: n(99_725_00)},
	})

	inTransit, err := s.AggregateBalances(ctx, "pending:")
	if err != nil {
		t.Fatal(err)
	}
	if got := inTransit["USD/2"]; got == nil || got.Int64() != 12_400_00 {
		t.Errorf("in transit = %v, want 1240000: only beta's is still in the air", got)
	}
}

// The gap the pattern does not close on its own. A wire that neither settles
// nor returns leaves money in the holding account indefinitely, and every
// check passes while it does: conservation holds, the chain is intact, the
// projection agrees. Nothing is wrong with the arithmetic, which is exactly
// why nothing notices.
func TestAStuckWireIsFoundByNothingElse(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "client:acme", 100_000_00)

	commit(t, ctx, s, "wire-submitted", ledger.Postings{
		{Source: "client:acme", Destination: wire, Asset: "USD/2", Amount: n(99_725_00)},
	})

	// every other check is happy
	assertConserved(t, ctx, pool)
	assertAllVerifiersPass(t, ctx, s)

	// nothing is stale yet, because it was submitted a moment ago
	fresh, err := s.StaleBalances(ctx, "pending:", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh) != 0 {
		t.Errorf("a wire submitted moments ago is already stale: %v", fresh)
	}

	// and with no grace period at all it is exactly what is sitting still
	stuck, err := s.StaleBalances(ctx, "pending:", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(stuck) != 1 {
		t.Fatalf("found %d, want 1: %v", len(stuck), stuck)
	}
	if stuck[0].Account != wire {
		t.Errorf("found %s, want %s", stuck[0].Account, wire)
	}
	if stuck[0].Balance.Int64() != 99_725_00 {
		t.Errorf("balance = %s, want 9972500", stuck[0].Balance)
	}
	if stuck[0].LastMove.IsZero() {
		t.Error("no last movement recorded")
	}
}

// A settled wire is not stuck. The holding account is empty, so it is not a
// balance sitting still, it is no balance at all.
func TestASettledWireIsNotStale(t *testing.T) {
	ctx, s, _ := testStore(t)
	fund(t, ctx, s, "client:acme", 100_000_00)

	commit(t, ctx, s, "wire-submitted", ledger.Postings{
		{Source: "client:acme", Destination: wire, Asset: "USD/2", Amount: n(99_725_00)},
	})
	commit(t, ctx, s, "wire-settled", ledger.Postings{
		{Source: wire, Destination: "external:bank:infinitus:USD", Asset: "USD/2", Amount: n(99_725_00)},
	})

	stuck, err := s.StaleBalances(ctx, "pending:", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(stuck) != 0 {
		t.Errorf("a settled wire is reported stuck: %v", stuck)
	}
}

// It is a general question about balances that are not moving, not a hold
// specific one, so it answers the other versions of that question too.
func TestStaleBalancesIsAboutAnyPrefix(t *testing.T) {
	ctx, s, _ := testStore(t)
	fund(t, ctx, s, "client:dormant", 5_000_00)
	fund(t, ctx, s, "ops:usd", 1_000_00)

	// an operating account that should be a conduit and is not
	conduit, err := s.StaleBalances(ctx, "ops:", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(conduit) != 1 || conduit[0].Account != "ops:usd" {
		t.Errorf("ops = %v, want ops:usd holding something it should not", conduit)
	}

	// and the whole ledger, which includes world and is rarely the question
	// being asked
	all, err := s.StaleBalances(ctx, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) < 3 {
		t.Errorf("found %d over the whole ledger, want world and both accounts", len(all))
	}

	// oldest first, so a sweep reads top down
	for i := 1; i < len(all); i++ {
		if all[i].LastMove.Before(all[i-1].LastMove) {
			t.Errorf("not ordered oldest first at %d", i)
		}
	}
}

// A negative olderThan would silently mean "in the future", and the caller
// almost certainly meant the sign the other way.
func TestStaleBalancesRejectsANegativeWindow(t *testing.T) {
	ctx, s, _ := testStore(t)
	if _, err := s.StaleBalances(ctx, "pending:", -time.Hour); err == nil {
		t.Error("a negative window was accepted")
	}
}
