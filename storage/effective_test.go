package storage

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/pixperk/giro/ledger"
)

func day(n int) time.Time {
	return time.Date(2026, 3, n, 12, 0, 0, 0, time.UTC)
}

func commitAt(t *testing.T, ctx context.Context, s *Store, from, to ledger.Address, amount int64, at time.Time) *ledger.Transaction {
	t.Helper()
	tx, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: from, Destination: to, Asset: "USD/2", Amount: n(amount)},
	}, CommitOptions{Timestamp: at})
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

// the fast path is only trustworthy because this checks it. maintaining a
// snapshot on write is an optimisation, and an optimisation nothing verifies is
// a guess.
func assertEffectiveVolumesAreCorrect(t *testing.T, ctx context.Context, s *Store) {
	t.Helper()
	if _, err := s.VerifyEffectiveVolumes(ctx); err != nil {
		t.Errorf("effective volumes disagree with a replay: %v", err)
	}
}

func TestEffectiveVolumesWithoutBackdating(t *testing.T) {
	ctx, s, pool := testStore(t)
	commitAt(t, ctx, s, "world", "alice", 100, day(1))
	commitAt(t, ctx, s, "world", "alice", 50, day(3))

	// with both clocks agreeing, the two views are the same
	var pcv, pcev string
	pool.QueryRow(ctx, `select (pcv_input-pcv_output)::text, (pcev_input-pcev_output)::text
		from moves where address='alice' order by seq desc limit 1`).Scan(&pcv, &pcev)
	if pcv != pcev || pcv != "150" {
		t.Errorf("pcv %s, pcev %s, want both 150", pcv, pcev)
	}
	assertEffectiveVolumesAreCorrect(t, ctx, s)
}

// the case the whole feature exists for.
func TestBackdatedTransactionShiftsLaterMoves(t *testing.T) {
	ctx, s, pool := testStore(t)
	commitAt(t, ctx, s, "world", "alice", 100, day(1))
	commitAt(t, ctx, s, "world", "alice", 50, day(3))
	commitAt(t, ctx, s, "alice", "bob", 30, day(5))

	// a settlement file arrives describing something that happened on day 2
	commitAt(t, ctx, s, "world", "alice", 50, day(2))

	rows, err := pool.Query(ctx, `
		select effective_date::date::text, (pcv_input-pcv_output)::text, (pcev_input-pcev_output)::text
		  from moves where ledger='main' and address='alice'
		 order by effective_date, seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	type row struct{ date, pcv, pcev string }
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.date, &r.pcv, &r.pcev); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}

	// the two columns tell different stories about the same four moves.
	//
	// pcv follows insertion order, so the day 2 move was the fourth committed
	// and saw alice at 100 -> 150 -> 120 before adding 50, giving 170.
	//
	// pcev follows effective order, so the same move sits second and reads
	// 100 + 50 = 150, and the two moves after it shift up by 50.
	want := []row{
		{"2026-03-01", "100", "100"},
		{"2026-03-02", "170", "150"}, // committed last, dated second
		{"2026-03-03", "150", "200"}, // pcv frozen at 150, pcev shifted up by 50
		{"2026-03-05", "120", "170"},
	}
	if len(got) != len(want) {
		t.Fatalf("%d moves, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("move on %s: pcv %s pcev %s, want pcv %s pcev %s",
				got[i].date, got[i].pcv, got[i].pcev, want[i].pcv, want[i].pcev)
		}
	}
	assertEffectiveVolumesAreCorrect(t, ctx, s)
}

// pcv records what the ledger believed at the time and must never move.
func TestBackdatingNeverRewritesPCV(t *testing.T) {
	ctx, s, pool := testStore(t)
	commitAt(t, ctx, s, "world", "alice", 100, day(1))
	commitAt(t, ctx, s, "world", "alice", 50, day(5))

	snapshot := func() []string {
		rows, _ := pool.Query(ctx, `select (pcv_input-pcv_output)::text from moves
			where address='alice' order by seq`)
		defer rows.Close()
		var out []string
		for rows.Next() {
			var v string
			rows.Scan(&v)
			out = append(out, v)
		}
		return out
	}

	before := snapshot()
	commitAt(t, ctx, s, "world", "alice", 999, day(2))
	after := snapshot()

	for i := range before {
		if before[i] != after[i] {
			t.Errorf("move %d pcv changed from %s to %s: it records what was believed at the time",
				i, before[i], after[i])
		}
	}
}

func TestBalanceAsOfADate(t *testing.T) {
	ctx, s, _ := testStore(t)
	commitAt(t, ctx, s, "world", "alice", 100, day(1))
	commitAt(t, ctx, s, "world", "alice", 50, day(3))
	commitAt(t, ctx, s, "alice", "bob", 30, day(5))
	commitAt(t, ctx, s, "world", "alice", 50, day(2)) // backdated

	tests := []struct {
		on   int
		want int64
	}{
		{1, 100},
		{2, 150},
		{3, 200},
		{4, 200},
		{5, 170},
		{9, 170},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("day %d", tc.on), func(t *testing.T) {
			got, err := s.GetBalancesAt(ctx, "alice", day(tc.on))
			if err != nil {
				t.Fatal(err)
			}
			if got["USD/2"].Cmp(n(tc.want)) != 0 {
				t.Errorf("balance on day %d = %s, want %d", tc.on, got["USD/2"], tc.want)
			}
		})
	}

	// before the account existed
	got, err := s.GetBalancesAt(ctx, "alice", day(-5))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("balance before any activity = %v, want nothing", got)
	}
}

// two transactions on the same effective date order by insertion, so the one
// that arrived first comes first.
func TestTiesOnTheSameEffectiveDate(t *testing.T) {
	ctx, s, _ := testStore(t)
	commitAt(t, ctx, s, "world", "alice", 100, day(1))
	commitAt(t, ctx, s, "world", "alice", 50, day(1))
	commitAt(t, ctx, s, "world", "alice", 25, day(1))

	got, err := s.GetBalancesAt(ctx, "alice", day(1))
	if err != nil {
		t.Fatal(err)
	}
	if got["USD/2"].Cmp(n(175)) != 0 {
		t.Errorf("balance = %s, want 175", got["USD/2"])
	}
	assertEffectiveVolumesAreCorrect(t, ctx, s)
}

func TestBackdatedBeforeEverything(t *testing.T) {
	ctx, s, _ := testStore(t)
	commitAt(t, ctx, s, "world", "alice", 100, day(10))
	commitAt(t, ctx, s, "world", "alice", 100, day(20))
	commitAt(t, ctx, s, "world", "alice", 7, day(1))

	if got, err := s.GetBalancesAt(ctx, "alice", day(1)); err != nil {
		t.Fatal(err)
	} else if got["USD/2"].Cmp(n(7)) != 0 {
		t.Errorf("day 1 = %s, want 7", got["USD/2"])
	}
	if got, err := s.GetBalancesAt(ctx, "alice", day(25)); err != nil {
		t.Fatal(err)
	} else if got["USD/2"].Cmp(n(207)) != 0 {
		t.Errorf("day 25 = %s, want 207", got["USD/2"])
	}
	assertEffectiveVolumesAreCorrect(t, ctx, s)
}

// a reversal goes through the same path, so it maintains the snapshot too.
func TestRevertMaintainsEffectiveVolumes(t *testing.T) {
	ctx, s, _ := testStore(t)
	commitAt(t, ctx, s, "world", "alice", 1000, day(1))
	payment := commitAt(t, ctx, s, "alice", "bob", 300, day(5))

	if _, err := s.RevertTransaction(ctx, payment.ID, RevertOptions{AtEffectiveDate: true}); err != nil {
		t.Fatal(err)
	}
	assertEffectiveVolumesAreCorrect(t, ctx, s)

	if got, err := s.GetBalancesAt(ctx, "bob", day(9)); err != nil {
		t.Fatal(err)
	} else if got["USD/2"].Sign() != 0 {
		t.Errorf("bob = %s, want 0 after the reversal", got["USD/2"])
	}
}

// the invariant that makes maintaining the snapshot on write safe: whatever
// order transactions arrive in, and whenever they claim to have happened, the
// stored value must equal what a replay produces.
func TestEffectiveVolumesSurviveRandomOrdering(t *testing.T) {
	ctx, s, _ := testStore(t)

	accounts := []ledger.Address{"alice", "bob", "carol"}
	rng := seededRand(t)

	// fund everyone far in the past so nothing runs out
	for _, a := range accounts {
		commitAt(t, ctx, s, "world", a, 1_000_000, day(1))
	}

	for range 60 {
		from := accounts[rng.IntN(len(accounts))]
		to := accounts[rng.IntN(len(accounts))]
		if from == to {
			to = "world"
		}
		// dates deliberately out of order, including deep in the past
		commitAt(t, ctx, s, from, to, rng.Int64N(500)+1, day(rng.IntN(28)+1))
	}

	checked, err := s.VerifyEffectiveVolumes(ctx)
	if err != nil {
		t.Fatalf("after 60 randomly dated transactions: %v", err)
	}
	if checked == 0 {
		t.Fatal("nothing was checked")
	}
	t.Logf("verified %d moves", checked)
}

// A property test is meant to go looking, not to re-check one answer. A
// hardcoded seed makes it generate the same cases on every run for ever: it
// hunts once, finds whatever it finds, and then confirms that same finding a
// thousand times while looking like it is still searching.
//
// So the seed is random per run and printed, and GIRO_TEST_SEED replays one.
// That keeps the reason fixed seeds are tempting -- a failure you cannot
// reproduce is not much use at three in the morning -- without paying for it
// with a test that has stopped exploring.
func seededRand(t *testing.T) *rand.Rand {
	t.Helper()

	seed := uint64(time.Now().UnixNano())
	if s := os.Getenv("GIRO_TEST_SEED"); s != "" {
		parsed, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			t.Fatalf("GIRO_TEST_SEED=%q is not a number", s)
		}
		seed = parsed
	}

	// logged unconditionally rather than on failure, because go test only
	// shows output for tests that fail or run with -v, which is exactly when
	// it is wanted and never in the way otherwise.
	t.Logf("seed %d: replay with GIRO_TEST_SEED=%d", seed, seed)

	// two words from one seed, so a single number names the whole sequence.
	return rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
}
