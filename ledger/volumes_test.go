package ledger

import (
	"errors"
	"fmt"
	"math/big"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestVolumesBalance(t *testing.T) {
	tests := []struct {
		why  string
		v    Volumes
		want int64
	}{
		{"input minus output", Volumes{n(10000), n(3000)}, 7000},
		{"can go negative, world does", Volumes{n(0), n(15000)}, -15000},
		{"zero value is usable, no row means nothing flowed", Volumes{}, 0},
		{"nil output", Volumes{Input: n(500)}, 500},
		{"nil input", Volumes{Output: n(500)}, -500},
		{"constructor zeroes both", NewVolumes(), 0},
	}

	for _, tc := range tests {
		t.Run(tc.why, func(t *testing.T) {
			if got := tc.v.Balance(); got.Cmp(n(tc.want)) != 0 {
				t.Errorf("Balance() = %s, want %d", got, tc.want)
			}
		})
	}
}

func TestVolumeUpdates(t *testing.T) {
	t.Run("an account in several postings collapses to one entry", func(t *testing.T) {
		// treasury receives once and pays out three times
		got, _ := Postings{
			{Source: "world", Destination: "treasury", Asset: "USD/2", Amount: n(10000)},
			{Source: "treasury", Destination: "users:alice", Asset: "USD/2", Amount: n(6000)},
			{Source: "treasury", Destination: "users:bob", Asset: "USD/2", Amount: n(3000)},
			{Source: "treasury", Destination: "fees:platform", Asset: "USD/2", Amount: n(1000)},
		}.VolumeUpdates()

		assertUpdates(t, got, []VolumeUpdate{
			{"fees:platform", "USD/2", n(1000), n(0)},
			{"treasury", "USD/2", n(10000), n(10000)},
			{"users:alice", "USD/2", n(6000), n(0)},
			{"users:bob", "USD/2", n(3000), n(0)},
			{"world", "USD/2", n(0), n(10000)},
		})
	})

	// source and destination resolve to the same entry, so both counters move.
	// balance is unchanged but the row remembers the money went round.
	t.Run("self posting moves both counters", func(t *testing.T) {
		got, _ := Postings{{Source: "users:alice", Destination: "users:alice", Asset: "USD/2", Amount: n(500)}}.VolumeUpdates()
		assertUpdates(t, got, []VolumeUpdate{
			{"users:alice", "USD/2", n(500), n(500)},
		})
	})

	// the key is (account, asset), so one account with two assets is two rows
	// and the amounts are never combined.
	t.Run("assets never mix", func(t *testing.T) {
		got, _ := Postings{
			{Source: "world", Destination: "users:alice", Asset: "USD/2", Amount: n(10000)},
			{Source: "world", Destination: "users:alice", Asset: "EUR/2", Amount: n(9200)},
		}.VolumeUpdates()

		assertUpdates(t, got, []VolumeUpdate{
			{"users:alice", "EUR/2", n(9200), n(0)},
			{"users:alice", "USD/2", n(10000), n(0)},
			{"world", "EUR/2", n(0), n(9200)},
			{"world", "USD/2", n(0), n(10000)},
		})
	})

	t.Run("sorted by account then asset", func(t *testing.T) {
		got, _ := Postings{
			{Source: "zzz", Destination: "aaa", Asset: "USD/2", Amount: n(1)},
			{Source: "mmm", Destination: "aaa", Asset: "EUR/2", Amount: n(1)},
			{Source: "aaa", Destination: "zzz", Asset: "BTC/8", Amount: n(1)},
		}.VolumeUpdates()

		want := []string{
			"aaa BTC/8", "aaa EUR/2", "aaa USD/2",
			"mmm EUR/2",
			"zzz BTC/8", "zzz USD/2",
		}
		for i, u := range got {
			if key := string(u.Account) + " " + string(u.Asset); key != want[i] {
				t.Errorf("updates[%d] = %q, want %q", i, key, want[i])
			}
		}
	})

	t.Run("empty", func(t *testing.T) {
		if got, _ := (Postings{}).VolumeUpdates(); len(got) != 0 {
			t.Errorf("got %d updates, want 0", len(got))
		}
	})

	// This used to assert a panic. A library that panics takes its host
	// process down, and giro is meant to be embedded, so a malformed posting
	// is now an error the caller can handle.
	t.Run("nil amount is an error, not a panic", func(t *testing.T) {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("panicked instead of returning an error: %v", r)
			}
		}()
		got, err := Postings{{Source: "A", Destination: "B", Asset: "USD/2", Amount: nil}}.VolumeUpdates()
		if !errors.Is(err, ErrNilAmount) {
			t.Errorf("err = %v, want ErrNilAmount", err)
		}
		if got != nil {
			t.Errorf("returned %v alongside the error", got)
		}
	})
}

// go randomises map iteration, so without the sort this ordering would vary
// run to run. that would pass every single threaded test and then deadlock
// under concurrency, which is why it gets its own test.
func TestVolumeUpdatesOrderIsDeterministic(t *testing.T) {
	p := Postings{
		{Source: "world", Destination: "treasury:incoming", Asset: "USD/2", Amount: n(50000)},
		{Source: "treasury:incoming", Destination: "escrow:order:1001", Asset: "USD/2", Amount: n(20000)},
		{Source: "escrow:order:1001", Destination: "merchants:acme", Asset: "USD/2", Amount: n(18000)},
		{Source: "escrow:order:1001", Destination: "fees:platform", Asset: "USD/2", Amount: n(2000)},
		{Source: "world", Destination: "merchants:acme", Asset: "EUR/2", Amount: n(4500)},
	}

	first := orderOf(mustUpdates(p))
	for i := range 200 {
		if got := orderOf(mustUpdates(p)); got != first {
			t.Fatalf("run %d produced a different order\n got: %s\nwant: %s", i, got, first)
		}
	}
}

// the invariant everything else rests on: a transaction can move value around
// but can never create or destroy it. holds per asset, for any postings.
func TestVolumeUpdatesConserveValue(t *testing.T) {
	accounts := []Address{"world", "treasury", "users:alice", "users:bob", "fees:platform", "escrow:1"}
	assets := []Asset{"USD/2", "EUR/2", "BTC/8", "POINTS"}
	rng := seededRand(t)

	for range 2000 {
		p := make(Postings, rng.IntN(12)+1)
		for i := range p {
			p[i] = Posting{
				Source:      accounts[rng.IntN(len(accounts))],
				Destination: accounts[rng.IntN(len(accounts))],
				Asset:       assets[rng.IntN(len(assets))],
				Amount:      big.NewInt(rng.Int64N(1_000_000)),
			}
		}

		drift := map[Asset]*big.Int{}
		us, _ := p.VolumeUpdates()
		for _, u := range us {
			if drift[u.Asset] == nil {
				drift[u.Asset] = new(big.Int)
			}
			drift[u.Asset].Add(drift[u.Asset], u.Input)
			drift[u.Asset].Sub(drift[u.Asset], u.Output)
		}
		for asset, d := range drift {
			if d.Sign() != 0 {
				t.Fatalf("asset %s drifted by %s for postings %v", asset, d, p)
			}
		}
	}
}

// every amount that leaves a source must arrive at a destination. checks the
// direction of the counters, which conservation alone would not catch if both
// were swapped.
func TestVolumeUpdatesDirection(t *testing.T) {
	got, _ := Postings{{Source: "payer", Destination: "payee", Asset: "USD/2", Amount: n(100)}}.VolumeUpdates()

	assertUpdates(t, got, []VolumeUpdate{
		{"payee", "USD/2", n(100), n(0)}, // arrived, so input
		{"payer", "USD/2", n(0), n(100)}, // left, so output
	})
}

func orderOf(updates []VolumeUpdate) string {
	keys := make([]string, len(updates))
	for i, u := range updates {
		keys[i] = string(u.Account) + "/" + string(u.Asset)
	}
	return strings.Join(keys, ", ")
}

func assertUpdates(t *testing.T, got, want []VolumeUpdate) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d updates, want %d\ngot:  %s\nwant: %s",
			len(got), len(want), formatUpdates(got), formatUpdates(want))
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.Account != w.Account || g.Asset != w.Asset ||
			g.Input.Cmp(w.Input) != 0 || g.Output.Cmp(w.Output) != 0 {
			t.Errorf("updates[%d]:\n got:  %s\n want: %s", i, formatUpdate(g), formatUpdate(w))
		}
	}
}

func formatUpdate(u VolumeUpdate) string {
	return fmt.Sprintf("%s %s in=%s out=%s", u.Account, u.Asset, u.Input, u.Output)
}

func formatUpdates(us []VolumeUpdate) string {
	parts := make([]string, len(us))
	for i, u := range us {
		parts[i] = formatUpdate(u)
	}
	return "[" + strings.Join(parts, " | ") + "]"
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

// mustUpdates is for the tests whose subject is the ordering rather than the
// error path.
func mustUpdates(p Postings) []VolumeUpdate {
	u, err := p.VolumeUpdates()
	if err != nil {
		panic(err)
	}
	return u
}
