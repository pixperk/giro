package storage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"math/rand"
	"net"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	giro "github.com/pixperk/giro"
	"github.com/pixperk/giro/ledger"
	"github.com/pixperk/giro/migrate"
)

// Killing a commit at an arbitrary moment, and checking what it left behind.
//
// Every other test here asks whether the code is right when the machinery
// works. These ask what happens when it does not: the connection dies between
// the lock and the commit, the server kills the backend, the caller's deadline
// expires while Postgres is still deciding. Those are the moments a ledger
// loses or duplicates money, and they cannot be provoked by calling functions
// in the ordinary way.
//
// # What is actually being tested
//
// One thing, in two directions:
//
//	a commit that returned an error left nothing behind
//	a commit that returned success is still there
//
// And the case between them, which is the one that matters most. A connection
// killed after the server committed but before the client heard about it
// leaves the caller unable to tell which happened. That is not a bug to be
// fixed, it is a property of networks -- so the question is whether the
// remedy works, and the remedy is the idempotency key. Retrying with the same
// key must produce exactly one transaction whichever side of the commit the
// connection died on.
//
// # Determinism
//
// The fault schedule comes from a seed, printed on failure, so a red run
// replays exactly:
//
//	GIRO_CHAOS_SEED=1738 go test -run TestChaos ./storage/
//
// This is not deterministic simulation testing in the FoundationDB sense --
// that needs control of the disk and the network, which belongs to Postgres
// here, not to us. What it does control is when the client dies, which is
// where the interesting half of the ambiguity lives.

// chaosDialer hands out connections that can be told to die mid-conversation.
//
// It counts writes rather than statements because that is what it can see: one
// write is roughly one flush to the server, so arming it at N kills the
// connection around the Nth message. The exact statement does not matter -- the
// point is to land somewhere unplanned and check the invariants either way.
type chaosDialer struct {
	armed atomic.Int64 // writes remaining before the kill; <= 0 disarms
	fired atomic.Bool
}

func (d *chaosDialer) arm(afterWrites int64) {
	d.fired.Store(false)
	d.armed.Store(afterWrites)
}

func (d *chaosDialer) disarm() { d.armed.Store(0) }

func (d *chaosDialer) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	return &chaosConn{Conn: conn, d: d}, nil
}

type chaosConn struct {
	net.Conn
	d *chaosDialer
}

var errChaos = errors.New("chaos: connection killed mid-flight")

func (c *chaosConn) Write(b []byte) (int, error) {
	if c.d.armed.Load() > 0 && c.d.armed.Add(-1) == 0 && c.d.fired.CompareAndSwap(false, true) {
		// close the underlying socket rather than returning a polite error, so
		// pgx sees what a real severed connection looks like
		_ = c.Close()
		return 0, errChaos
	}
	return c.Conn.Write(b)
}

// chaosStore is testStore with a dialer that can be armed. It is a copy rather
// than a flag on testStore because every other test in this package wants
// connections that behave.
func chaosStore(t *testing.T) (context.Context, *Store, *pgxpool.Pool, *chaosDialer) {
	t.Helper()
	ctx := context.Background()

	schema := fmt.Sprintf("chaos_%d_%d", os.Getpid(), testCounter())
	admin, err := pgxpool.New(ctx, testURL())
	if err != nil {
		skipNoDatabase(t, err)
	}
	if _, err := admin.Exec(ctx, "create schema "+schema); err != nil {
		skipNoDatabase(t, err)
	}
	admin.Close()
	t.Cleanup(func() {
		c, err := pgxpool.New(context.Background(), testURL())
		if err == nil {
			_, _ = c.Exec(context.Background(), "drop schema "+schema+" cascade")
			c.Close()
		}
	})

	d := &chaosDialer{}
	cfg, err := pgxpool.ParseConfig(testURL())
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.ConnConfig.DialFunc = d.dial
	cfg.MaxConns = 8
	// a killed connection must not be handed to the next caller
	cfg.MaxConnLifetime = time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sub, _ := fs.Sub(giro.MigrationsFS, giro.MigrationsDir)
	if _, err := migrate.Run(ctx, conn.Conn(), sub); err != nil {
		t.Fatal(err)
	}
	conn.Release()

	s := New(pool, "main")
	if _, err := s.CreateLedger(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterAsset(ctx, "USD/2"); err != nil {
		t.Fatal(err)
	}
	return ctx, s, pool, d
}

func chaosSeed(t *testing.T) int64 {
	t.Helper()
	if v := os.Getenv("GIRO_CHAOS_SEED"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			t.Fatalf("GIRO_CHAOS_SEED=%q: %v", v, err)
		}
		return n
	}
	return time.Now().UnixNano()
}

// assertIntact is what every chaos iteration ends with. A ledger that survived
// a killed connection has to be indistinguishable from one that was never
// touched -- not merely still running.
func assertIntact(t *testing.T, ctx context.Context, s *Store, pool *pgxpool.Pool, seed int64, when string) {
	t.Helper()
	fail := func(format string, args ...any) {
		t.Errorf("%s (seed %d): "+format, append([]any{when, seed}, args...)...)
	}

	if _, err := s.VerifyConservation(ctx); err != nil {
		fail("conservation broken: %v", err)
	}
	if _, err := s.VerifyLog(ctx); err != nil {
		fail("hash chain broken: %v", err)
	}
	if _, err := s.VerifyProjection(ctx); err != nil {
		fail("tables disagree with the log: %v", err)
	}

	// A transaction row with no log entry, or the reverse, means a commit tore
	// in half -- which the database transaction is supposed to make impossible
	// and is exactly what a mid-flight kill would expose if it were not.
	var txs, logs, tip int64
	if err := pool.QueryRow(ctx, `
		select (select count(*) from transactions where ledger='main'),
		       (select count(*) from logs where ledger='main' and type='NEW_TRANSACTION'),
		       (select last_log_id from ledgers where name='main')`).Scan(&txs, &logs, &tip); err != nil {
		t.Fatal(err)
	}
	if txs != logs {
		fail("%d transactions but %d log entries: a commit landed in halves", txs, logs)
	}

	// ids are gapless, so a counter ahead of the entries means an allocation
	// escaped its own rollback
	var entries int64
	if err := pool.QueryRow(ctx,
		"select count(*) from logs where ledger='main'").Scan(&entries); err != nil {
		t.Fatal(err)
	}
	if tip != entries {
		fail("log id is at %d with %d entries: %d allocations leaked", tip, entries, tip-entries)
	}
}

// The core property, at every point in the commit a connection can die.
//
// The kill lands wherever the write count puts it: before the locks, between
// the lock and the allocation, after the log entry, or in the COMMIT itself.
// Whichever it is, the ledger has to come out whole.
func TestChaosAKilledConnectionLeavesTheLedgerWhole(t *testing.T) {
	ctx, s, pool, d := chaosStore(t)
	seed := chaosSeed(t)
	rng := rand.New(rand.NewSource(seed))
	t.Logf("seed %d — replay with GIRO_CHAOS_SEED=%d", seed, seed)

	fund(t, ctx, s, "users:alice", 100_000_000)

	var killed, survived int
	for i := range 40 {
		// somewhere in the conversation. a commit is nine round trips, so this
		// range straddles the whole of it -- including past the end, where the
		// commit survives and the kill lands on the next one. both outcomes
		// are wanted: the interesting kills are the ones near COMMIT, where
		// the server has already decided and the client never hears.
		d.arm(int64(1 + rng.Intn(22)))

		_, err := s.CommitTransaction(ctx, ledger.Postings{
			{Source: "users:alice", Destination: ledger.Address(fmt.Sprintf("users:b%d", i)),
				Asset: "USD/2", Amount: big.NewInt(1)},
		}, CommitOptions{})
		d.disarm()

		if err != nil {
			killed++
		} else {
			survived++
		}
		assertIntact(t, ctx, s, pool, seed, fmt.Sprintf("iteration %d", i))
	}

	t.Logf("%d commits killed, %d survived", killed, survived)
	if killed == 0 {
		t.Fatal("no commit was actually killed, so this asserted nothing")
	}
}

// The one that decides whether money is safe.
//
// A connection killed around COMMIT leaves the caller unable to tell whether
// the transaction landed. That ambiguity is a property of networks and cannot
// be engineered away; what can be engineered is the remedy. Retrying with the
// same idempotency key must leave exactly one transaction, whichever side of
// the commit the connection died on.
func TestChaosARetryAfterAKilledCommitDoesNotPayTwice(t *testing.T) {
	ctx, s, pool, d := chaosStore(t)
	seed := chaosSeed(t)
	rng := rand.New(rand.NewSource(seed))
	t.Logf("seed %d — replay with GIRO_CHAOS_SEED=%d", seed, seed)

	fund(t, ctx, s, "users:alice", 100_000_000)

	const payment = 1000
	var attempted, landed int

	for i := range 30 {
		key := fmt.Sprintf("inv-%d", i)
		postings := ledger.Postings{
			{Source: "users:alice", Destination: "users:bob", Asset: "USD/2", Amount: big.NewInt(payment)},
		}

		// first attempt, killed at an arbitrary point -- sometimes before the
		// server decides, sometimes after, which is the whole difficulty
		d.arm(int64(1 + rng.Intn(22)))
		_, first := s.CommitTransaction(ctx, postings, CommitOptions{IdempotencyKey: key})
		d.disarm()

		// the caller does what a payment client must do: retry the same
		// request under the same key, because it cannot know what happened
		_, second := s.CommitTransaction(ctx, postings, CommitOptions{IdempotencyKey: key})
		if second != nil {
			t.Fatalf("seed %d: retry under key %s failed: %v", seed, key, second)
		}
		attempted++
		_ = first

		// exactly one, no matter how the first attempt ended
		var n int
		if err := pool.QueryRow(ctx,
			"select count(*) from logs where ledger='main' and idempotency_key=$1", key).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("seed %d: key %s produced %d transactions, want exactly 1", seed, key, n)
		}
		landed++
		assertIntact(t, ctx, s, pool, seed, "after retry "+key)
	}

	// and the money agrees: bob holds one payment per key, not two
	bal, err := s.GetBalances(ctx, "users:bob")
	if err != nil {
		t.Fatal(err)
	}
	want := big.NewInt(int64(landed) * payment)
	if bal["USD/2"].Cmp(want) != 0 {
		t.Errorf("seed %d: bob holds %v, want %v — a retry paid twice or lost one",
			seed, bal["USD/2"], want)
	}
	t.Logf("%d payments, each retried after a killed connection, %d landed exactly once", attempted, landed)
}

// The server's side of the same failure: Postgres kills the backend while the
// transaction is open, which is what a failover, an OOM kill or an
// administrator does.
func TestChaosATerminatedBackendLeavesNothingBehind(t *testing.T) {
	ctx, s, pool, _ := chaosStore(t)
	seed := chaosSeed(t)
	fund(t, ctx, s, "users:alice", 100_000_000)

	before, err := s.ChainTip(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// kill the backend from the outside, in the window between taking the
	// locks and committing
	var killedPID int
	s.afterLock = func() {
		killer, err := pgxpool.New(ctx, testURL())
		if err != nil {
			return
		}
		defer killer.Close()
		_ = killer.QueryRow(ctx, `
			select pid from pg_stat_activity
			 where state = 'idle in transaction' and pid <> pg_backend_pid()
			 limit 1`).Scan(&killedPID)
		if killedPID != 0 {
			_, _ = killer.Exec(ctx, "select pg_terminate_backend($1)", killedPID)
		}
	}

	_, err = s.CommitTransaction(ctx, ledger.Postings{
		{Source: "users:alice", Destination: "users:carol", Asset: "USD/2", Amount: big.NewInt(500)},
	}, CommitOptions{})
	s.afterLock = nil

	if killedPID == 0 {
		t.Skip("could not find the backend to terminate")
	}
	if err == nil {
		t.Log("the commit survived the termination, which is also a valid outcome")
	}

	after, err := s.ChainTip(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err == nil && after.LogID != before.LogID && after.LogID != before.LogID+1 {
		t.Errorf("seed %d: chain tip moved from %d to %d", seed, before.LogID, after.LogID)
	}
	assertIntact(t, ctx, s, pool, seed, "after pg_terminate_backend")
}

// A caller's deadline expiring mid-commit. Different from a severed connection
// because pgx gets to unwind cleanly, and a clean unwind is exactly where a
// half-finished transaction could hide.
func TestChaosACancelledContextLeavesNothingBehind(t *testing.T) {
	ctx, s, pool, _ := chaosStore(t)
	seed := chaosSeed(t)
	rng := rand.New(rand.NewSource(seed))
	t.Logf("seed %d — replay with GIRO_CHAOS_SEED=%d", seed, seed)

	fund(t, ctx, s, "users:alice", 100_000_000)

	var cancelled int
	for i := range 30 {
		// a deadline short enough to land somewhere inside the commit
		d := time.Duration(rng.Intn(3000)) * time.Microsecond
		runCtx, cancel := context.WithTimeout(ctx, d)

		_, err := s.CommitTransaction(runCtx, ledger.Postings{
			{Source: "users:alice", Destination: ledger.Address(fmt.Sprintf("users:c%d", i)),
				Asset: "USD/2", Amount: big.NewInt(1)},
		}, CommitOptions{})
		cancel()

		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			cancelled++
		}
		assertIntact(t, ctx, s, pool, seed, fmt.Sprintf("cancel iteration %d", i))
	}

	t.Logf("%d of 30 commits were cut short by the deadline", cancelled)
	if cancelled == 0 {
		t.Skip("no commit was slow enough to interrupt on this machine")
	}
}
