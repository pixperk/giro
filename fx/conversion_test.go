package fx

import (
	"errors"
	"math/big"
	"testing"

	"github.com/pixperk/giro/ledger"
)

func TestConversionArithmetic(t *testing.T) {
	tests := []struct {
		why       string
		from, to  ledger.Asset
		amount    string
		rate      string
		want      string
		wantExact bool
	}{
		{
			why:  "the real off ramp: 100,000 USDT at 0.99960 is $99,960.00",
			from: "USDT/6", to: "USD/2",
			amount: "100000000000", rate: "0.99960",
			want: "9996000", wantExact: true,
		},
		{
			why:  "above par, which happens and is profit rather than an error",
			from: "USDT/6", to: "USD/2",
			amount: "100000000000", rate: "1.00020",
			want: "10002000", wantExact: true,
		},
		{
			why:  "same scale both sides, so only the rate applies",
			from: "USDT/6", to: "USDC/6",
			amount: "100000000000", rate: "0.9998",
			want: "99980000000", wantExact: true,
		},
		{
			why:  "scale up rather than down",
			from: "USD/2", to: "USDT/6",
			amount: "10000000", rate: "1.0",
			want: "100000000000", wantExact: true,
		},
		{
			why:  "par, so the answer is the scale shift alone",
			from: "USDT/6", to: "USD/2",
			amount: "1000000", rate: "1",
			want: "100", wantExact: true,
		},
		{
			why:  "a fraction of a cent, truncated rather than rounded up",
			from: "USDT/6", to: "USD/2",
			amount: "42317482915", rate: "1",
			want: "4231748", wantExact: false,
		},
		{
			why:  "truncation never invents a unit that was not there",
			from: "USDT/6", to: "USD/2",
			amount: "9999", rate: "1",
			want: "0", wantExact: false,
		},
		{
			why:  "a rate with more places than either scale",
			from: "USDT/6", to: "USD/2",
			amount: "100000000000", rate: "0.999612345",
			want: "9996123", wantExact: false,
		},
		{
			why:  "an amount past int64, which is two units of an 18 decimal token",
			from: "TOKEN/18", to: "USD/2",
			amount: "20000000000000000000", rate: "2",
			want: "4000", wantExact: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.why, func(t *testing.T) {
			amount, _ := new(big.Int).SetString(tc.amount, 10)
			c := Conversion{From: tc.from, To: tc.to, Amount: amount, Rate: tc.rate}

			got, exact, err := c.Receives()
			if err != nil {
				t.Fatal(err)
			}
			if got.String() != tc.want {
				t.Errorf("receives = %s, want %s", got, tc.want)
			}
			if exact != tc.wantExact {
				t.Errorf("exact = %v, want %v", exact, tc.wantExact)
			}
		})
	}
}

// The rate is a decimal quantity and binary floating point cannot hold most
// decimals exactly. 0.99960 as a float64 is not 0.99960, and on a hundred
// thousand dollars the difference is real money.
func TestRatesAreExactDecimalsNotFloats(t *testing.T) {
	amount, _ := new(big.Int).SetString("100000000000", 10)
	c := Conversion{From: "USDT/6", To: "USD/2", Amount: amount, Rate: "0.1"}

	got, exact, err := c.Receives()
	if err != nil {
		t.Fatal(err)
	}
	// 0.1 has no exact binary representation. done in float64 this drifts.
	if got.String() != "1000000" || !exact {
		t.Errorf("receives = %s exact=%v, want 1000000 exact", got, exact)
	}
}

func TestBadRatesAreRefused(t *testing.T) {
	for _, rate := range []string{"", "0", "-1", "abc", "1e-3", "2/3", "1 2", " 1"} {
		t.Run("rate "+rate, func(t *testing.T) {
			c := Conversion{From: "USDT/6", To: "USD/2", Amount: big.NewInt(1000), Rate: rate}
			if _, _, err := c.Receives(); !errors.Is(err, ErrInvalidRate) {
				t.Errorf("err = %v, want ErrInvalidRate", err)
			}
		})
	}
}

// The point of the type: what arrives is computed, never given, so a caller
// cannot state a rate and then post amounts that mean a different one.
func TestPostingsDeriveTheReceivedAmount(t *testing.T) {
	amount, _ := new(big.Int).SetString("100000000000", 10)
	c := Conversion{
		Seller: "treasury:usdt", SoldTo: "external:lp:kraken:USDT",
		BoughtFrom: "external:lp:kraken:USD", Buyer: "ops:usd",
		From: "USDT/6", To: "USD/2", Amount: amount, Rate: "0.99960",
	}

	p, err := c.Postings()
	if err != nil {
		t.Fatal(err)
	}
	if len(p) != 2 {
		t.Fatalf("%d postings, want 2", len(p))
	}
	// sold first: the thing given, then the thing received
	if p[0].Asset != "USDT/6" || p[0].Source != "treasury:usdt" || p[0].Destination != "external:lp:kraken:USDT" {
		t.Errorf("sold side = %+v", p[0])
	}
	if p[1].Asset != "USD/2" || p[1].Source != "external:lp:kraken:USD" || p[1].Destination != "ops:usd" {
		t.Errorf("bought side = %+v", p[1])
	}
	if p[1].Amount.String() != "9996000" {
		t.Errorf("received = %s, want 9996000", p[1].Amount)
	}
	if _, err := p.Validate(); err != nil {
		t.Errorf("the postings it builds are not valid: %v", err)
	}
}

func TestAConversionNeedsTwoAssets(t *testing.T) {
	c := Conversion{From: "USD/2", To: "USD/2", Amount: big.NewInt(100), Rate: "1"}
	if _, err := c.Postings(); !errors.Is(err, ErrSameAsset) {
		t.Errorf("err = %v, want ErrSameAsset", err)
	}
}

// A rate so small the trade receives nothing is refused rather than committing
// a posting of zero, which would record a sale that gave nothing back.
func TestAConversionThatReceivesNothingIsRefused(t *testing.T) {
	c := Conversion{
		Seller: "a", SoldTo: "v", BoughtFrom: "v", Buyer: "b",
		From: "USDT/6", To: "USD/2", Amount: big.NewInt(1), Rate: "0.0001",
	}
	if _, err := c.Postings(); !errors.Is(err, ledger.ErrInvalidAmount) {
		t.Errorf("err = %v, want a refusal", err)
	}
}

func TestCheckConversionCatchesAMisstatedRate(t *testing.T) {
	amount, _ := new(big.Int).SetString("100000000000", 10)
	sound := Conversion{
		Seller: "treasury:usdt", SoldTo: "external:lp:kraken:USDT",
		BoughtFrom: "external:lp:kraken:USD", Buyer: "ops:usd",
		From: "USDT/6", To: "USD/2", Amount: amount, Rate: "0.99960",
	}
	p, err := sound.Postings()
	if err != nil {
		t.Fatal(err)
	}

	tx := &ledger.Transaction{ID: 1, Postings: p, Metadata: sound.Metadata(nil)}
	if err := Check(tx, 1); err != nil {
		t.Errorf("a sound conversion was reported: %v", err)
	}

	// the fat finger: a rate stated with one digit missing, amounts unchanged.
	// both sides still conserve and the book still balances.
	tx.Metadata[ConversionRateKey] = "0.9960"
	err = Check(tx, 1)
	if !errors.Is(err, ErrConversionRounding) {
		t.Fatalf("err = %v, want the disagreement reported", err)
	}
	t.Logf("reported: %v", err)
}

// A transaction that claims nothing is not checked. The convention is opt in,
// so a ledger that never records a rate is not suddenly full of findings.
func TestATransactionWithoutARateIsNotAConversion(t *testing.T) {
	tx := &ledger.Transaction{ID: 1, Postings: ledger.Postings{
		{Source: "a", Destination: "b", Asset: "USD/2", Amount: big.NewInt(100)},
	}}
	if err := Check(tx, 1); err != nil {
		t.Errorf("a plain transfer was checked as a conversion: %v", err)
	}
}

// Truncation is expected and must not read as a finding. A scale 6 asset
// converting into a scale 2 one lands between cents almost every time.
func TestRoundingWithinToleranceIsNotAFinding(t *testing.T) {
	amount, _ := new(big.Int).SetString("42317482915", 10)
	c := Conversion{
		Seller: "a", SoldTo: "v", BoughtFrom: "v", Buyer: "b",
		From: "USDT/6", To: "USD/2", Amount: amount, Rate: "1",
	}
	p, err := c.Postings()
	if err != nil {
		t.Fatal(err)
	}
	tx := &ledger.Transaction{ID: 1, Postings: p, Metadata: c.Metadata(nil)}
	if err := Check(tx, 1); err != nil {
		t.Errorf("expected truncation was reported: %v", err)
	}

	// but a cent in the other direction is not rounding
	p[1].Amount.Add(p[1].Amount, big.NewInt(2))
	if err := Check(tx, 1); !errors.Is(err, ErrConversionRounding) {
		t.Errorf("err = %v, want two units past tolerance to be reported", err)
	}
}

// Stating a rate without naming both assets is a malformed claim rather than
// an absent one, and reads as a finding.
func TestAnIncompleteConversionClaimIsAFinding(t *testing.T) {
	tx := &ledger.Transaction{ID: 7, Postings: ledger.Postings{
		{Source: "a", Destination: "b", Asset: "USD/2", Amount: big.NewInt(100)},
	}, Metadata: ledger.Metadata{ConversionRateKey: "0.9996"}}

	if err := Check(tx, 1); !errors.Is(err, ErrConversionRounding) {
		t.Errorf("err = %v, want a finding for a rate with no assets", err)
	}
}
