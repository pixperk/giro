package storage

import (
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/pixperk/giro/ledger"
)

func TestRevertReturnsBalancesButNotVolumes(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "users:alice", 10000)
	payment := mustCommit(t, ctx, s, "users:alice", "users:bob", 3000)

	result, err := s.RevertTransaction(ctx, payment.ID, RevertOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if result.Reversal.ID == payment.ID {
		t.Error("the reversal must be its own transaction, not an edit of the original")
	}
	if result.Original.RevertedAt == nil {
		t.Error("the original was not marked")
	}

	// balances are back
	if got := balance(t, ctx, pool, "users:alice", "USD/2"); got.Cmp(n(10000)) != 0 {
		t.Errorf("alice = %s, want 10000", got)
	}
	if got := balance(t, ctx, pool, "users:bob", "USD/2"); got.Sign() != 0 {
		t.Errorf("bob = %s, want 0", got)
	}

	// but nothing decreased. the rows still say money moved and came back,
	// which a balance column would have forgotten entirely.
	var in, out string
	pool.QueryRow(ctx, "select input::text, output::text from accounts_volumes where address='users:bob'").Scan(&in, &out)
	if in != "3000" || out != "3000" {
		t.Errorf("bob volumes = (%s, %s), want (3000, 3000)", in, out)
	}
	assertConserved(t, ctx, pool)
}

func TestRevertIsTaggedAndOriginalIsUntouched(t *testing.T) {
	ctx, s, _ := testStore(t)
	fund(t, ctx, s, "users:alice", 10000)
	payment := mustCommit(t, ctx, s, "users:alice", "users:bob", 3000)

	result, err := s.RevertTransaction(ctx, payment.ID, RevertOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if got := result.Reversal.Metadata[ledger.RevertsKey]; got != strconv.FormatInt(payment.ID, 10) {
		t.Errorf("reversal is tagged %q, want the original id", got)
	}

	// the original keeps its postings exactly as they were
	original, err := s.GetTransaction(ctx, payment.ID)
	if err != nil {
		t.Fatal(err)
	}
	if original.Postings[0].Source != "users:alice" || original.Postings[0].Amount.Cmp(n(3000)) != 0 {
		t.Errorf("the original was modified: %+v", original.Postings[0])
	}
	if original.RevertedAt == nil {
		t.Error("revertedAt was not persisted")
	}

	// and the reversal moves the other way
	if result.Reversal.Postings[0].Source != "users:bob" {
		t.Errorf("reversal source = %q, want users:bob", result.Reversal.Postings[0].Source)
	}
}

// a reversal is an ordinary transaction and can fail. if the money has been
// spent it is not there to give back.
func TestRevertFailsWhenTheMoneyIsGone(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "users:alice", 10000)
	payment := mustCommit(t, ctx, s, "users:alice", "users:bob", 3000)
	mustCommit(t, ctx, s, "users:bob", "users:carol", 3000)

	_, err := s.RevertTransaction(ctx, payment.ID, RevertOptions{})
	var insufficient *InsufficientFundsError
	if !errors.As(err, &insufficient) {
		t.Fatalf("err = %v, want InsufficientFundsError", err)
	}

	// and the failed attempt left nothing behind
	original, err := s.GetTransaction(ctx, payment.ID)
	if err != nil {
		t.Fatal(err)
	}
	if original.RevertedAt != nil {
		t.Error("a failed revert marked the original anyway")
	}
	assertConserved(t, ctx, pool)
}

// A reversal can legitimately fail: if the money has since been spent it is
// not there to give back. There used to be a Force option that committed
// anyway, and it is gone.
//
// It had to declare itself to the database, because the overdraw guard lives
// there too and cannot know an operator decided a negative balance was the
// lesser problem. That declaration was a transaction local setting, and any
// role can set one -- including the application role, which made the overdraw
// guard the only one the application could walk past.
//
// The permission mechanism already expresses "this account may go below zero",
// so an operator who needs the reversal grants it, reverts, and revokes. Three
// steps that leave a trail in the permission state, rather than one flag that
// leaves none.
func TestARevertThatWouldOverdrawIsRefusedUntilPermitted(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "users:alice", 10000)
	payment := mustCommit(t, ctx, s, "users:alice", "users:bob", 3000)
	mustCommit(t, ctx, s, "users:bob", "users:carol", 3000)

	// bob has spent it, so there is nothing to give back
	_, err := s.RevertTransaction(ctx, payment.ID, RevertOptions{})
	var insufficient *InsufficientFundsError
	if !errors.As(err, &insufficient) {
		t.Fatalf("err = %v, want InsufficientFundsError", err)
	}
	if insufficient.Account != "users:bob" {
		t.Errorf("refused %s, want users:bob", insufficient.Account)
	}

	// an operator decides the negative balance is the lesser problem, and says
	// so in a statement that is visible afterwards
	if err := s.SetAllowNegative(ctx, "users:bob", "USD/2", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RevertTransaction(ctx, payment.ID, RevertOptions{}); err != nil {
		t.Fatalf("revert after permitting: %v", err)
	}

	if got := balance(t, ctx, pool, "users:bob", "USD/2"); got.Cmp(n(-3000)) != 0 {
		t.Errorf("bob = %s, want -3000", got)
	}

	// and closing it again, which leaves the account negative and unpermitted
	// for the detector to surface
	if err := s.SetAllowNegative(ctx, "users:bob", "USD/2", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyBalancePermissions(ctx); err == nil {
		t.Error("the detector did not report the negative unpermitted account")
	}

	// value is conserved throughout: permitting an overdraw relaxes the no
	// negatives rule, never the conservation one
	assertConserved(t, ctx, pool)
}
func TestRevertTwiceIsRejected(t *testing.T) {
	ctx, s, _ := testStore(t)
	fund(t, ctx, s, "users:alice", 10000)
	payment := mustCommit(t, ctx, s, "users:alice", "users:bob", 3000)

	if _, err := s.RevertTransaction(ctx, payment.ID, RevertOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err := s.RevertTransaction(ctx, payment.ID, RevertOptions{})
	if !errors.Is(err, ErrAlreadyReverted) {
		t.Fatalf("err = %v, want ErrAlreadyReverted", err)
	}
}

// two reverts landing together must not both succeed and refund twice.
func TestConcurrentRevertsRefundOnce(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "users:alice", 10000)
	payment := mustCommit(t, ctx, s, "users:alice", "users:bob", 3000)

	const callers = 10
	var wg sync.WaitGroup
	errs := make([]error, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = s.RevertTransaction(ctx, payment.ID, RevertOptions{})
		}()
	}
	wg.Wait()

	var ok, rejected int
	for i, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrAlreadyReverted):
			rejected++
		default:
			t.Errorf("caller %d: %v", i, err)
		}
	}
	if ok != 1 {
		t.Errorf("%d reverts succeeded, want exactly 1", ok)
	}
	if rejected != callers-1 {
		t.Errorf("%d rejected, want %d", rejected, callers-1)
	}
	if got := balance(t, ctx, pool, "users:alice", "USD/2"); got.Cmp(n(10000)) != 0 {
		t.Errorf("alice = %s, want 10000: a double refund would show here", got)
	}
	assertConserved(t, ctx, pool)
}

// the order of the postings has to reverse too, or an intermediate account
// dips negative and a reversal that should succeed fails.
func TestRevertOfAChainSucceeds(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "a", 1000)

	chain, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "a", Destination: "b", Asset: "USD/2", Amount: n(1000)},
		{Source: "b", Destination: "c", Asset: "USD/2", Amount: n(1000)},
	}, CommitOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.RevertTransaction(ctx, chain.ID, RevertOptions{}); err != nil {
		t.Fatalf("reverting a chain should succeed, b never has to go negative: %v", err)
	}
	if got := balance(t, ctx, pool, "a", "USD/2"); got.Cmp(n(1000)) != 0 {
		t.Errorf("a = %s, want 1000", got)
	}
	assertConserved(t, ctx, pool)
}

func TestRevertAtEffectiveDate(t *testing.T) {
	ctx, s, _ := testStore(t)
	fund(t, ctx, s, "users:alice", 10000)

	payment, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "users:alice", Destination: "users:bob", Asset: "USD/2", Amount: n(3000)},
	}, CommitOptions{Timestamp: mustTime("2026-03-01T12:00:00Z")})
	if err != nil {
		t.Fatal(err)
	}

	byDefault, err := s.RevertTransaction(ctx, payment.ID, RevertOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !byDefault.Reversal.Timestamp.After(payment.Timestamp) {
		t.Errorf("reversal dated %v, want now rather than the original's date", byDefault.Reversal.Timestamp)
	}

	// and with the flag it carries the original's date
	second := mustCommit(t, ctx, s, "users:alice", "users:bob", 500)
	backdated, err := s.RevertTransaction(ctx, second.ID, RevertOptions{AtEffectiveDate: true})
	if err != nil {
		t.Fatal(err)
	}
	if !backdated.Reversal.Timestamp.Equal(second.Timestamp) {
		t.Errorf("reversal dated %v, want the original's %v",
			backdated.Reversal.Timestamp, second.Timestamp)
	}
}

func TestRevertIsLoggedAndChained(t *testing.T) {
	ctx, s, _ := testStore(t)
	fund(t, ctx, s, "users:alice", 10000)
	payment := mustCommit(t, ctx, s, "users:alice", "users:bob", 3000)

	if _, err := s.RevertTransaction(ctx, payment.ID, RevertOptions{}); err != nil {
		t.Fatal(err)
	}

	want := []ledger.LogType{
		ledger.LogNewTransaction,
		ledger.LogNewTransaction,
		ledger.LogRevertedTransaction,
	}
	got := logTypes(t, s)
	if len(got) != len(want) {
		t.Fatalf("log = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d is %s, want %s", i+1, got[i], want[i])
		}
	}
	if _, err := s.VerifyLog(ctx); err != nil {
		t.Errorf("chain broken: %v", err)
	}
}

func TestRevertMissingTransaction(t *testing.T) {
	ctx, s, _ := testStore(t)
	if _, err := s.RevertTransaction(ctx, 99, RevertOptions{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func mustTime(s string) (t time.Time) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// every timestamp leaves in utc, or the same instant serialises differently
// depending on where the server runs.
func TestRevertedAtIsUTC(t *testing.T) {
	ctx, s, _ := testStore(t)
	fund(t, ctx, s, "users:alice", 10000)
	payment := mustCommit(t, ctx, s, "users:alice", "users:bob", 3000)

	if _, err := s.RevertTransaction(ctx, payment.ID, RevertOptions{}); err != nil {
		t.Fatal(err)
	}

	read, err := s.GetTransaction(ctx, payment.ID)
	if err != nil {
		t.Fatal(err)
	}
	if read.RevertedAt == nil {
		t.Fatal("revertedAt missing")
	}
	if name, offset := read.RevertedAt.Zone(); offset != 0 {
		t.Errorf("revertedAt is in %s (offset %d), want UTC", name, offset)
	}
	// and the same through a list
	page, err := s.ListTransactions(ctx, ListTransactionsQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, tx := range page.Items {
		if tx.RevertedAt == nil {
			continue
		}
		if _, offset := tx.RevertedAt.Zone(); offset != 0 {
			t.Errorf("transaction %d revertedAt is not utc", tx.ID)
		}
	}
}
