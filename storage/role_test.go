package storage

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pixperk/giro/ledger"
)

// The guards in the previous migration are advisory while the application owns
// the tables it is guarded by, because an owner can disable its own triggers
// with one statement. These tests run the real engine as giro_app instead.
//
// SET ROLE rather than a separate login, so the tests need no credential and
// no pg_hba change. It exercises the grants, which is what is under test. It
// does not prove isolation against something already inside the session, since
// RESET ROLE undoes it -- a real deployment connects as a role that has no
// membership in the owner to go back to.

// a store whose every connection has dropped to the restricted role before it
// is handed out.
func restrictedStore(t *testing.T) (context.Context, *Store, *pgxpool.Pool) {
	t.Helper()
	ctx, owner, pool := testStore(t)

	_ = owner
	cfg := pool.Config().Copy()
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "set role giro_app")
		return err
	}
	restricted, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restricted.Close)

	return ctx, New(restricted, "main"), restricted
}

// the grants have to be sufficient, or the ledger simply does not work. this
// is the half that is easy to get wrong in the tightening direction: a missing
// grant on a sequence, or a column left off an update list, surfaces as a
// permission error in the middle of a money path.
func TestTheLedgerWorksUnderTheRestrictedRole(t *testing.T) {
	ctx, s, pool := restrictedStore(t)

	// testStore already made "main", so creating one exercises the insert on
	// ledgers rather than colliding with it
	if _, err := New(pool, "second").CreateLedger(ctx); err != nil {
		t.Fatalf("create ledger: %v", err)
	}

	if _, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "world", Destination: "users:alice", Asset: "USD/2", Amount: n(10000)},
	}, CommitOptions{Reference: "deposit"}); err != nil {
		t.Fatalf("commit: %v", err)
	}

	payment, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "users:alice", Destination: "users:bob", Asset: "USD/2", Amount: n(3000)},
	}, CommitOptions{})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	// a backdated transaction, which is the path that rewrites effective
	// volumes on existing moves
	if _, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "world", Destination: "users:alice", Asset: "USD/2", Amount: n(500)},
	}, CommitOptions{Timestamp: day(1)}); err != nil {
		t.Fatalf("backdated commit: %v", err)
	}

	// a revert, which stamps the original row
	if _, err := s.RevertTransaction(ctx, payment.ID, RevertOptions{}); err != nil {
		t.Fatalf("revert: %v", err)
	}

	// metadata on both targets
	if _, err := s.SetTransactionMetadata(ctx, payment.ID, ledger.Metadata{"note": "x"}); err != nil {
		t.Fatalf("transaction metadata: %v", err)
	}
	if _, err := s.SetAccountMetadata(ctx, "users:alice", ledger.Metadata{"kyc": "done"}); err != nil {
		t.Fatalf("account metadata: %v", err)
	}

	// the permission flag
	if err := s.SetAllowNegative(ctx, "cost:peg", "USD/2", true); err != nil {
		t.Fatalf("set allow negative: %v", err)
	}

	// and every verifier, which reads across all six tables
	for name, verify := range map[string]func(context.Context) (int, error){
		"log":         s.VerifyLog,
		"projection":  s.VerifyProjection,
		"effective":   s.VerifyEffectiveVolumes,
		"permissions": s.VerifyBalancePermissions,
	} {
		if _, err := verify(ctx); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	assertConserved(t, ctx, pool)
}

// deniedTo asserts the statement was refused for want of privilege rather than
// by a trigger. the distinction is the whole point of this migration: a
// trigger can be switched off by whoever owns the table, and a missing grant
// cannot be granted by the role that lacks it.
func deniedTo(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string) {
	t.Helper()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "set role giro_app"); err != nil {
		t.Fatal(err)
	}

	_, err = conn.Exec(ctx, sql)
	if err == nil {
		t.Fatalf("permitted, want denied: %s", sql)
	}

	// 42501 insufficient_privilege, rather than the message text. postgres
	// words it several ways -- "permission denied for table", "must be owner
	// of table" -- and matching prose would pass or fail on a wording change
	// rather than on the fact under test.
	//
	// and the code is what distinguishes this from a guard: a trigger raises
	// 23001, which the table owner can switch off. 42501 cannot be granted by
	// the role that lacks it.
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "42501" {
		t.Errorf("err = %v\n  want sqlstate 42501 insufficient_privilege. a guard can be "+
			"disabled by the table owner; a missing grant cannot be given by the role that lacks it.\n  statement: %s",
			err, sql)
	}
}

// the one that matters most. if this passes, every trigger in the previous
// migration is decoration.
func TestTheApplicationRoleCannotDisableTheGuards(t *testing.T) {
	ctx, _, pool := testStore(t)

	for _, sql := range []string{
		"alter table logs disable trigger user",
		"alter table logs disable trigger logs_append_only",
		"alter table accounts_volumes disable trigger user",
		"drop trigger logs_append_only on logs",
		"alter table logs owner to current_user",
	} {
		t.Run(sql, func(t *testing.T) { deniedTo(t, ctx, pool, sql) })
	}
}

// removal is not a thing the application can express at all, at any level.
func TestTheApplicationRoleCannotRemoveHistory(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "users:alice", 10000)

	for _, sql := range []string{
		"delete from logs where ledger = 'main'",
		"delete from transactions where ledger = 'main'",
		"delete from moves where ledger = 'main'",
		"delete from accounts_volumes where ledger = 'main'",
		"truncate logs cascade",
		"truncate transactions cascade",
		"drop table logs",
	} {
		t.Run(sql, func(t *testing.T) { deniedTo(t, ctx, pool, sql) })
	}

	if got := logCount(t, ctx, pool); got == 0 {
		t.Error("history was removed after all")
	}
}

// the column scoping is the load bearing half of the grants. a table level
// update grant would re-open the money columns and leave the trigger as the
// only thing standing, which is exactly the hole an external audit found in
// the system this borrows from.
func TestTheApplicationRoleCannotReachUnlistedColumns(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "users:alice", 10000)

	for _, sql := range []string{
		// what a transaction recorded
		"update transactions set postings = '[]' where ledger = 'main'",
		"update transactions set timestamp = now() where ledger = 'main'",
		"update transactions set reference = 'other' where ledger = 'main'",
		// the frozen snapshot, and the movement itself
		"update moves set pcv_input = 0 where ledger = 'main'",
		"update moves set amount = 1 where ledger = 'main'",
		// the log, which takes no update grant at all
		"update logs set hash = '\\x00' where ledger = 'main'",
		// an account's identity
		"update accounts set address = 'someone:else' where ledger = 'main'",
		// and the schema version, which would let a process claim it migrated
		"update schema_migrations set checksum = 'x'",
		"delete from schema_migrations",
	} {
		t.Run(sql, func(t *testing.T) { deniedTo(t, ctx, pool, sql) })
	}
}

// the columns that are granted are still guarded, so the two mechanisms
// overlap rather than each covering half. a grant says what may be reached
// for; a trigger says what it may become.
func TestGrantedColumnsAreStillGuarded(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "users:alice", 10000)

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "set role giro_app"); err != nil {
		t.Fatal(err)
	}

	// input is granted, and raising it alone still breaks conservation
	_, err = conn.Exec(ctx,
		"update accounts_volumes set input = input + 500000 where ledger='main' and address='users:alice'")
	if err == nil {
		t.Fatal("a granted column let value be created")
	}
	if !strings.Contains(err.Error(), "drifted by") {
		t.Errorf("err = %v, want the conservation guard", err)
	}
}

// The overdraw guard used to honour a transaction local flag, so that a forced
// revert could commit anyway. Any role can set a custom setting, so the
// application role could set it too and then overdraw anything. That made the
// overdraw guard the only one of the seven the application could walk past.
//
// This is the exact statement sequence that worked before the flag was
// removed.
func TestTheApplicationRoleCannotOverdrawByDeclaringAnythingAtAll(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "users:alice", 10000)

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "set role giro_app"); err != nil {
		t.Fatal(err)
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)

	// still permitted: anyone may set a custom setting, which is the whole
	// reason honouring one was a mistake
	if _, err := tx.Exec(ctx, "set local giro.force_overdraw = 'on'"); err != nil {
		t.Fatalf("setting a custom parameter: %v", err)
	}

	// conservation preserving, so only the overdraw guard stands in the way
	_, err = tx.Exec(ctx, `
		update accounts_volumes
		   set output = output + case when address = 'users:alice' then 99999 else 0 end,
		       input  = input  + case when address = 'world'       then 99999 else 0 end
		 where ledger = 'main' and address in ('users:alice', 'world')`)
	if err == nil {
		t.Fatal("the application role overdrew an account by declaring itself forced")
	}

	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "23001" {
		t.Fatalf("err = %v, want the overdraw guard (23001)", err)
	}
	if !strings.Contains(pg.Message, "not permitted a negative balance") {
		t.Errorf("message = %q, want the overdraw guard", pg.Message)
	}
}
