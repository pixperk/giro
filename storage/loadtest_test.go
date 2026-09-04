package storage

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pixperk/giro/ledger"
)

// Load, as distinct from the benchmarks beside this file.
//
// The benchmarks answer "how fast is one operation". These answer the two
// questions you actually have before putting a ledger in front of somebody
// else's money:
//
//   - what does a caller wait, at the tail, under sustained concurrency
//   - is the book still correct afterwards
//
// The second is the one most load tests skip, and for a ledger it is the whole
// point. Throughput that arrives with a broken invariant is not throughput. So
// every scenario here ends by verifying conservation, the hash chain and the
// projection against the log, and treats a finding as a failure of the load
// test rather than a separate concern.
//
// Three further things this measures that a naive harness gets wrong:
//
// A refusal is not an error. "users:alice cannot spend money she does not
// have" is the ledger working, and counting it as a failure would make a
// correct run look broken -- or, worse, make a run that refused everything look
// fast.
//
// Retries are a correctness signal. Sorted lock ordering is supposed to make
// deadlocks impossible; a commit is never the victim, because whichever
// transaction closes the cycle is the one Postgres kills and giro's always
// acquires first and waits. So a non-zero retry count under contention means
// the ordering has been broken, and that matters more than any latency number
// on the same run.
//
// Time, not iterations. A fixed iteration count measures a warm cache and a
// lucky window. These run for a duration, so what comes out is a steady state
// somebody could hold a pager against.
//
// They are skipped unless GIRO_LOAD is set, because they take minutes:
//
//	just load
//	GIRO_LOAD=1 go test -run TestLoad -v ./storage/

func loadEnabled(t *testing.T) time.Duration {
	t.Helper()
	v := os.Getenv("GIRO_LOAD")
	if v == "" {
		t.Skip("set GIRO_LOAD=1 to run load tests, or GIRO_LOAD=30s for longer")
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	return 5 * time.Second
}

// result is one scenario's outcome. Latencies are kept per operation rather
// than averaged as they arrive, because the average is the one number that
// never tells you anything about a tail.
type loadStats struct {
	name      string
	workers   int
	elapsed   time.Duration
	committed int64
	refused   int64
	failed    int64
	retries   int64
	latency   []time.Duration
	firstErr  error
}

func (r *loadStats) rate() float64 { return float64(r.committed) / r.elapsed.Seconds() }

// percentile is nearest-rank on the sorted slice: no interpolation, so every
// number printed is a latency that some request actually experienced.
func (r *loadStats) percentile(p float64) time.Duration {
	if len(r.latency) == 0 {
		return 0
	}
	i := int(p * float64(len(r.latency)))
	if i >= len(r.latency) {
		i = len(r.latency) - 1
	}
	return r.latency[i]
}

func (r *loadStats) String() string {
	return fmt.Sprintf(
		"%-22s w=%-3d %7.0f tx/s   p50 %6s  p95 %6s  p99 %6s  max %6s   refused %d  retries %d",
		r.name, r.workers, r.rate(),
		round(r.percentile(0.50)), round(r.percentile(0.95)),
		round(r.percentile(0.99)), round(r.percentile(1.0)),
		r.refused, r.retries)
}

func round(d time.Duration) time.Duration {
	switch {
	case d > time.Second:
		return d.Round(10 * time.Millisecond)
	case d > time.Millisecond:
		return d.Round(100 * time.Microsecond)
	default:
		return d.Round(time.Microsecond)
	}
}

// drive runs work in parallel for a duration and collects what happened.
//
// Each worker keeps its own latency slice and its own counters, merged at the
// end, so the measurement does not itself become the contention being
// measured -- a shared mutex around every sample would put the harness on the
// critical path and quietly flatten the tail it exists to find.
func drive(t *testing.T, s *Store, name string, workers int, d time.Duration,
	work func(ctx context.Context, worker, i int) error) *loadStats {
	t.Helper()

	r := &loadStats{name: name, workers: workers}
	before := s.Retries()

	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()

	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		failures atomic.Int64
	)
	start := time.Now()
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var (
				local           []time.Duration
				committed, refs int64
				firstErr        error
			)
			for i := 0; ctx.Err() == nil; i++ {
				began := time.Now()
				err := work(ctx, w, i)
				took := time.Since(began)

				switch {
				case err == nil:
					committed++
					local = append(local, took)
				case ctx.Err() != nil:
					// the clock ran out mid-request; not a result either way
				default:
					if _, refused := CauseOf(err); refused {
						refs++
						continue
					}
					failures.Add(1)
					if firstErr == nil {
						firstErr = err
					}
				}
			}
			mu.Lock()
			r.latency = append(r.latency, local...)
			r.committed += committed
			r.refused += refs
			if r.firstErr == nil {
				r.firstErr = firstErr
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	r.elapsed = time.Since(start)
	r.failed = failures.Load()
	r.retries = s.Retries() - before
	slices.Sort(r.latency)
	return r
}

// assertSound is what makes this a ledger load test rather than a throughput
// benchmark. Speed that arrives with a broken invariant is not a result.
func assertSound(t *testing.T, ctx context.Context, s *Store, pool *pgxpool.Pool, r *loadStats) {
	t.Helper()
	t.Log(r)

	if r.failed > 0 {
		t.Errorf("%s: %d requests failed for a reason that was not a refusal: %v",
			r.name, r.failed, r.firstErr)
	}
	if r.committed == 0 {
		t.Fatalf("%s: nothing committed, so the numbers above mean nothing", r.name)
	}

	// the design claim, and the one worth failing on. transactions should
	// queue on the sorted lock rather than deadlock, so this is zero.
	if r.retries > 0 {
		t.Errorf("%s: %d retries. sorted lock ordering should make deadlocks "+
			"impossible, so this means the ordering has been broken somewhere", r.name, r.retries)
	}

	assertConserved(t, ctx, pool)
	if _, err := s.VerifyLog(ctx); err != nil {
		t.Errorf("%s: the hash chain broke under load: %v", r.name, err)
	}
	if _, err := s.VerifyProjection(ctx); err != nil {
		t.Errorf("%s: the tables disagree with the log after load: %v", r.name, err)
	}
}

// The wall this design meets first. Every caller spends from one account, so
// they queue on its volume row as well as on the ledgers row -- a treasury, a
// fee account, or world itself.
//
// The number to look at is not the throughput but the shape of the curve: if
// it is flat from 1 caller to 32, the serialisation is doing what the design
// says, and the tail latency is what a caller pays for standing in the queue.
func TestLoadHotAccount(t *testing.T) {
	d := loadEnabled(t)
	for _, workers := range []int{1, 4, 16, 32} {
		t.Run(fmt.Sprintf("workers=%d", workers), func(t *testing.T) {
			ctx, s, pool := testStore(t)
			fund(t, ctx, s, "treasury", 1_000_000_000_000)

			var seq atomic.Int64
			r := drive(t, s, "hot account", workers, d, func(ctx context.Context, _, _ int) error {
				dst := ledger.Address(fmt.Sprintf("payee:%d", seq.Add(1)))
				_, err := s.CommitTransaction(ctx, transfer("treasury", dst, 1), CommitOptions{})
				return err
			})
			assertSound(t, ctx, s, pool, r)
		})
	}
}

// Disjoint accounts, so the only thing shared is the ledgers row. If this
// matches the hot account case, the ledgers row is the bottleneck rather than
// account contention -- which is what the design predicts, and is why adding
// ledgers is the way to scale writes.
func TestLoadDisjointAccounts(t *testing.T) {
	d := loadEnabled(t)
	ctx, s, pool := testStore(t)

	const workers = 16
	for w := range workers {
		fund(t, ctx, s, ledger.Address(fmt.Sprintf("payer:%d", w)), 1_000_000_000)
	}

	r := drive(t, s, "disjoint", workers, d, func(ctx context.Context, w, i int) error {
		from := ledger.Address(fmt.Sprintf("payer:%d", w))
		to := ledger.Address(fmt.Sprintf("payee:%d:%d", w, i))
		_, err := s.CommitTransaction(ctx, transfer(from, to, 1), CommitOptions{})
		return err
	})
	assertSound(t, ctx, s, pool, r)
}

// The escape hatch the design claims: separate ledgers share no row at all, so
// throughput scales by adding them rather than by adding callers to one.
func TestLoadAcrossLedgers(t *testing.T) {
	d := loadEnabled(t)
	ctx, first, pool := testStore(t)

	const ledgers = 8
	stores := []*Store{first}
	for i := 1; i < ledgers; i++ {
		name := fmt.Sprintf("load%d", i)
		s := New(pool, name)
		if _, err := s.CreateLedger(ctx); err != nil {
			t.Fatal(err)
		}
		if err := s.RegisterAsset(ctx, "USD/2"); err != nil {
			t.Fatal(err)
		}
		fund(t, ctx, s, "payer", 1_000_000_000)
		stores = append(stores, s)
	}
	fund(t, ctx, first, "payer", 1_000_000_000)

	var seq atomic.Int64
	r := drive(t, first, "across ledgers", ledgers*2, d, func(ctx context.Context, w, _ int) error {
		s := stores[w%len(stores)]
		dst := ledger.Address(fmt.Sprintf("payee:%d", seq.Add(1)))
		_, err := s.CommitTransaction(ctx, transfer("payer", dst, 1), CommitOptions{})
		return err
	})

	// the aggregate rate is across every ledger, so compare it to the hot
	// account run at the same worker count rather than to one ledger's ceiling
	t.Logf("%v  (aggregate across %d ledgers)", r, ledgers)
	if r.failed > 0 {
		t.Errorf("%d failures: %v", r.failed, r.firstErr)
	}
	for _, s := range stores {
		if _, err := s.VerifyLog(ctx); err != nil {
			t.Errorf("chain broke under load: %v", err)
		}
	}
	assertConserved(t, ctx, pool)
}

// A realistic mix rather than one shape repeated. Reads share no lock with
// writes and should be unaffected; a read latency that tracks the write tail
// means something is serialising that should not be.
func TestLoadMixedReadsAndWrites(t *testing.T) {
	d := loadEnabled(t)
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "treasury", 1_000_000_000_000)

	var seq atomic.Int64
	writes := drive(t, s, "mixed writes", 8, d, func(ctx context.Context, _, _ int) error {
		dst := ledger.Address(fmt.Sprintf("payee:%d", seq.Add(1)))
		_, err := s.CommitTransaction(ctx, transfer("treasury", dst, 1), CommitOptions{})
		return err
	})
	reads := drive(t, s, "mixed reads", 8, d, func(ctx context.Context, _, _ int) error {
		_, err := s.GetBalances(ctx, "treasury")
		return err
	})

	assertSound(t, ctx, s, pool, writes)
	t.Log(reads)
	if reads.failed > 0 {
		t.Errorf("%d read failures: %v", reads.failed, reads.firstErr)
	}
	if reads.percentile(0.99) > writes.percentile(0.99) {
		t.Errorf("read p99 %v exceeds write p99 %v: reads are queueing behind writes, "+
			"which they should not be", round(reads.percentile(0.99)), round(writes.percentile(0.99)))
	}
}

// A batch pays for the ledgers row lock once instead of fifty times. That is
// only worth anything when the lock is contended, so this measures it under
// contention -- and each arm gets its own store, because running one after the
// other would give the second a larger table to insert into and quietly
// measure that instead.
func TestLoadBatchVersusSingles(t *testing.T) {
	d := loadEnabled(t)
	const size = 50
	const workers = 16

	batchCtx, batchStore, batchPool := testStore(t)
	fund(t, batchCtx, batchStore, "treasury", 1_000_000_000_000)
	var batchSeq atomic.Int64
	batched := drive(t, batchStore, "batch of 50", workers, d, func(ctx context.Context, _, _ int) error {
		items := make([]BatchItem, size)
		for j := range items {
			dst := ledger.Address(fmt.Sprintf("payee:%d", batchSeq.Add(1)))
			items[j] = BatchItem{Postings: transfer("treasury", dst, 1)}
		}
		_, err := batchStore.CommitBatch(ctx, items, CommitOptions{})
		return err
	})

	singleCtx, singleStore, _ := testStore(t)
	fund(t, singleCtx, singleStore, "treasury", 1_000_000_000_000)
	var singleSeq atomic.Int64
	singles := drive(t, singleStore, "singles", workers, d, func(ctx context.Context, _, _ int) error {
		dst := ledger.Address(fmt.Sprintf("payee:%d", singleSeq.Add(1)))
		_, err := singleStore.CommitTransaction(ctx, transfer("treasury", dst, 1), CommitOptions{})
		return err
	})

	perTx := batched.rate() * size
	t.Logf("%v", batched)
	t.Logf("%v", singles)
	t.Logf("batching moves %.0f transactions/s against %.0f as singles: %.2fx",
		perTx, singles.rate(), perTx/singles.rate())
	t.Logf("the trade is latency: a batch waits %v at p99 to move %d transactions, "+
		"a single waits %v to move one", round(batched.percentile(0.99)), size,
		round(singles.percentile(0.99)))

	// Deliberately not asserting a ratio. Whether batching wins depends on how
	// contended the ledger row is, and pinning a number here would turn a
	// machine having a bad afternoon into a failing build. What is asserted is
	// that it works and leaves the book sound; the ratio is reported for a
	// person to read.
	assertSound(t, batchCtx, batchStore, batchPool, batched)
}

// Refusals must be cheap and must not damage anything. A ledger under attack,
// or in front of a buggy caller, spends its time saying no -- and a refusal
// path that leaves rows behind or slows the accepted path is how a correct
// system falls over while behaving correctly.
func TestLoadUnderRefusal(t *testing.T) {
	d := loadEnabled(t)
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "treasury", 1_000_000)

	r := drive(t, s, "all refused", 16, d, func(ctx context.Context, _, _ int) error {
		// nobody has this, so every one of these is refused
		_, err := s.CommitTransaction(ctx, transfer("users:empty", "users:other", 1_000_000_000), CommitOptions{})
		return err
	})

	t.Logf("%v", r)
	if r.refused == 0 {
		t.Fatal("nothing was refused, so this measured the wrong thing")
	}
	if r.failed > 0 {
		t.Errorf("%d refusals arrived as failures: %v", r.failed, r.firstErr)
	}

	// the important half: a refused transaction did not happen, so it left
	// nothing behind. not a zero row, not a volume row, nothing.
	var rows int
	if err := pool.QueryRow(ctx,
		"select count(*) from accounts_volumes where ledger='main' and address like 'users:%'").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("%d volume rows left by refused transactions: a refusal wrote something", rows)
	}
	assertConserved(t, ctx, pool)
}

// A soak. Long enough that anything which leaks -- connections, ids, rows a
// rolled back transaction was supposed to take with it -- has time to show.
func TestLoadSoak(t *testing.T) {
	d := loadEnabled(t)
	if d < 20*time.Second {
		t.Skip("set GIRO_LOAD to 30s or more to soak")
	}
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "treasury", 1_000_000_000_000)

	var seq atomic.Int64
	r := drive(t, s, "soak", 16, d, func(ctx context.Context, w, i int) error {
		// a mix, so this is not one code path held open for a minute
		dst := ledger.Address(fmt.Sprintf("payee:%d", seq.Add(1)))
		switch i % 4 {
		case 0:
			_, err := s.CommitTransaction(ctx, ledger.Postings{
				{Source: "treasury", Destination: dst, Asset: "USD/2", Amount: big.NewInt(2)},
				{Source: "treasury", Destination: "fees:platform", Asset: "USD/2", Amount: big.NewInt(1)},
			}, CommitOptions{})
			return err
		case 1:
			_, err := s.GetBalances(ctx, "treasury")
			return err
		default:
			_, err := s.CommitTransaction(ctx, transfer("treasury", dst, 1), CommitOptions{})
			return err
		}
	})
	assertSound(t, ctx, s, pool, r)

	// ids are gapless, so the highest id and the number of entries agree if
	// nothing leaked an allocation
	tip, err := s.ChainTip(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var entries int64
	if err := pool.QueryRow(ctx, "select count(*) from logs where ledger='main'").Scan(&entries); err != nil {
		t.Fatal(err)
	}
	if tip.LogID != entries {
		t.Errorf("log id is at %d but there are %d entries: %d allocations leaked",
			tip.LogID, entries, tip.LogID-entries)
	}
}

// Printed at the end of a run so the numbers arrive with the caveat attached.
func TestLoadSummary(t *testing.T) {
	loadEnabled(t)
	t.Log(strings.TrimSpace(`
These are laptop numbers against local Postgres. They are useful for the shape
of the curve -- flat from 1 caller to 32 means the ledger row is serialising as
designed -- and for catching a regression. They are not a capacity plan for
your hardware, your network, or a pooler in the path.

What to look at, in order:
  retries   must be zero. anything else means the lock ordering broke.
  p99       what a caller waits at the tail, which is what an SLA is about.
  the curve flat is correct. rising throughput would mean the lock is not held.
`))
}
