package storage

import (
	"errors"
	"strings"
	"testing"

	"github.com/pixperk/giro/ledger"
)

func TestAClosedAccountRefusesMovementBothWays(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "client:acme", 10_000)

	// empty it first, because closure requires that
	if _, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "client:acme", Destination: "treasury:usd", Asset: "USD/2", Amount: n(10_000)},
	}, CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := s.CloseAccount(ctx, "client:acme"); err != nil {
		t.Fatal(err)
	}

	// paying in is refused, not only paying out. a closed account holds
	// nothing, and the only thing an incoming posting could do is give it a
	// balance nobody is watching.
	for _, p := range []ledger.Postings{
		{{Source: "world", Destination: "client:acme", Asset: "USD/2", Amount: n(100)}},
		{{Source: "client:acme", Destination: "world", Asset: "USD/2", Amount: n(100)}},
	} {
		_, err := s.CommitTransaction(ctx, p, CommitOptions{})
		var closed *AccountClosedError
		if !errors.As(err, &closed) {
			t.Errorf("err = %v, want AccountClosedError", err)
		}
	}

	// and in a different asset, which is why closure is per account rather
	// than per (account, asset)
	_, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "world", Destination: "client:acme", Asset: "USDT/6", Amount: n(100)},
	}, CommitOptions{})
	var closed *AccountClosedError
	if !errors.As(err, &closed) {
		t.Errorf("a closed account accepted a different asset: %v", err)
	}
	assertConserved(t, ctx, pool)
}

// The rule that stops closure stranding money.
func TestAnAccountHoldingMoneyCannotBeClosed(t *testing.T) {
	ctx, s, _ := testStore(t)
	fund(t, ctx, s, "client:acme", 10_000)

	err := s.CloseAccount(ctx, "client:acme")
	if !errors.Is(err, ErrAccountNotEmpty) {
		t.Fatalf("err = %v, want ErrAccountNotEmpty", err)
	}
	// and it says what is in the way
	if !strings.Contains(err.Error(), "10000 USD/2") {
		t.Errorf("err = %v, want it to name the balance", err)
	}

	closed, err := s.IsClosed(ctx, "client:acme")
	if err != nil {
		t.Fatal(err)
	}
	if closed {
		t.Error("a refused closure closed the account anyway")
	}
}

// An account nothing has ever touched has no row at all, so closing it is
// recording a statement rather than editing a record.
func TestAnUntouchedAccountCanBeClosed(t *testing.T) {
	ctx, s, _ := testStore(t)

	if err := s.CloseAccount(ctx, "client:never-used"); err != nil {
		t.Fatal(err)
	}
	closed, err := s.IsClosed(ctx, "client:never-used")
	if err != nil {
		t.Fatal(err)
	}
	if !closed {
		t.Error("closing an untouched account recorded nothing")
	}

	_, err = s.CommitTransaction(ctx, ledger.Postings{
		{Source: "world", Destination: "client:never-used", Asset: "USD/2", Amount: n(1)},
	}, CommitOptions{})
	var refused *AccountClosedError
	if !errors.As(err, &refused) {
		t.Errorf("err = %v, want it refused", err)
	}
}

// The path for money that arrives after the relationship ended: reopen, deal
// with it, close again. Three deliberate acts rather than a hole in the guard.
func TestAReturnedWireAfterClosureIsHandledByReopening(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "client:acme", 10_000)

	// the wire goes out and the client is closed behind it
	commit(t, ctx, s, "wire-out", ledger.Postings{
		{Source: "client:acme", Destination: "pending:wire:W1", Asset: "USD/2", Amount: n(10_000)},
	})
	if err := s.CloseAccount(ctx, "client:acme"); err != nil {
		t.Fatal(err)
	}

	// it bounces. the return is refused, and the money stays where it is
	// rather than landing somewhere nothing watches.
	_, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "pending:wire:W1", Destination: "client:acme", Asset: "USD/2", Amount: n(10_000)},
	}, CommitOptions{})
	var closed *AccountClosedError
	if !errors.As(err, &closed) {
		t.Fatalf("err = %v, want the return refused", err)
	}
	if got := balance(t, ctx, pool, "pending:wire:W1", "USD/2"); got.Int64() != 10_000 {
		t.Errorf("holding = %s, want the money still there", got)
	}

	// so an operator reopens, resolves it, and closes again
	if err := s.ReopenAccount(ctx, "client:acme"); err != nil {
		t.Fatal(err)
	}
	commit(t, ctx, s, "wire-returned", ledger.Postings{
		{Source: "pending:wire:W1", Destination: "client:acme", Asset: "USD/2", Amount: n(10_000)},
	})
	commit(t, ctx, s, "paid-out", ledger.Postings{
		{Source: "client:acme", Destination: "world", Asset: "USD/2", Amount: n(10_000)},
	})
	if err := s.CloseAccount(ctx, "client:acme"); err != nil {
		t.Fatalf("closing again: %v", err)
	}
	assertConserved(t, ctx, pool)
	assertAllVerifiersPass(t, ctx, s)
}

// Closing world would refuse every deposit and every payout at once.
func TestWorldCannotBeClosed(t *testing.T) {
	ctx, s, _ := testStore(t)
	if err := s.CloseAccount(ctx, ledger.WorldAccount); !errors.Is(err, ErrAccountClosureRefused) {
		t.Errorf("err = %v, want it refused", err)
	}
}

func TestReopeningSomethingThatIsNotClosed(t *testing.T) {
	ctx, s, _ := testStore(t)
	if err := s.ReopenAccount(ctx, "client:acme"); err == nil {
		t.Error("reopening an open account reported success")
	}
}

// Closure locks the balances it can see, which stops a commit already in
// flight and not one that starts afterwards. Refusing that race would mean
// every commit locking the account row, which is the shape that deadlocks. So
// the state is reachable and something has to look for it.
func TestAClosedAccountHoldingMoneyIsFound(t *testing.T) {
	ctx, s, pool := testStore(t)

	if err := s.CloseAccount(ctx, "client:acme"); err != nil {
		t.Fatal(err)
	}
	checked, err := s.VerifyClosedAccounts(ctx)
	if err != nil {
		t.Fatalf("a properly closed account was reported: %v", err)
	}
	if checked != 1 {
		t.Errorf("examined %d closed accounts, want 1", checked)
	}

	// force the state the race would produce
	withoutGuards(t, ctx, pool, "accounts_volumes", "accounts")
	if _, err := pool.Exec(ctx, `
		insert into accounts_volumes (ledger, address, asset, input, output)
		values ('main', 'client:acme', 'USD/2', 500, 0)`); err != nil {
		t.Fatal(err)
	}

	var holds *ClosedAccountHoldsBalance
	if _, err := s.VerifyClosedAccounts(ctx); !errors.As(err, &holds) {
		t.Fatalf("err = %v, want ClosedAccountHoldsBalance", err)
	}
	if holds.Account != "client:acme" || holds.Balance.Int64() != 500 {
		t.Errorf("reported %s holding %s", holds.Account, holds.Balance)
	}
}
