// Package fx converts one asset into another at a stated rate.
//
// It sits above the ledger rather than inside it, and the reason is a decision
// the ledger makes about itself: assets never mix, conservation is checked per
// asset, and the engine has no idea that the two sides of a trade are related.
// It has no opinion on whether a price was fair, because that is a pricing
// question and it belongs upstream.
//
// This is upstream. It builds postings the ledger will accept and records what
// it did in metadata the ledger stores without reading, which keeps both
// positions intact: the engine knows nothing about rates, and a caller that
// does no trading never imports this.
package fx

import (
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/pixperk/giro/ledger"
)

// A conversion is two postings in one transaction: one asset leaves, another
// arrives. Conservation is checked per asset, so the ledger balances each side
// on its own and never compares them. Nothing relates the amounts.
//
// For a business whose margin is the rate, that leaves the one number that
// matters as the one number nothing checks.
//
// What follows is careful about which question it answers. Whether 0.99960 was
// a fair price is a pricing question and belongs upstream: the ledger has no
// opinion and should not grow one. Whether the amounts recorded match the rate
// recorded beside them is arithmetic, and a book that disagrees with its own
// stated rate is wrong regardless of what the market did.
//
// So a conversion declares its rate in metadata, and the amounts are derived
// from it rather than typed alongside it. A caller cannot disagree with itself
// about a number it only supplied once.

// the keys a conversion writes. namespaced like every other reserved key, so a
// caller's own metadata cannot collide with them.
const (
	ConversionRateKey = "giro/conversion.rate"
	ConversionFromKey = "giro/conversion.from"
	ConversionToKey   = "giro/conversion.to"
)

var (
	ErrInvalidRate        = errors.New("invalid rate")
	ErrSameAsset          = errors.New("a conversion needs two different assets")
	ErrConversionRounding = errors.New("conversion does not match its rate")
)

// Conversion describes one asset being exchanged for another at a stated rate.
//
// Amount is what leaves, in From's minor units. What arrives is computed, never
// given, which is the whole point: two numbers that must agree are one number
// that cannot disagree.
//
// Four accounts rather than three, because a conversion is genuinely two
// postings and the counterparty is not one account. A boundary account stands
// for one counterparty in one asset, so selling USDT to Kraken and being paid
// dollars by Kraken touches external:lp:kraken:USDT and
// external:lp:kraken:USD. Collapsing them would make each one stop being a
// balance comparable against a specific statement line, which is the reason
// they are split at all.
type Conversion struct {
	// the asset given up: it leaves Seller and arrives at SoldTo.
	From   ledger.Asset
	Seller ledger.Address
	SoldTo ledger.Address

	// the asset received: it leaves BoughtFrom and arrives at Buyer.
	To         ledger.Asset
	BoughtFrom ledger.Address
	Buyer      ledger.Address

	Amount *big.Int

	// decimal, as written: "0.99960". A string rather than a float, because a
	// rate is a decimal quantity and binary floating point cannot hold most of
	// them exactly.
	Rate string
}

// Receives reports what arrives, in To's minor units, and whether the rate
// divides exactly.
//
// The scales are part of the answer. An amount is in minor units, so
// converting between assets of different scale is not just a multiplication:
// 100000000000 of USDT/6 at 0.99960 is 9996000 of USD/2, and the factor of
// 10^(2-6) is doing as much work as the rate.
//
//	out = in * rate * 10^(toScale - fromScale)
//
// exact is false when that product has a fractional part, which is the normal
// case rather than an error: USDT is scale 6 and USD is scale 2, so most
// conversions land between cents. The fraction is truncated, never rounded up,
// so a conversion can never manufacture a unit that was not there. What it
// leaves behind is dust, and dust is a real amount that belongs in an account
// rather than in the difference between two numbers.
func (c Conversion) Receives() (out *big.Int, exact bool, err error) {
	rate, err := parseRate(c.Rate)
	if err != nil {
		return nil, false, err
	}
	fromScale, ok := c.From.Scale()
	if !ok {
		return nil, false, fmt.Errorf("%w: %q", ledger.ErrInvalidAsset, c.From)
	}
	toScale, ok := c.To.Scale()
	if !ok {
		return nil, false, fmt.Errorf("%w: %q", ledger.ErrInvalidAsset, c.To)
	}
	if c.Amount == nil || c.Amount.Sign() <= 0 {
		return nil, false, ledger.ErrInvalidAmount
	}

	// exact rational arithmetic throughout. a rate is a decimal and a scale
	// difference is a power of ten, and doing either in floating point would
	// lose exactly the fractions this is here to account for.
	value := new(big.Rat).SetInt(c.Amount)
	value.Mul(value, rate)

	shift := toScale - fromScale
	ten := big.NewRat(10, 1)
	for range abs(shift) {
		if shift > 0 {
			value.Mul(value, ten)
		} else {
			value.Quo(value, ten)
		}
	}

	out = new(big.Int).Quo(value.Num(), value.Denom())
	return out, value.IsInt(), nil
}

// Postings builds the two sides, in the order they must commit.
//
// The sold asset leaves first. Order matters when the same account appears on
// both sides, and it is also the order that reads correctly in a log: the
// thing given, then the thing received.
func (c Conversion) Postings() (ledger.Postings, error) {
	if c.From == c.To {
		return nil, fmt.Errorf("%w: both are %s", ErrSameAsset, c.From)
	}
	received, _, err := c.Receives()
	if err != nil {
		return nil, err
	}
	if received.Sign() <= 0 {
		return nil, fmt.Errorf("%w: %s at %s receives nothing", ledger.ErrInvalidAmount, c.Amount, c.Rate)
	}

	return ledger.Postings{
		{Source: c.Seller, Destination: c.SoldTo, Asset: c.From, Amount: new(big.Int).Set(c.Amount)},
		{Source: c.BoughtFrom, Destination: c.Buyer, Asset: c.To, Amount: received},
	}, nil
}

// Metadata records the rate so the postings can be checked against it later.
//
// Merged into whatever else the caller is recording rather than replacing it,
// because a conversion is usually part of something larger that has its own
// identifiers.
func (c Conversion) Metadata(into ledger.Metadata) ledger.Metadata {
	if into == nil {
		into = ledger.Metadata{}
	}
	into[ConversionRateKey] = c.Rate
	into[ConversionFromKey] = string(c.From)
	into[ConversionToKey] = string(c.To)
	return into
}

// Check recomputes what a transaction claiming a rate should have
// moved, and reports a disagreement.
//
// tolerance is in minor units of the received asset. See DefaultTolerance.
func Check(t *ledger.Transaction, tolerance int64) error {
	rate, ok := t.Metadata[ConversionRateKey]
	if !ok {
		return nil // not a conversion, nothing claimed, nothing to check
	}
	from := ledger.Asset(t.Metadata[ConversionFromKey])
	to := ledger.Asset(t.Metadata[ConversionToKey])
	if from == "" || to == "" {
		return fmt.Errorf("%w: transaction %d states a rate without naming both assets",
			ErrConversionRounding, t.ID)
	}

	sold, bought := new(big.Int), new(big.Int)
	for _, p := range t.Postings {
		switch p.Asset {
		case from:
			sold.Add(sold, p.Amount)
		case to:
			bought.Add(bought, p.Amount)
		}
	}
	if sold.Sign() == 0 || bought.Sign() == 0 {
		return fmt.Errorf("%w: transaction %d states %s -> %s and moves neither",
			ErrConversionRounding, t.ID, from, to)
	}

	want, _, err := Conversion{From: from, To: to, Amount: sold, Rate: rate}.Receives()
	if err != nil {
		return fmt.Errorf("transaction %d: %w", t.ID, err)
	}

	drift := new(big.Int).Sub(bought, want)
	if drift.CmpAbs(big.NewInt(tolerance)) > 0 {
		return fmt.Errorf("%w: transaction %d moved %s %s for %s %s, which at %s should be %s %s, out by %s",
			ErrConversionRounding, t.ID, sold, from, bought, to, rate, want, to, drift)
	}
	return nil
}

// a decimal string to an exact rational. big.Rat parses "0.99960" natively;
// this exists to refuse the things it would otherwise accept, because a rate
// arriving as "1e-3" or "2/3" is a sign that something upstream is not
// producing what it thinks it is.
func parseRate(s string) (*big.Rat, error) {
	if s == "" {
		return nil, fmt.Errorf("%w: empty", ErrInvalidRate)
	}
	if strings.ContainsAny(s, "eE/ ") {
		return nil, fmt.Errorf("%w: %q must be a plain decimal", ErrInvalidRate, s)
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrInvalidRate, s)
	}
	if r.Sign() <= 0 {
		return nil, fmt.Errorf("%w: %q is not positive", ErrInvalidRate, s)
	}
	return r, nil
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
