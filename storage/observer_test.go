package storage

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/pixperk/giro/ledger"
)

// A recording Observer. The mutex is not decoration: an implementation is
// called from whatever goroutine is committing, and a test that shares one
// across concurrent commits would otherwise be reporting a race in itself.
type recorder struct {
	mu         sync.Mutex
	commits    []Commit
	refusals   []Refusal
	contention []Contention
}

func (r *recorder) Committed(_ context.Context, e Commit) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commits = append(r.commits, e)
}

func (r *recorder) Refused(_ context.Context, e Refusal) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refusals = append(r.refusals, e)
}

func (r *recorder) Contended(_ context.Context, e Contention) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.contention = append(r.contention, e)
}

func observed(t *testing.T) (context.Context, *Store, *recorder) {
	t.Helper()
	ctx, s, _ := testStore(t)
	r := &recorder{}
	s.Observe(r)
	return ctx, s, r
}

func TestACommitIsReportedWithWhatItTouched(t *testing.T) {
	ctx, s, r := observed(t)

	if _, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "world", Destination: "users:alice", Asset: "USD/2", Amount: n(50000)},
	}, CommitOptions{}); err != nil {
		t.Fatal(err)
	}

	if len(r.commits) != 1 {
		t.Fatalf("%d commit events, want 1", len(r.commits))
	}
	e := r.commits[0]
	if e.Ledger != "main" {
		t.Errorf("ledger = %q", e.Ledger)
	}
	if e.Postings != 1 || e.Accounts != 2 {
		t.Errorf("postings = %d, accounts = %d, want 1 and 2", e.Postings, e.Accounts)
	}
	if e.Attempts != 1 {
		t.Errorf("attempts = %d, want 1 for an uncontended commit", e.Attempts)
	}
	if e.Took <= 0 {
		t.Error("took = 0, so nothing was actually timed")
	}
	if len(e.Assets) != 1 || e.Assets[0] != "USD/2" {
		t.Errorf("assets = %v, want [USD/2]", e.Assets)
	}
}

// A conversion is the case that makes deduplication worth having: two assets
// and four accounts in one transaction.
func TestAssetsAndAccountsAreDeduplicated(t *testing.T) {
	ctx, s, r := observed(t)
	fund(t, ctx, s, "users:alice", 50000)
	r.commits = nil

	if _, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "users:alice", Destination: "users:bob", Asset: "USD/2", Amount: n(100)},
		{Source: "users:alice", Destination: "fees:platform", Asset: "USD/2", Amount: n(50)},
	}, CommitOptions{}); err != nil {
		t.Fatal(err)
	}

	e := r.commits[0]
	if len(e.Assets) != 1 {
		t.Errorf("assets = %v, want one entry for two postings in the same asset", e.Assets)
	}
	if e.Postings != 2 {
		t.Errorf("postings = %d, want 2", e.Postings)
	}
	if e.Accounts != 3 {
		t.Errorf("accounts = %d, want 3: alice appears in both postings", e.Accounts)
	}
}

// The distinction the whole design rests on. A refusal must not arrive as a
// commit, and it must not arrive as nothing.
func TestARefusalIsItsOwnSignalAndNotAnError(t *testing.T) {
	ctx, s, r := observed(t)

	_, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "users:bob", Destination: "users:carol", Asset: "USD/2", Amount: n(9999)},
	}, CommitOptions{})
	if err == nil {
		t.Fatal("expected a refusal")
	}

	if len(r.commits) != 0 {
		t.Errorf("a refused transaction was reported as committed: %+v", r.commits)
	}
	if len(r.refusals) != 1 {
		t.Fatalf("%d refusal events, want 1", len(r.refusals))
	}
	e := r.refusals[0]
	if e.Reason != CauseInsufficientFunds {
		t.Errorf("reason = %q, want %q", e.Reason, CauseInsufficientFunds)
	}
	if e.Asset != "USD/2" {
		t.Errorf("asset = %q, want the asset it was short of", e.Asset)
	}
	if e.Account != "users:bob" {
		t.Errorf("account = %q, want the account that was short", e.Account)
	}
}

// Each cause is a metric label, so each has to be produced by the path that
// claims it. A reason that never fires is a dashboard panel that stays empty
// during the incident it was built for.
func TestEachRefusalCauseIsReachable(t *testing.T) {
	for _, tc := range []struct {
		name  string
		want  RefusalCause
		setup func(t *testing.T, ctx context.Context, s *Store)
		post  ledger.Postings
	}{
		{
			name: "insufficient funds", want: CauseInsufficientFunds,
			post: ledger.Postings{{Source: "users:bob", Destination: "users:carol", Asset: "USD/2", Amount: n(1)}},
		},
		{
			name: "unknown asset", want: CauseUnknownAsset,
			post: ledger.Postings{{Source: "world", Destination: "users:alice", Asset: "XXX/2", Amount: n(1)}},
		},
		{
			name: "account closed", want: CauseAccountClosed,
			setup: func(t *testing.T, ctx context.Context, s *Store) {
				if err := s.CloseAccount(ctx, "users:gone"); err != nil {
					t.Fatal(err)
				}
			},
			post: ledger.Postings{{Source: "world", Destination: "users:gone", Asset: "USD/2", Amount: n(1)}},
		},
		{
			name: "unexpected credit", want: CauseUnexpectedCredit,
			setup: func(t *testing.T, ctx context.Context, s *Store) {
				if err := s.SetAllowPositive(ctx, "cost:peg", "USD/2", false); err != nil {
					t.Fatal(err)
				}
			},
			post: ledger.Postings{{Source: "world", Destination: "cost:peg", Asset: "USD/2", Amount: n(1)}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, s, r := observed(t)
			if tc.setup != nil {
				tc.setup(t, ctx, s)
			}
			r.refusals = nil

			if _, err := s.CommitTransaction(ctx, tc.post, CommitOptions{}); err == nil {
				t.Fatal("expected a refusal")
			}
			if len(r.refusals) != 1 {
				t.Fatalf("%d refusal events, want 1", len(r.refusals))
			}
			if got := r.refusals[0].Reason; got != tc.want {
				t.Errorf("reason = %q, want %q", got, tc.want)
			}
		})
	}
}

// An infrastructure failure is not the ledger declining anything, and putting
// it in the refusal series would hide a real outage behind a customer being
// short of money.
func TestSomethingThatIsNotARefusalIsNotCountedAsOne(t *testing.T) {
	for _, err := range []error{
		context.Canceled,
		context.DeadlineExceeded,
		errString("connection refused"),
	} {
		if cause, refused := CauseOf(err); refused {
			t.Errorf("CauseOf(%v) = %q, true; want it not to be a refusal", err, cause)
		}
	}
	if _, refused := CauseOf(nil); refused {
		t.Error("CauseOf(nil) reported a refusal")
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// Every commit waits on the locking statement, whether or not anything else
// wanted the same rows, so this event fires on the happy path too. The
// distribution is the signal; a single value means nothing.
func TestLockingIsTimedOnEveryCommit(t *testing.T) {
	ctx, s, r := observed(t)

	if _, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "world", Destination: "users:alice", Asset: "USD/2", Amount: n(100)},
	}, CommitOptions{}); err != nil {
		t.Fatal(err)
	}

	var waits []Contention
	for _, c := range r.contention {
		if !c.Restarted {
			waits = append(waits, c)
		}
	}
	if len(waits) != 1 {
		t.Fatalf("%d lock wait events, want 1", len(waits))
	}
	if waits[0].Waited <= 0 {
		t.Error("waited = 0, so the lock was not actually timed")
	}

	// world is the hot row by construction, and naming it is the point
	var sawWorld bool
	for _, a := range waits[0].Accounts {
		if a == ledger.WorldAccount {
			sawWorld = true
		}
	}
	if !sawWorld {
		t.Errorf("accounts = %v, want world named so the hot row can be found", waits[0].Accounts)
	}
}

// The guarantee that makes this safe to leave in the engine.
func TestAnUnobservedStoreCostsNothing(t *testing.T) {
	ctx, s, _ := testStore(t)
	if s.observing() {
		t.Fatal("a fresh store is already observed")
	}
	// the paths that would build an event must not panic or allocate one
	if _, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "world", Destination: "users:alice", Asset: "USD/2", Amount: n(100)},
	}, CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "users:nobody", Destination: "users:alice", Asset: "USD/2", Amount: n(100)},
	}, CommitOptions{}); err == nil {
		t.Fatal("expected a refusal")
	}
}

// Cardinality is the constraint this whole design is shaped around, so the
// closed set has to stay closed and stay small.
func TestTheRefusalCausesAreAClosedSet(t *testing.T) {
	seen := map[RefusalCause]bool{}
	for _, c := range RefusalCauses {
		if seen[c] {
			t.Errorf("%q listed twice", c)
		}
		seen[c] = true
		if strings.ContainsAny(string(c), " :/") {
			t.Errorf("%q looks like an address or an asset, not a reason", c)
		}
	}
	// a label with more values than this is a design change, not a tweak
	if len(RefusalCauses) > 12 {
		t.Errorf("%d refusal causes: this is a metric label and it is growing", len(RefusalCauses))
	}
}
