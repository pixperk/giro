package storage

import (
	"context"
	"errors"
	"testing"

	"github.com/pixperk/giro/ledger"
)

// the permission is closed unless someone opened it. a naming convention would
// mean a typo could join the exempt set; a default of true would mean
// forgetting to set it is indistinguishable from deciding not to.
func TestNegativeBalanceIsRefusedByDefault(t *testing.T) {
	ctx, s, _ := testStore(t)

	allowed, err := s.AllowsNegative(ctx, "cost:peg_absorption", "USD/2")
	if err != nil {
		t.Fatal(err)
	}
	if allowed {
		t.Error("an account nobody configured is allowed to go negative")
	}

	_, err = s.CommitTransaction(ctx, ledger.Postings{
		{Source: "cost:peg_absorption", Destination: "ops:usd", Asset: "USD/2", Amount: n(4000)},
	}, CommitOptions{})

	var insufficient *InsufficientFundsError
	if !errors.As(err, &insufficient) {
		t.Fatalf("err = %v, want InsufficientFundsError", err)
	}
}

// world carries the permission from creation rather than from a name the
// balance guard has to know about. it holds before anything has been written,
// because the answer must not depend on whether the row exists yet.
func TestWorldCarriesThePermissionFromTheStart(t *testing.T) {
	ctx, s, pool := testStore(t)

	allowed, err := s.AllowsNegative(ctx, ledger.WorldAccount, "USD/2")
	if err != nil {
		t.Fatal(err)
	}
	if !allowed {
		t.Fatal("world is not permitted a negative balance on an untouched ledger")
	}

	fund(t, ctx, s, "users:alice", 10000)

	if got := balance(t, ctx, pool, "world", "USD/2"); got.Int64() != -10000 {
		t.Errorf("world = %s, want -10000", got)
	}
	assertConserved(t, ctx, pool)
}

// taking the permission from world makes the next deposit into an empty ledger
// a refused overdraw of the account defined by being overdrawn, and every
// existing balance unreachable from outside. nobody means to do it.
func TestWorldCannotBeRefusedANegativeBalance(t *testing.T) {
	ctx, s, _ := testStore(t)

	if err := s.SetAllowNegative(ctx, ledger.WorldAccount, "USD/2", false); !errors.Is(err, ErrWorldMustAllowNegative) {
		t.Fatalf("err = %v, want ErrWorldMustAllowNegative", err)
	}

	// and it is still permitted afterwards, so the refusal changed nothing
	allowed, err := s.AllowsNegative(ctx, ledger.WorldAccount, "USD/2")
	if err != nil {
		t.Fatal(err)
	}
	if !allowed {
		t.Error("a refused call took the permission away anyway")
	}
}

// the permission can be set before the account has been touched, so a book is
// configurable at setup rather than only after value has moved through it.
//
// this is the case the materialising insert in lockVolumes could break: it
// creates missing rows on every commit, and if it did anything other than
// nothing on conflict it would reset a flag set in advance.
func TestPermissionSetBeforeFirstUseSurvivesIt(t *testing.T) {
	ctx, s, pool := testStore(t)

	if err := s.SetAllowNegative(ctx, "cost:peg_absorption", "USD/2", true); err != nil {
		t.Fatal(err)
	}

	if _, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "cost:peg_absorption", Destination: "ops:usd", Asset: "USD/2", Amount: n(4000)},
	}, CommitOptions{}); err != nil {
		t.Fatalf("a permitted account was refused: %v", err)
	}

	if got := balance(t, ctx, pool, "cost:peg_absorption", "USD/2"); got.Int64() != -4000 {
		t.Errorf("cost account = %s, want -4000", got)
	}

	allowed, err := s.AllowsNegative(ctx, "cost:peg_absorption", "USD/2")
	if err != nil {
		t.Fatal(err)
	}
	if !allowed {
		t.Error("the commit path reset a permission set before first use")
	}
	assertConserved(t, ctx, pool)
}

// per asset, not per account. an operating account that absorbs USD peg
// movements must still not be able to overdraw its USDT.
func TestPermissionIsScopedToOneAsset(t *testing.T) {
	ctx, s, _ := testStore(t)

	if err := s.SetAllowNegative(ctx, "ops:mixed", "USD/2", true); err != nil {
		t.Fatal(err)
	}

	if _, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "ops:mixed", Destination: "somewhere", Asset: "USD/2", Amount: n(500)},
	}, CommitOptions{}); err != nil {
		t.Fatalf("permitted asset was refused: %v", err)
	}

	_, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "ops:mixed", Destination: "somewhere", Asset: "USDT/6", Amount: n(500)},
	}, CommitOptions{})

	var insufficient *InsufficientFundsError
	if !errors.As(err, &insufficient) {
		t.Fatalf("err = %v, want the other asset to still be guarded", err)
	}
	if insufficient.Asset != "USDT/6" {
		t.Errorf("refused %s, want USDT/6", insufficient.Asset)
	}
}

// the flag is read from the row the commit path locks, so revoking it takes
// effect on the next transaction rather than at the next restart.
func TestPermissionCanBeRevoked(t *testing.T) {
	ctx, s, _ := testStore(t)

	if err := s.SetAllowNegative(ctx, "cost:lp_fee", "USD/2", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "cost:lp_fee", Destination: "ops:usd", Asset: "USD/2", Amount: n(998)},
	}, CommitOptions{}); err != nil {
		t.Fatal(err)
	}

	if err := s.SetAllowNegative(ctx, "cost:lp_fee", "USD/2", false); err != nil {
		t.Fatal(err)
	}

	_, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "cost:lp_fee", Destination: "ops:usd", Asset: "USD/2", Amount: n(1)},
	}, CommitOptions{})

	var insufficient *InsufficientFundsError
	if !errors.As(err, &insufficient) {
		t.Fatalf("err = %v, want the revoked account to be guarded again", err)
	}
}

// revoking leaves an already negative balance where it is, because the
// alternative is the ledger inventing a correcting transaction nobody
// authorised. so the state exists and something has to look for it: every
// other check passes, since nothing was tampered with and the posting that
// created it was legitimate.
func TestRevokingLeavesTheBalanceForTheDetector(t *testing.T) {
	ctx, s, _ := testStore(t)

	if err := s.SetAllowNegative(ctx, "cost:lp_fee", "USD/2", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "cost:lp_fee", Destination: "ops:usd", Asset: "USD/2", Amount: n(998)},
	}, CommitOptions{}); err != nil {
		t.Fatal(err)
	}

	if checked, err := s.VerifyBalancePermissions(ctx); err != nil {
		t.Fatalf("a permitted negative balance was reported: %v", err)
	} else if checked == 0 {
		t.Error("the detector examined nothing and reported success")
	}

	if err := s.SetAllowNegative(ctx, "cost:lp_fee", "USD/2", false); err != nil {
		t.Fatal(err)
	}

	// everything else still passes, which is the point of having this check
	if _, err := s.VerifyLog(ctx); err != nil {
		t.Errorf("log: %v", err)
	}
	if _, err := s.VerifyProjection(ctx); err != nil {
		t.Errorf("projection: %v", err)
	}

	var unpermitted *UnpermittedNegative
	_, err := s.VerifyBalancePermissions(ctx)
	if !errors.As(err, &unpermitted) {
		t.Fatalf("err = %v, want UnpermittedNegative", err)
	}
	if unpermitted.Account != "cost:lp_fee" || unpermitted.Balance.Int64() != -998 {
		t.Errorf("reported %s at %s, want cost:lp_fee at -998", unpermitted.Account, unpermitted.Balance)
	}
}

// a book where nobody revoked anything has nothing to report, and says so
// after looking rather than by not looking.
func TestBalancePermissionsAreCleanOnAHealthyBook(t *testing.T) {
	ctx, s, _ := testStore(t)
	fund(t, ctx, s, "users:alice", 10000)

	checked, err := s.VerifyBalancePermissions(ctx)
	if err != nil {
		t.Fatalf("healthy book reported: %v", err)
	}
	if checked != 2 {
		t.Errorf("checked %d rows, want 2 (world and alice)", checked)
	}
}

// the shape this whole feature exists for, with the real numbers from a
// $100,000 off ramp.
//
// the client is quoted a flat fee and paid at par, so $99,725.00 has to arrive
// whatever the market does. kraken paid 0.99960, so $99,960.00 came back
// rather than $100,000.00. revenue is $275.00 and only $235.00 is left in the
// operating account, and the missing $40.00 is not an error: it is what
// carrying the client's price risk cost today.
//
// booking it needs a cost account that goes negative. netting revenue down to
// $235.00 instead would balance too, and would hide the one number the
// business is taking risk on.
func TestPegAbsorptionBalancesTheBook(t *testing.T) {
	ctx, s, pool := testStore(t)

	const (
		fromLP     = 9_996_000 // $99,960.00 received for the stablecoin
		toClient   = 9_972_500 // $99,725.00 wired, fixed at quote time
		conversion = 25_000    // $250.00, 0.25% of notional
		wire       = 2_500     // $25.00 flat
		peg        = 4_000     // $40.00 absorbed
	)

	if err := s.SetAllowNegative(ctx, "cost:peg_absorption", "USD/2", true); err != nil {
		t.Fatal(err)
	}

	if _, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "world", Destination: "ops:usd", Asset: "USD/2", Amount: n(fromLP)},
	}, CommitOptions{Reference: "lp-settlement"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "ops:usd", Destination: "world", Asset: "USD/2", Amount: n(toClient)},
	}, CommitOptions{Reference: "client-wire"}); err != nil {
		t.Fatal(err)
	}

	// one transaction, so the fees and the absorption that funds them are
	// atomic. the guard checks the final state, so the operating account may
	// dip through zero inside it: at no point does an ordering of these three
	// postings need the money to be there first.
	if _, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "cost:peg_absorption", Destination: "ops:usd", Asset: "USD/2", Amount: n(peg)},
		{Source: "ops:usd", Destination: "revenue:conversion_fee", Asset: "USD/2", Amount: n(conversion)},
		{Source: "ops:usd", Destination: "revenue:wire_fee", Asset: "USD/2", Amount: n(wire)},
	}, CommitOptions{Reference: "book-the-deal"}); err != nil {
		t.Fatalf("booking the deal: %v", err)
	}

	want := map[string]int64{
		"ops:usd":                0, // a conduit, not a store. nothing stranded.
		"revenue:conversion_fee": conversion,
		"revenue:wire_fee":       wire,
		"cost:peg_absorption":    -peg,
		"world":                  -(fromLP - toClient),
	}
	for address, expected := range want {
		if got := balance(t, ctx, pool, address, "USD/2"); got.Int64() != expected {
			t.Errorf("%s = %s, want %d", address, got, expected)
		}
	}

	// revenue less the cost of carrying the peg is what the deal actually
	// made, and it is exactly what the operating account was left holding
	// before the fees were booked out of it
	if net := conversion + wire - peg; net != fromLP-toClient {
		t.Fatalf("net = %d, want %d", net, fromLP-toClient)
	}

	assertConserved(t, ctx, pool)
	if _, err := s.VerifyProjection(ctx); err != nil {
		t.Errorf("projection: %v", err)
	}
}

// a negative balance is still a balance: it has to reach the effective date
// history and the projection replay intact, not just accounts_volumes.
func TestNegativeBalancesSurviveVerification(t *testing.T) {
	ctx, s, _ := testStore(t)

	if err := s.SetAllowNegative(ctx, "cost:peg_absorption", "USD/2", true); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := s.CommitTransaction(ctx, ledger.Postings{
			{Source: "cost:peg_absorption", Destination: "ops:usd", Asset: "USD/2", Amount: n(1500)},
		}, CommitOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := s.VerifyLog(ctx); err != nil {
		t.Errorf("log: %v", err)
	}
	if _, err := s.VerifyProjection(ctx); err != nil {
		t.Errorf("projection: %v", err)
	}
	if _, err := s.VerifyEffectiveVolumes(ctx); err != nil {
		t.Errorf("effective volumes: %v", err)
	}
}

// a bad address or asset is refused rather than written, so a typo cannot
// leave a permission sitting on an account that can never be posted to.
func TestSetAllowNegativeValidatesItsArguments(t *testing.T) {
	ctx, s, _ := testStore(t)

	for _, tc := range []struct{ address, asset string }{
		{"", "USD/2"},
		{"a::b", "USD/2"},
		{"ops:usd", ""},
		{"ops:usd", "usd/2"},
	} {
		if err := s.SetAllowNegative(context.Background(), tc.address, tc.asset, true); err == nil {
			t.Errorf("SetAllowNegative(%q, %q) was accepted", tc.address, tc.asset)
		}
	}
	_ = ctx
}
