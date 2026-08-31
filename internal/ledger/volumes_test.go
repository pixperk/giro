package ledger

import (
	"fmt"
	"math/big"
	"math/rand/v2"
	"strings"
	"testing"
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
		got := Postings{
			{"world", "treasury", "USD/2", n(10000)},
			{"treasury", "users:alice", "USD/2", n(6000)},
			{"treasury", "users:bob", "USD/2", n(3000)},
			{"treasury", "fees:platform", "USD/2", n(1000)},
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
		got := Postings{{"users:alice", "users:alice", "USD/2", n(500)}}.VolumeUpdates()
		assertUpdates(t, got, []VolumeUpdate{
			{"users:alice", "USD/2", n(500), n(500)},
		})
	})

	// the key is (account, asset), so one account with two assets is two rows
	// and the amounts are never combined.
	t.Run("assets never mix", func(t *testing.T) {
		got := Postings{
			{"world", "users:alice", "USD/2", n(10000)},
			{"world", "users:alice", "EUR/2", n(9200)},
		}.VolumeUpdates()

		assertUpdates(t, got, []VolumeUpdate{
			{"users:alice", "EUR/2", n(9200), n(0)},
			{"users:alice", "USD/2", n(10000), n(0)},
			{"world", "EUR/2", n(0), n(9200)},
			{"world", "USD/2", n(0), n(10000)},
		})
	})

	t.Run("sorted by account then asset", func(t *testing.T) {
		got := Postings{
			{"zzz", "aaa", "USD/2", n(1)},
			{"mmm", "aaa", "EUR/2", n(1)},
			{"aaa", "zzz", "BTC/8", n(1)},
		}.VolumeUpdates()

		want := []string{
			"aaa BTC/8", "aaa EUR/2", "aaa USD/2",
			"mmm EUR/2",
			"zzz BTC/8", "zzz USD/2",
		}
		for i, u := range got {
			if key := u.Account + " " + u.Asset; key != want[i] {
				t.Errorf("updates[%d] = %q, want %q", i, key, want[i])
			}
		}
	})

	t.Run("empty", func(t *testing.T) {
		if got := (Postings{}).VolumeUpdates(); len(got) != 0 {
			t.Errorf("got %d updates, want 0", len(got))
		}
	})

	t.Run("panics on nil amount", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Error("expected a panic")
			}
		}()
		Postings{{"A", "B", "USD/2", nil}}.VolumeUpdates()
	})
}

// go randomises map iteration, so without the sort this ordering would vary
// run to run. that would pass every single threaded test and then deadlock
// under concurrency, which is why it gets its own test.
func TestVolumeUpdatesOrderIsDeterministic(t *testing.T) {
	p := Postings{
		{"world", "treasury:incoming", "USD/2", n(50000)},
		{"treasury:incoming", "escrow:order:1001", "USD/2", n(20000)},
		{"escrow:order:1001", "merchants:acme", "USD/2", n(18000)},
		{"escrow:order:1001", "fees:platform", "USD/2", n(2000)},
		{"world", "merchants:acme", "EUR/2", n(4500)},
	}

	first := orderOf(p.VolumeUpdates())
	for i := range 200 {
		if got := orderOf(p.VolumeUpdates()); got != first {
			t.Fatalf("run %d produced a different order\n got: %s\nwant: %s", i, got, first)
		}
	}
}

// the invariant everything else rests on: a transaction can move value around
// but can never create or destroy it. holds per asset, for any postings.
func TestVolumeUpdatesConserveValue(t *testing.T) {
	accounts := []string{"world", "treasury", "users:alice", "users:bob", "fees:platform", "escrow:1"}
	assets := []string{"USD/2", "EUR/2", "BTC/8", "POINTS"}
	rng := rand.New(rand.NewPCG(1, 2))

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

		drift := map[string]*big.Int{}
		for _, u := range p.VolumeUpdates() {
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
	got := Postings{{"payer", "payee", "USD/2", n(100)}}.VolumeUpdates()

	assertUpdates(t, got, []VolumeUpdate{
		{"payee", "USD/2", n(100), n(0)}, // arrived, so input
		{"payer", "USD/2", n(0), n(100)}, // left, so output
	})
}

func orderOf(updates []VolumeUpdate) string {
	keys := make([]string, len(updates))
	for i, u := range updates {
		keys[i] = u.Account + "/" + u.Asset
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
