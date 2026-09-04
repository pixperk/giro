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
			p:       Postings{{Source: "world", Destination: "users:alice", Asset: "USD/2", Amount: n(10000)}},
			wantIdx: -1,
		},
		{
			why:     "zero amount is allowed",
			p:       Postings{{Source: "world", Destination: "users:alice", Asset: "USD/2", Amount: n(0)}},
			wantIdx: -1,
		},
		{
			why:     "self posting is allowed",
			p:       Postings{{Source: "users:alice", Destination: "users:alice", Asset: "USD/2", Amount: n(500)}},
			wantIdx: -1,
		},
		{
			why:     "bad source",
			p:       Postings{{Source: "wor ld", Destination: "users:alice", Asset: "USD/2", Amount: n(1)}},
			wantIdx: 0,
			wantErr: ErrInvalidSourceAddress,
		},
		{
			why:     "bad destination",
			p:       Postings{{Source: "world", Destination: "a::b", Asset: "USD/2", Amount: n(1)}},
			wantIdx: 0,
			wantErr: ErrInvalidDestinationAddress,
		},
		{
			why:     "bad asset",
			p:       Postings{{Source: "world", Destination: "users:alice", Asset: "usd", Amount: n(1)}},
			wantIdx: 0,
			wantErr: ErrInvalidAsset,
		},
		{
			why:     "negative amount",
			p:       Postings{{Source: "world", Destination: "users:alice", Asset: "USD/2", Amount: n(-1)}},
			wantIdx: 0,
			wantErr: ErrInvalidAmount,
		},
		{
			why:     "nil amount",
			p:       Postings{{Source: "world", Destination: "users:alice", Asset: "USD/2", Amount: nil}},
			wantIdx: 0,
			wantErr: ErrInvalidAmount,
		},
		{
			why:     "a 99 digit amount is fine, the cap is a size guard not a limit",
			p:       Postings{{Source: "world", Destination: "users:alice", Asset: "USD/2", Amount: digits(99)}},
			wantIdx: -1,
		},
		{
			why:     "an absurd amount is rejected before it reaches postgres",
			p:       Postings{{Source: "world", Destination: "users:alice", Asset: "USD/2", Amount: digits(500)}},
			wantIdx: 0,
			wantErr: ErrAmountTooLarge,
		},
		{
			why: "reports the index of the first bad posting, not the first posting",
			p: Postings{
				{Source: "world", Destination: "users:alice", Asset: "USD/2", Amount: n(100)},
				{Source: "users:alice", Destination: "users:bob", Asset: "USD/2", Amount: n(50)},
				{Source: "users:bob", Destination: "users:carol", Asset: "USD/2", Amount: n(-1)},
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
		got, _ := Postings{{Source: "users:alice", Destination: "users:bob", Asset: "USD/2", Amount: n(3000)}}.Reverse()
		want := Postings{{Source: "users:bob", Destination: "users:alice", Asset: "USD/2", Amount: n(3000)}}
		assertPostings(t, got, want)
	})

	// the case that matters. keeping the original order would pay A back out of
	// B before C has returned anything, so B dips negative mid transaction.
	t.Run("chain reverses order as well as sides", func(t *testing.T) {
		got, _ := Postings{
			{Source: "A", Destination: "B", Asset: "USD/2", Amount: n(100)},
			{Source: "B", Destination: "C", Asset: "USD/2", Amount: n(100)},
		}.Reverse()
		want := Postings{
			{Source: "C", Destination: "B", Asset: "USD/2", Amount: n(100)},
			{Source: "B", Destination: "A", Asset: "USD/2", Amount: n(100)},
		}
		assertPostings(t, got, want)
	})

	t.Run("reversing twice is the identity", func(t *testing.T) {
		original := Postings{
			{Source: "A", Destination: "B", Asset: "USD/2", Amount: n(100)},
			{Source: "B", Destination: "C", Asset: "USD/2", Amount: n(60)},
			{Source: "C", Destination: "D", Asset: "EUR/2", Amount: n(40)},
		}
		assertPostings(t, mustReverse(mustReverse(original)), original)
	})

	// a reversal sharing pointers with the original would let a later mutation
	// rewrite history.
	t.Run("amounts are copied, not aliased", func(t *testing.T) {
		amount := n(100)
		original := Postings{{Source: "A", Destination: "B", Asset: "USD/2", Amount: amount}}
		reversed, _ := original.Reverse()

		amount.SetInt64(999)

		if reversed[0].Amount.Cmp(n(100)) != 0 {
			t.Errorf("reversed amount changed with the original: got %s, want 100", reversed[0].Amount)
		}
	})

	t.Run("empty", func(t *testing.T) {
		if got, _ := (Postings{}).Reverse(); len(got) != 0 {
			t.Errorf("got %v, want empty", got)
		}
	})

	// This used to assert a panic. giro is meant to be embedded, and a library
	// that panics takes its host process down, so a malformed posting is now an
	// error the caller can handle.
	t.Run("nil amount is an error rather than a panic or a zero", func(t *testing.T) {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("panicked instead of returning an error: %v", r)
			}
		}()
		got, err := Postings{{Source: "A", Destination: "B", Asset: "USD/2", Amount: nil}}.Reverse()
		if !errors.Is(err, ErrNilAmount) {
			t.Errorf("err = %v, want ErrNilAmount", err)
		}
		if got != nil {
			t.Errorf("returned %v alongside the error", got)
		}
	})
}

// a typed *big.Int field round trips exactly. decoding into any would give
// 1e+32 here, so this pins the property against a future refactor.
func TestPostingJSONPrecision(t *testing.T) {
	const huge = "100000000000000000000000000000001"

	amount, _ := new(big.Int).SetString(huge, 10)
	raw, err := json.Marshal(Posting{Source: "world", Destination: "users:alice", Asset: "USD/2", Amount: amount})
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

// A library that panics takes its host process down. giro is meant to be
// embedded, so a malformed posting reaching these has to be an error the
// caller can handle rather than a crash they cannot.
func TestAMalformedPostingIsAnErrorRatherThanAPanic(t *testing.T) {
	bad := Postings{
		{Source: "world", Destination: "users:alice", Asset: "USD/2", Amount: big.NewInt(1)},
		{Source: "world", Destination: "users:bob", Asset: "USD/2", Amount: nil},
	}

	t.Run("VolumeUpdates", func(t *testing.T) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panicked instead of returning an error: %v", r)
			}
		}()
		got, err := bad.VolumeUpdates()
		if !errors.Is(err, ErrNilAmount) {
			t.Fatalf("err = %v, want ErrNilAmount", err)
		}
		if got != nil {
			t.Errorf("returned %v alongside the error", got)
		}
		if !strings.Contains(err.Error(), "postings[1]") {
			t.Errorf("err = %v, want it to name which posting", err)
		}
	})

	t.Run("Reverse", func(t *testing.T) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panicked instead of returning an error: %v", r)
			}
		}()
		got, err := bad.Reverse()
		if !errors.Is(err, ErrNilAmount) {
			t.Fatalf("err = %v, want ErrNilAmount", err)
		}
		if got != nil {
			t.Errorf("returned %v alongside the error", got)
		}
	})

	// and the well-formed case still works, because an error path that also
	// broke the happy path would be a poor trade
	good := Postings{{Source: "world", Destination: "users:alice", Asset: "USD/2", Amount: big.NewInt(5)}}
	if _, err := good.VolumeUpdates(); err != nil {
		t.Errorf("valid postings rejected: %v", err)
	}
	rev, err := good.Reverse()
	if err != nil {
		t.Errorf("valid postings rejected: %v", err)
	}
	if len(rev) != 1 || rev[0].Source != "users:alice" {
		t.Errorf("Reverse returned %v", rev)
	}
}

// mustReverse is for the tests whose subject is the reversal itself.
func mustReverse(p Postings) Postings {
	r, err := p.Reverse()
	if err != nil {
		panic(err)
	}
	return r
}
