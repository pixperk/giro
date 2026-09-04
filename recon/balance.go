package recon

import (
	"context"
	"fmt"
	"math/big"

	"github.com/pixperk/giro/ledger"
)

// Comparing a whole position rather than line by line.
//
// Matching pairs individual statement lines, and it can only pair the lines a
// source actually sent. If a source never mentions a movement at all, matching
// has nothing to notice: every line it did send matched, and the report is
// clean.
//
// A balance closes that. The counterparty says what it holds for us in total,
// we say what we hold against it, and the two are one number each. It catches
// the whole class of "something is missing" that line matching cannot see, and
// it costs one read.
//
// giro is unusually well placed for it. A boundary account per counterparty
// and asset -- external:lp:kraken:USD -- means our side of the comparison is
// already an account balance rather than a report to be assembled.

// BalanceSource is a Source that can also state its own position.
//
// Optional, and separate from Source on purpose: a chain can tell you what an
// address holds and a bank statement file cannot, and a source that cannot
// answer should not have to pretend. Match works without it; this is the
// stronger check available when the source supports it.
type BalanceSource interface {
	Source
	// Balance is what this counterparty says it holds for us, in the asset's
	// minor units. Positive means they hold it; a payable is negative.
	Balance(ctx context.Context, asset ledger.Asset) (*big.Int, error)
}

// BalanceComparison is one position, from both sides.
type BalanceComparison struct {
	Source  string
	Account ledger.Address
	Asset   ledger.Asset

	// Ours is the boundary account's balance, negated.
	//
	// A boundary account is the outside world's side of the book, so it holds
	// the mirror of our position: money we received from a counterparty leaves
	// their account here and its balance goes negative by what we hold. Negating
	// it puts both sides of this comparison in the same terms, which is the
	// only way the difference means anything.
	Ours   *big.Int
	Theirs *big.Int
	// Difference is Theirs minus Ours. Zero is agreement.
	Difference *big.Int
}

func (c BalanceComparison) Agrees() bool { return c.Difference.Sign() == 0 }

func (c BalanceComparison) Error() string {
	return fmt.Sprintf("%s says it holds %s %s for us and we say %s: out by %s",
		c.Source, c.Theirs, c.Asset, c.Ours, c.Difference)
}

// CompareBalance asks a source what it holds and compares it to the boundary
// account standing for it.
//
// Nothing is written. A disagreement here is not a break to be resolved by
// this layer: it is the fact that two organisations disagree about a position,
// and what to do about it is a person's decision.
func CompareBalance(
	ctx context.Context, db DB, ledgerName string,
	s BalanceSource, account ledger.Address, asset ledger.Asset,
) (BalanceComparison, error) {
	out := BalanceComparison{Source: s.ID(), Account: account, Asset: asset}

	theirs, err := s.Balance(ctx, asset)
	if err != nil {
		return out, fmt.Errorf("ask %s for its balance: %w", s.ID(), err)
	}
	if theirs == nil {
		return out, fmt.Errorf("%s reported no balance for %s", s.ID(), asset)
	}
	out.Theirs = theirs

	var balance string
	err = db.QueryRow(ctx, `
		select coalesce((select (input - output)::text from accounts_volumes
		                  where ledger = $1 and address = $2 and asset = $3), '0')`,
		ledgerName, account, asset).Scan(&balance)
	if err != nil {
		return out, fmt.Errorf("read %s: %w", account, err)
	}
	ours, ok := new(big.Int).SetString(balance, 10)
	if !ok {
		return out, fmt.Errorf("balance of %s is not an integer: %q", account, balance)
	}

	out.Ours = ours.Neg(ours)
	out.Difference = new(big.Int).Sub(out.Theirs, out.Ours)
	return out, nil
}
