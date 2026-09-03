package ledger

import (
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"testing"
)

// n is a terse *big.Int for tests.
func n(i int64) *big.Int { return big.NewInt(i) }

// digits returns a positive integer with exactly d decimal digits.
func digits(d int) *big.Int {
	v, ok := new(big.Int).SetString(strings.Repeat("9", d), 10)
	if !ok {
		panic("bad digit count")
	}
	return v
}

func TestPostingsValidate(t *testing.T) {
	tests := []struct {
		why     string
		p       Postings
		wantIdx int
		wantErr error
	}{
		{
			why:     "empty is valid, emptiness is the caller's problem",
			p:       Postings{},
			wantIdx: -1,
		},
		{
			why:     "well formed",
			p:       Postings{{"world", "users:alice", "USD/2", n(10000)}},
			wantIdx: -1,
		},
		{
			why:     "zero amount is allowed",
			p:       Postings{{"world", "users:alice", "USD/2", n(0)}},
			wantIdx: -1,
		},
		{
			why:     "self posting is allowed",
			p:       Postings{{"users:alice", "users:alice", "USD/2", n(500)}},
			wantIdx: -1,
		},
		{
			why:     "bad source",
			p:       Postings{{"wor ld", "users:alice", "USD/2", n(1)}},
			wantIdx: 0,
			wantErr: ErrInvalidSourceAddress,
		},
		{
			why:     "bad destination",
			p:       Postings{{"world", "a::b", "USD/2", n(1)}},
			wantIdx: 0,
			wantErr: ErrInvalidDestinationAddress,
		},
		{
			why:     "bad asset",
			p:       Postings{{"world", "users:alice", "usd", n(1)}},
			wantIdx: 0,
			wantErr: ErrInvalidAsset,
		},
		{
			why:     "negative amount",
			p:       Postings{{"world", "users:alice", "USD/2", n(-1)}},
			wantIdx: 0,
			wantErr: ErrInvalidAmount,
		},
		{
			why:     "nil amount",
			p:       Postings{{"world", "users:alice", "USD/2", nil}},
			wantIdx: 0,
			wantErr: ErrInvalidAmount,
		},
		{
			why:     "a 99 digit amount is fine, the cap is a size guard not a limit",
			p:       Postings{{"world", "users:alice", "USD/2", digits(99)}},
			wantIdx: -1,
		},
		{
			why:     "an absurd amount is rejected before it reaches postgres",
			p:       Postings{{"world", "users:alice", "USD/2", digits(500)}},
			wantIdx: 0,
			wantErr: ErrAmountTooLarge,
		},
		{
			why: "reports the index of the first bad posting, not the first posting",
			p: Postings{
				{"world", "users:alice", "USD/2", n(100)},
				{"users:alice", "users:bob", "USD/2", n(50)},
				{"users:bob", "users:carol", "USD/2", n(-1)},
			},
			wantIdx: 2,
			wantErr: ErrInvalidAmount,
		},
	}

	for _, tc := range tests {
		t.Run(tc.why, func(t *testing.T) {
			idx, err := tc.p.Validate()
			if idx != tc.wantIdx {
				t.Errorf("index = %d, want %d", idx, tc.wantIdx)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestPostingsReverse(t *testing.T) {
	t.Run("single posting swaps sides", func(t *testing.T) {
		got := Postings{{"users:alice", "users:bob", "USD/2", n(3000)}}.Reverse()
		want := Postings{{"users:bob", "users:alice", "USD/2", n(3000)}}
		assertPostings(t, got, want)
	})

	// the case that matters. keeping the original order would pay A back out of
	// B before C has returned anything, so B dips negative mid transaction.
	t.Run("chain reverses order as well as sides", func(t *testing.T) {
		got := Postings{
			{"A", "B", "USD/2", n(100)},
			{"B", "C", "USD/2", n(100)},
		}.Reverse()
		want := Postings{
			{"C", "B", "USD/2", n(100)},
			{"B", "A", "USD/2", n(100)},
		}
		assertPostings(t, got, want)
	})

	t.Run("reversing twice is the identity", func(t *testing.T) {
		original := Postings{
			{"A", "B", "USD/2", n(100)},
			{"B", "C", "USD/2", n(60)},
			{"C", "D", "EUR/2", n(40)},
		}
		assertPostings(t, original.Reverse().Reverse(), original)
	})

	// a reversal sharing pointers with the original would let a later mutation
	// rewrite history.
	t.Run("amounts are copied, not aliased", func(t *testing.T) {
		amount := n(100)
		original := Postings{{"A", "B", "USD/2", amount}}
		reversed := original.Reverse()

		amount.SetInt64(999)

		if reversed[0].Amount.Cmp(n(100)) != 0 {
			t.Errorf("reversed amount changed with the original: got %s, want 100", reversed[0].Amount)
		}
	})

	t.Run("empty", func(t *testing.T) {
		if got := (Postings{}).Reverse(); len(got) != 0 {
			t.Errorf("got %v, want empty", got)
		}
	})

	t.Run("panics on nil amount rather than treating it as zero", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Error("expected a panic")
			}
		}()
		Postings{{"A", "B", "USD/2", nil}}.Reverse()
	})
}

// a typed *big.Int field round trips exactly. decoding into any would give
// 1e+32 here, so this pins the property against a future refactor.
func TestPostingJSONPrecision(t *testing.T) {
	const huge = "100000000000000000000000000000001"

	amount, _ := new(big.Int).SetString(huge, 10)
	raw, err := json.Marshal(Posting{"world", "users:alice", "USD/2", amount})
	if err != nil {
		t.Fatal(err)
	}

	var back Posting
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.Amount.String() != huge {
		t.Errorf("amount = %s, want %s", back.Amount, huge)
	}
}

func assertPostings(t *testing.T, got, want Postings) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("length = %d, want %d\ngot:  %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.Source != w.Source || g.Destination != w.Destination ||
			g.Asset != w.Asset || g.Amount.Cmp(w.Amount) != 0 {
			t.Errorf("postings[%d] = {%s -> %s %s %s}, want {%s -> %s %s %s}",
				i, g.Source, g.Destination, g.Asset, g.Amount,
				w.Source, w.Destination, w.Asset, w.Amount)
		}
	}
}
