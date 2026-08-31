package storage

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	giro "github.com/pixperk/giro"
	"github.com/pixperk/giro/internal/ledger"
	"github.com/pixperk/giro/internal/migrate"
	"io/fs"
)

func n(i int64) *big.Int { return big.NewInt(i) }

func testURL() string {
	if u := os.Getenv("GIRO_TEST_DATABASE_URL"); u != "" {
		return u
	}
	return "postgres://" + os.Getenv("USER") + "@localhost:5432/giro_test"
}

// every test gets its own schema, migrated from scratch, so they cannot see
// each other's rows.
func testStore(t *testing.T) (context.Context, *Store, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()

	schema := fmt.Sprintf("t_%d", os.Getpid()*1000+len(t.Name()))
	schema = fmt.Sprintf("%s_%d", schema, testCounter())

	admin, err := pgxpool.New(ctx, testURL())
	if err != nil {
		t.Skipf("no test database: %v", err)
	}
	if _, err := admin.Exec(ctx, "create schema "+schema); err != nil {
		t.Skipf("no test database: %v", err)
	}
	admin.Close()

	cfg, err := pgxpool.ParseConfig(testURL())
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.MaxConns = 20

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sub, _ := fs.Sub(giro.MigrationsFS, giro.MigrationsDir)
	if _, err := migrate.Run(ctx, conn.Conn(), sub); err != nil {
		t.Fatal(err)
	}
	conn.Release()

	if _, err := pool.Exec(ctx, "insert into ledgers (name) values ('main')"); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		pool.Close()
		cleanup, err := pgxpool.New(ctx, testURL())
		if err == nil {
			cleanup.Exec(ctx, "drop schema "+schema+" cascade")
			cleanup.Close()
		}
	})

	return ctx, New(pool, "main"), pool
}

var counterMu sync.Mutex
var counter int

func testCounter() int {
	counterMu.Lock()
	defer counterMu.Unlock()
	counter++
	return counter
}

func balance(t *testing.T, ctx context.Context, pool *pgxpool.Pool, address, asset string) *big.Int {
	t.Helper()
	var s string
	err := pool.QueryRow(ctx,
		"select (input - output)::text from accounts_volumes where ledger='main' and address=$1 and asset=$2",
		address, asset).Scan(&s)
	if err != nil {
		t.Fatalf("balance(%s): %v", address, err)
	}
	v, _ := new(big.Int).SetString(s, 10)
	return v
}

// no asset may create or destroy value, ever.
func assertConserved(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	rows, err := pool.Query(ctx,
		"select asset, (sum(input) - sum(output))::text from accounts_volumes where ledger='main' group by asset")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var asset, drift string
		if err := rows.Scan(&asset, &drift); err != nil {
			t.Fatal(err)
		}
		if drift != "0" {
			t.Errorf("asset %s drifted by %s", asset, drift)
		}
	}
}

func fund(t *testing.T, ctx context.Context, s *Store, account string, amount int64) {
	t.Helper()
	_, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "world", Destination: account, Asset: "USD/2", Amount: n(amount)},
	}, CommitOptions{})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCommitFundsAnAccount(t *testing.T) {
	ctx, s, pool := testStore(t)

	tx, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "world", Destination: "users:alice", Asset: "USD/2", Amount: n(10000)},
	}, CommitOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if tx.ID != 1 {
		t.Errorf("id = %d, want 1", tx.ID)
	}
	if tx.InsertedAt.IsZero() {
		t.Error("inserted_at was not returned")
	}
	if got := balance(t, ctx, pool, "users:alice", "USD/2"); got.Cmp(n(10000)) != 0 {
		t.Errorf("alice = %s, want 10000", got)
	}
	if got := balance(t, ctx, pool, "world", "USD/2"); got.Cmp(n(-10000)) != 0 {
		t.Errorf("world = %s, want -10000", got)
	}
	assertConserved(t, ctx, pool)
}

func TestIDsAreGaplessEvenAfterFailures(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "users:alice", 100)

	// this must fail and must not burn an id
	_, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "users:alice", Destination: "users:bob", Asset: "USD/2", Amount: n(500)},
	}, CommitOptions{})
	var insufficient *InsufficientFundsError
	if !errors.As(err, &insufficient) {
		t.Fatalf("err = %v, want InsufficientFundsError", err)
	}

	tx, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "users:alice", Destination: "users:bob", Asset: "USD/2", Amount: n(40)},
	}, CommitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if tx.ID != 2 {
		t.Errorf("id = %d, want 2: a rolled back transaction must not consume an id", tx.ID)
	}
	assertConserved(t, ctx, pool)
}

func TestInsufficientFundsNamesTheAccount(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "users:alice", 100)

	_, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "users:alice", Destination: "users:bob", Asset: "USD/2", Amount: n(150)},
	}, CommitOptions{})

	var e *InsufficientFundsError
	if !errors.As(err, &e) {
		t.Fatalf("err = %v, want InsufficientFundsError", err)
	}
	if e.Account != "users:alice" || e.Available.Cmp(n(100)) != 0 {
		t.Errorf("got %+v, want alice with 100 available", e)
	}
	if got := balance(t, ctx, pool, "users:alice", "USD/2"); got.Cmp(n(100)) != 0 {
		t.Errorf("alice = %s, a rejected transaction must change nothing", got)
	}
}

// an account may pass through zero inside a transaction. the check is on the
// final state.
func TestMoneyFlowsThroughAnAccount(t *testing.T) {
	ctx, s, pool := testStore(t)

	_, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "world", Destination: "treasury", Asset: "USD/2", Amount: n(10000)},
		{Source: "treasury", Destination: "users:alice", Asset: "USD/2", Amount: n(6000)},
		{Source: "treasury", Destination: "users:bob", Asset: "USD/2", Amount: n(4000)},
	}, CommitOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if got := balance(t, ctx, pool, "treasury", "USD/2"); got.Sign() != 0 {
		t.Errorf("treasury = %s, want 0", got)
	}
	// but the volumes remember it happened
	var in, out string
	pool.QueryRow(ctx, "select input::text, output::text from accounts_volumes where address='treasury'").Scan(&in, &out)
	if in != "10000" || out != "10000" {
		t.Errorf("treasury volumes = (%s, %s), want (10000, 10000)", in, out)
	}
	assertConserved(t, ctx, pool)
}

func TestMovesCarryRunningSnapshots(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "users:alice", 10000)

	// alice appears in both postings, so her two moves must show different
	// running balances, not the final one twice
	_, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "users:alice", Destination: "users:bob", Asset: "USD/2", Amount: n(3000)},
		{Source: "users:alice", Destination: "fees", Asset: "USD/2", Amount: n(250)},
	}, CommitOptions{})
	if err != nil {
		t.Fatal(err)
	}

	rows, err := pool.Query(ctx, `
		select (pcv_input - pcv_output)::text from moves
		where address='users:alice' and tx_id=2 order by seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var got []string
	for rows.Next() {
		var b string
		rows.Scan(&b)
		got = append(got, b)
	}
	want := []string{"7000", "6750"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("alice's snapshots = %v, want %v", got, want)
	}
}

func TestDuplicateReferenceRejected(t *testing.T) {
	ctx, s, _ := testStore(t)

	opts := CommitOptions{Reference: "order-1001"}
	if _, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "world", Destination: "users:alice", Asset: "USD/2", Amount: n(100)},
	}, opts); err != nil {
		t.Fatal(err)
	}

	_, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "world", Destination: "users:bob", Asset: "USD/2", Amount: n(100)},
	}, opts)
	if !errors.Is(err, ErrDuplicateReference) {
		t.Fatalf("err = %v, want ErrDuplicateReference", err)
	}
}

func TestUnknownLedgerRejected(t *testing.T) {
	ctx, _, pool := testStore(t)
	other := New(pool, "does-not-exist")

	_, err := other.CommitTransaction(ctx, ledger.Postings{
		{Source: "world", Destination: "users:alice", Asset: "USD/2", Amount: n(100)},
	}, CommitOptions{})
	if !errors.Is(err, ErrLedgerNotFound) {
		t.Fatalf("err = %v, want ErrLedgerNotFound", err)
	}
}

// the test the whole design exists for.
func TestConcurrentSpendCannotOverdraw(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "users:alice", 100)

	// hold every goroutine between locking and writing. with the row lock they
	// queue here one at a time, so the pause costs nothing. without it they all
	// read the same balance, all pass the check, and all overdraw.
	s.afterLock = func() { time.Sleep(25 * time.Millisecond) }

	const attempts = 50
	var wg sync.WaitGroup
	results := make([]error, attempts)

	for i := range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[i] = s.CommitTransaction(ctx, ledger.Postings{
				{Source: "users:alice", Destination: fmt.Sprintf("users:bob:%d", i), Asset: "USD/2", Amount: n(10)},
			}, CommitOptions{})
		}()
	}
	wg.Wait()

	var ok, rejected int
	for i, err := range results {
		switch {
		case err == nil:
			ok++
		case errors.As(err, new(*InsufficientFundsError)):
			rejected++
		default:
			t.Errorf("goroutine %d: unexpected error: %v", i, err)
		}
	}

	if ok != 10 {
		t.Errorf("%d transactions succeeded, want exactly 10", ok)
	}
	if rejected != attempts-10 {
		t.Errorf("%d rejected, want %d", rejected, attempts-10)
	}
	if got := balance(t, ctx, pool, "users:alice", "USD/2"); got.Sign() != 0 {
		t.Errorf("alice = %s, want exactly 0", got)
	}
	assertConserved(t, ctx, pool)
}

// two transactions touching the same accounts in opposite posting order.
// without a consistent lock order this is the classic deadlock.
func TestOppositeOrderDoesNotDeadlock(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "a", 100000)
	fund(t, ctx, s, "b", 100000)

	const rounds = 40
	var wg sync.WaitGroup
	errs := make([]error, rounds*2)

	for i := range rounds {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, errs[i*2] = s.CommitTransaction(ctx, ledger.Postings{
				{Source: "a", Destination: "b", Asset: "USD/2", Amount: n(1)},
			}, CommitOptions{})
		}()
		go func() {
			defer wg.Done()
			_, errs[i*2+1] = s.CommitTransaction(ctx, ledger.Postings{
				{Source: "b", Destination: "a", Asset: "USD/2", Amount: n(1)},
			}, CommitOptions{})
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("transfer %d: %v", i, err)
		}
	}

	// the point is not that deadlocks are survived, it is that they never
	// happen. a consistent lock order removes the precondition, so the retry
	// loop should never fire.
	if got := s.Retries(); got != 0 {
		t.Errorf("%d retries, want 0: lock ordering should make deadlock impossible, not merely survivable", got)
	}
	assertConserved(t, ctx, pool)
}
