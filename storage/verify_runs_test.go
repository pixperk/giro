package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pixperk/giro/ledger"
)

func TestVerifyRunsEveryCheckOnAHealthyLedger(t *testing.T) {
	ctx, s, _ := testStore(t)
	fund(t, ctx, s, "users:alice", 10000)

	// a closed account too. like the conversions check, this one examines
	// nothing on a ledger where nobody has closed anything, and a check with
	// nothing to examine is not evidence of anything.
	if err := s.CloseAccount(ctx, "client:departed"); err != nil {
		t.Fatal(err)
	}

	results, err := s.Verify(ctx, VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"conservation", "log", "projection", "effective_volumes", "balance_permissions", "closed_accounts"}
	// exactly the engine's own checks. anything a layer above contributes
	// arrives through Extra rather than being built in, which is what keeps
	// the engine from having to know what those checks mean.
	if len(results) != len(want) {
		t.Fatalf("%d checks, want %d", len(results), len(want))
	}
	for i, name := range want {
		// fixed order, so two runs are comparable line by line
		if results[i].Name != name {
			t.Errorf("check %d is %s, want %s", i, results[i].Name, name)
		}
		if !results[i].OK {
			t.Errorf("%s failed on a healthy ledger: %s", name, results[i].Detail)
		}
		// the column that separates "looked and found nothing" from "did not
		// look". a check reporting zero examined is not a passing check.
		if results[i].Checked == 0 {
			t.Errorf("%s examined nothing and reported success", name)
		}
	}
}

// One broken thing must not hide the rest. A run that reports the first
// problem and stops is worse than useless during an incident.
func TestVerifyReportsEveryFailureNotJustTheFirst(t *testing.T) {
	ctx, s, pool := testStore(t)
	withoutGuards(t, ctx, pool, "accounts_volumes")
	fund(t, ctx, s, "users:alice", 10000)

	if _, err := pool.Exec(ctx,
		"update accounts_volumes set input = input + 500 where ledger='main' and address='world'"); err != nil {
		t.Fatal(err)
	}

	results, err := s.Verify(ctx, VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}

	failed := map[string]string{}
	for _, r := range results {
		if !r.OK {
			failed[r.Name] = r.Detail
		}
	}
	// conservation sees the drift; projection sees that the row disagrees with
	// the log. both are true and an operator wants both.
	for _, name := range []string{"conservation", "projection"} {
		if _, ok := failed[name]; !ok {
			t.Errorf("%s did not report the tampering", name)
		}
	}
	if len(failed) < 2 {
		t.Errorf("stopped after the first failure: %v", failed)
	}
}

// The half of alerting people leave out: a check that stopped running looks
// exactly like a book with nothing wrong.
func TestRecordingMakesSilenceDetectable(t *testing.T) {
	ctx, s, _ := testStore(t)
	fund(t, ctx, s, "users:alice", 10000)

	// nothing has run yet, and that is distinguishable from a clean run
	before, err := s.LastVerified(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 0 {
		t.Errorf("a ledger nobody checked reports runs: %v", before)
	}

	if _, err := s.Verify(ctx, VerifyOptions{Record: true}); err != nil {
		t.Fatal(err)
	}

	after, err := s.LastVerified(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"conservation", "log", "projection", "effective_volumes", "balance_permissions", "closed_accounts"} {
		at, ok := after[name]
		if !ok {
			t.Errorf("%s did not record that it ran", name)
			continue
		}
		if time.Since(at) > time.Minute {
			t.Errorf("%s recorded %s ago", name, time.Since(at))
		}
	}
}

// Recording is opt in, because a read replica has to be able to check without
// writing.
func TestVerifyRecordsNothingByDefault(t *testing.T) {
	ctx, s, _ := testStore(t)
	fund(t, ctx, s, "users:alice", 10000)

	if _, err := s.Verify(ctx, VerifyOptions{}); err != nil {
		t.Fatal(err)
	}
	seen, err := s.LastVerified(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 0 {
		t.Errorf("recorded without being asked: %v", seen)
	}
}

// The stale check is opt in too: with no window it would report every balance
// on the ledger, which answers nothing.
func TestStaleCheckIsSkippedWithoutAWindow(t *testing.T) {
	ctx, s, _ := testStore(t)
	fund(t, ctx, s, "users:alice", 10000)

	off, err := s.Verify(ctx, VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range off {
		if r.Name == "stale_balances" {
			t.Fatal("the stale check ran with no window")
		}
	}

	on, err := s.Verify(ctx, VerifyOptions{StalePrefix: "users:", StaleAfter: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	last := on[len(on)-1]
	if last.Name != "stale_balances" {
		t.Fatalf("last check is %s, want stale_balances", last.Name)
	}
	if last.OK {
		t.Error("alice has been holding money since before the window and is not reported")
	}
}

// A finding is recorded, not just printed. An incident review asks what the
// checks said at the time, and "it printed something to a log that rotated" is
// not an answer.
func TestAFindingIsRecordedWithItsDetail(t *testing.T) {
	ctx, s, pool := testStore(t)
	withoutGuards(t, ctx, pool, "accounts_volumes")
	fund(t, ctx, s, "users:alice", 10000)
	if _, err := pool.Exec(ctx,
		"update accounts_volumes set input = input + 500 where ledger='main' and address='world'"); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Verify(ctx, VerifyOptions{Record: true}); err != nil {
		t.Fatal(err)
	}

	var ok bool
	var detail string
	if err := pool.QueryRow(ctx, `
		select ok, detail from verification_runs
		 where ledger='main' and check_name='conservation'
		 order by ran_at desc limit 1`).Scan(&ok, &detail); err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("a broken ledger was recorded as sound")
	}
	if detail == "" {
		t.Error("the finding was recorded without saying what it was")
	}
}

// Conservation is the master invariant and had no method of its own until the
// command needed one.
func TestVerifyConservationCountsWhatItExamined(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "users:alice", 10000)
	if _, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "world", Destination: "users:bob", Asset: "EUR/2", Amount: n(500)},
	}, CommitOptions{}); err != nil {
		t.Fatal(err)
	}

	checked, err := s.VerifyConservation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// world and alice in USD, world and bob in EUR
	if checked != 4 {
		t.Errorf("checked %d rows, want 4 across both assets", checked)
	}
	assertConserved(t, ctx, pool)
}

// Ledgers is a package function rather than a method, because a Store is
// scoped to one ledger and that scoping is the tenant boundary.
func TestLedgersListsAllOfThem(t *testing.T) {
	ctx, _, _, pool := twoLedgers(t)

	names, err := Ledgers(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "main" || names[1] != "theirs" {
		t.Errorf("names = %v, want [main theirs] in order", names)
	}
}

// A check belonging to a layer above the engine runs alongside the engine's
// own and is recorded the same way, because an operator wants one answer to
// "is the book sound" rather than one per package.
func TestContributedChecksRunAndAreRecorded(t *testing.T) {
	ctx, s, _ := testStore(t)
	fund(t, ctx, s, "users:alice", 10000)

	results, err := s.Verify(ctx, VerifyOptions{
		Record: true,
		Extra: []NamedCheck{
			{Name: "contributed_ok", Run: func(context.Context) (int, error) { return 7, nil }},
			{Name: "contributed_finding", Run: func(context.Context) (int, error) {
				return 3, errors.New("something a layer above found")
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	byName := map[string]CheckResult{}
	for _, r := range results {
		byName[r.Name] = r
	}
	if got := byName["contributed_ok"]; !got.OK || got.Checked != 7 {
		t.Errorf("contributed_ok = %+v, want ok with 7 checked", got)
	}
	if got := byName["contributed_finding"]; got.OK || got.Detail == "" {
		t.Errorf("contributed_finding = %+v, want a finding with detail", got)
	}

	seen, err := s.LastVerified(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"contributed_ok", "contributed_finding"} {
		if _, ok := seen[name]; !ok {
			t.Errorf("%s was not recorded alongside the engine's own checks", name)
		}
	}
}
