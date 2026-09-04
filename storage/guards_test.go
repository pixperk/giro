package storage

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pixperk/giro/ledger"
)

// The guards live in the database, so these tests go around the application
// entirely. Every one of them is a statement an engineer could type into psql,
// or a compromised process could send, and none of them run a line of Go from
// this package.
//
// A guard nothing tests is a comment with a runtime cost.

// refused asserts the statement was rejected by one of our guards, and names
// which. Asserting merely that an error occurred is how a test passes while
// proving nothing: a foreign key, a check constraint or a syntax error all
// produce one, and none of them is the thing under test.
func refused(t *testing.T, ctx context.Context, pool *pgxpool.Pool, want string, sql string, args ...any) {
	t.Helper()
	_, err := pool.Exec(ctx, sql, args...)
	if err == nil {
		t.Fatalf("accepted, want refused: %s", sql)
	}
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		t.Fatalf("err = %v, want a postgres error", err)
	}

	// under the restricted application role the privilege check fires before
	// the trigger ever runs, so the statement is refused a layer earlier. that
	// is defence in depth working rather than a different outcome, and it is
	// why the two mechanisms are both worth having: one says what a row may
	// become, the other says what the role may reach for.
	if os.Getenv("GIRO_TEST_ROLE") != "" {
		if pg.Code != "23001" && pg.Code != "42501" {
			t.Fatalf("sqlstate = %s (%s), want a guard (23001) or a privilege (42501)", pg.Code, pg.Message)
		}
		return
	}

	if pg.Code != "23001" {
		t.Fatalf("sqlstate = %s (%s), want 23001 restrict_violation: the statement was "+
			"refused by something other than our guard", pg.Code, pg.Message)
	}
	if !strings.Contains(pg.Message, want) {
		t.Errorf("message = %q, want it to mention %q", pg.Message, want)
	}
}

// value cannot be created, and this is the one a row level guard cannot see:
// raising one account's input is an increase, on one row, and nothing about
// that row is wrong. only the whole-table check notices.
func TestRawSQLCannotMintMoney(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "users:alice", 10000)

	refused(t, ctx, pool, "drifted by",
		"update accounts_volumes set input = input + 500000 where ledger='main' and address='users:alice'")

	if got := balance(t, ctx, pool, "users:alice", "USD/2"); got.Int64() != 10000 {
		t.Errorf("alice = %s, want 10000", got)
	}
	assertConserved(t, ctx, pool)
}

// and cannot be destroyed either, which is the same check in the other
// direction and the one a deleted row produces.
func TestRawSQLCannotDeleteABalance(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "users:alice", 10000)

	refused(t, ctx, pool, "drifted by",
		"delete from accounts_volumes where ledger='main' and address='users:alice'")
	assertConserved(t, ctx, pool)
}

// the cheapest way to fake a balance is to lower what an account has spent.
// gross flow still looks plausible and the balance rises out of nowhere.
func TestRawSQLCannotLowerAVolume(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "users:alice", 10000)
	if _, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "users:alice", Destination: "users:bob", Asset: "USD/2", Amount: n(3000)},
	}, CommitOptions{}); err != nil {
		t.Fatal(err)
	}

	refused(t, ctx, pool, "volumes only increase",
		"update accounts_volumes set output = output - 1000 where ledger='main' and address='users:alice'")
}

func TestRawSQLCannotOverdraw(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "users:alice", 10000)

	// conservation preserving on purpose, so this isolates the overdraw guard
	// rather than tripping the conservation one: alice spends what she does
	// not have and world receives it, which balances.
	refused(t, ctx, pool, "not permitted a negative balance", `
		update accounts_volumes
		   set output = output + case when address = 'users:alice' then 999999 else 0 end,
		       input  = input  + case when address = 'world'       then 999999 else 0 end
		 where ledger = 'main' and address in ('users:alice', 'world')`)
}

// the log is the source of truth, so it is the only one of these tables that
// is append only without qualification.
func TestRawSQLCannotRewriteTheLog(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "users:alice", 10000)

	refused(t, ctx, pool, "logs is append only",
		`update logs set data = '{"tampered":true}' where ledger='main' and id = 1`)
	refused(t, ctx, pool, "logs is append only",
		"delete from logs where ledger='main' and id = 1")
}

// the one that update and delete guards miss entirely. truncate visits no
// rows, so a row level trigger is never called and the whole table goes
// without an error being raised.
//
// CASCADE deliberately: a plain truncate on a table a foreign key references
// is refused by the foreign key first, so the same assertion would pass with
// no guard installed at all and prove nothing.
func TestRawSQLCannotTruncate(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "users:alice", 10000)

	for _, table := range []string{"logs", "transactions", "moves", "accounts", "accounts_volumes", "ledgers"} {
		t.Run(table, func(t *testing.T) {
			refused(t, ctx, pool, "append only", "truncate "+table+" cascade")
		})
	}

	if got := logCount(t, ctx, pool); got == 0 {
		t.Error("something was truncated after all")
	}
}

// transactions and moves are not append only, and pretending otherwise was
// wrong. what they permit is narrow and named, and everything else is refused
// including columns that do not exist yet.
func TestTransactionsPermitOnlyRevertAndMetadata(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "users:alice", 10000)

	// what a revert and a metadata write do, which must keep working
	if _, err := pool.Exec(ctx,
		"update transactions set reverted_at = now() where ledger='main' and id = 1"); err != nil {
		t.Fatalf("stamping a revert was refused: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`update transactions set metadata = '{"note":"x"}' where ledger='main' and id = 1`); err != nil {
		t.Fatalf("writing metadata was refused: %v", err)
	}

	// what it recorded cannot move
	refused(t, ctx, pool, "may only change",
		`update transactions set postings = '[]' where ledger='main' and id = 1`)
	refused(t, ctx, pool, "may only change",
		"update transactions set timestamp = now() where ledger='main' and id = 1")
	refused(t, ctx, pool, "never deleted",
		"delete from transactions where ledger='main' and id = 1")
}

func TestMovesPermitOnlyEffectiveVolumes(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "users:alice", 10000)

	// what a backdated transaction landing behind this move does
	if _, err := pool.Exec(ctx,
		"update moves set pcev_input = pcev_input + 1 where ledger='main' and seq = 1"); err != nil {
		t.Fatalf("shifting effective volumes was refused: %v", err)
	}

	// the frozen snapshot is frozen, and the movement itself does not move
	refused(t, ctx, pool, "may only change",
		"update moves set pcv_input = pcv_input + 1 where ledger='main' and seq = 1")
	refused(t, ctx, pool, "may only change",
		"update moves set amount = amount * 2 where ledger='main' and seq = 1")
}

// ids come from a counter rather than a sequence so that a gap means a missing
// entry. lowering it hands the next commit an id that already exists, and the
// unique constraint then rejects honest work while the tampering goes
// unrecorded.
func TestLedgerCountersCannotGoBackwards(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "users:alice", 10000)

	refused(t, ctx, pool, "ids would be reused",
		"update ledgers set last_tx_id = 0 where name = 'main'")
	refused(t, ctx, pool, "ids would be reused",
		"update ledgers set last_log_id = 0 where name = 'main'")
}

// the guard is about value moving, not about policy. an operator stopping
// further drawdown on an account that is already negative is doing something
// reasonable and often urgent, and refusing it would block the only way to
// stop the bleeding on the fact that it is bleeding.
func TestRevokingPermissionOnANegativeAccountIsAllowed(t *testing.T) {
	ctx, s, _ := testStore(t)

	if err := s.SetAllowNegative(ctx, "cost:peg_absorption", "USD/2", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "cost:peg_absorption", Destination: "ops:usd", Asset: "USD/2", Amount: n(4000)},
	}, CommitOptions{}); err != nil {
		t.Fatal(err)
	}

	if err := s.SetAllowNegative(ctx, "cost:peg_absorption", "USD/2", false); err != nil {
		t.Fatalf("revoking on a negative account was refused: %v", err)
	}

	// and the state it leaves behind is exactly what the detector is for
	if _, err := s.VerifyBalancePermissions(ctx); err == nil {
		t.Error("the detector did not report a negative unpermitted account")
	}
}

// The upper bound holds against raw SQL too, like every other guard.
func TestRawSQLCannotCreditABoundedAccount(t *testing.T) {
	ctx, s, pool := testStore(t)
	if err := s.SetAllowPositive(ctx, "cost:peg", "USD/2", false); err != nil {
		t.Fatal(err)
	}
	fund(t, ctx, s, "users:alice", 10_000)

	// conservation preserving, so this isolates the bound rather than tripping
	// the conservation check
	refused(t, ctx, pool, "not permitted a positive balance", `
		update accounts_volumes
		   set input  = input  + case when address = 'cost:peg'    then 500 else 0 end,
		       output = output + case when address = 'users:alice' then 500 else 0 end
		 where ledger = 'main' and address in ('cost:peg', 'users:alice')`)
}

// Reconciliation evidence is append only, and this is the guard that matters
// most in that layer. Deleting a match moves no money: the postings are
// untouched, the chain still verifies, conservation still holds, and the book
// now reconciles because the rows that did not reconcile are gone. A clean
// report obtained by deleting the mess is the failure reconciliation exists to
// prevent.
func TestReconciliationEvidenceIsAppendOnly(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "users:alice", 10_000)
	stageOneRecord(t, ctx, pool)

	if _, err := pool.Exec(ctx, `
		insert into recon_matches (ledger, source, record_id, transaction_id, variance, rule)
		values ('main', 'kraken', 'L1', 1, 0, 'exact_ref')`); err != nil {
		t.Fatal(err)
	}

	refused(t, ctx, pool, "append only",
		"delete from recon_matches where ledger = 'main'")
	refused(t, ctx, pool, "append only",
		"update recon_matches set variance = 0 where ledger = 'main'")
	refused(t, ctx, pool, "append only", "truncate recon_matches cascade")
}

// A staged line may be marked matched and nothing else. Revising the amount or
// the reference of a line that did not match is how an unreconciled book is
// made to look reconciled.
func TestAStagedRecordCannotBeRevised(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "users:alice", 10_000)
	stageOneRecord(t, ctx, pool)

	// what matching does, which must keep working
	if _, err := pool.Exec(ctx, `
		update recon_records set matched_count = 1, matched_at = now()
		 where ledger = 'main' and source = 'kraken' and record_id = 'L1'`); err != nil {
		t.Fatalf("marking a record matched was refused: %v", err)
	}

	// what the source said, which cannot move
	refused(t, ctx, pool, "may only change",
		"update recon_records set amount = 1 where ledger = 'main'")
	refused(t, ctx, pool, "may only change",
		"update recon_records set reference = 'other' where ledger = 'main'")
	refused(t, ctx, pool, "may only change",
		"update recon_records set direction = 'out' where ledger = 'main'")
	refused(t, ctx, pool, "never deleted",
		"delete from recon_records where ledger = 'main'")
	refused(t, ctx, pool, "append only", "truncate recon_records cascade")
}

// A line naming an asset the ledger does not handle is refused at ingest
// rather than sitting unmatched for ever. It is a source misconfiguration, and
// an unmatched queue is the wrong place to discover one.
func TestAStagedRecordMustNameARegisteredAsset(t *testing.T) {
	ctx, _, pool := testStore(t)
	if _, err := pool.Exec(ctx,
		"insert into recon_sources (ledger, id, name) values ('main','kraken','Kraken')"); err != nil {
		t.Fatal(err)
	}

	_, err := pool.Exec(ctx, `
		insert into recon_records (ledger, source, record_id, reference, asset, amount, direction)
		values ('main','kraken','L9','W-9','GBP/2',100,'in')`)
	if err == nil {
		t.Fatal("a line in an unregistered asset was staged")
	}
	if !strings.Contains(err.Error(), "asset") {
		t.Errorf("err = %v, want the asset foreign key", err)
	}
}

func stageOneRecord(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		"insert into recon_sources (ledger, id, name) values ('main','kraken','Kraken')"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into recon_records (ledger, source, record_id, reference, asset, amount, direction)
		values ('main','kraken','L1','W-1','USD/2',10000,'in')`); err != nil {
		t.Fatal(err)
	}
}
