package ledger

import (
	"errors"
	"fmt"
	"math/big"
)

// world is the boundary of the ledger, standing for everything not tracked
// here. it is the only account allowed a negative balance, because otherwise
// the first deposit would have nowhere to come from and the ledger could never
// be started.
const WorldAccount = "world"

var (
	ErrInvalidSourceAddress      = errors.New("invalid source address")
	ErrInvalidDestinationAddress = errors.New("invalid destination address")
	ErrInvalidAsset              = errors.New("invalid asset")
	ErrInvalidAmount             = errors.New("invalid amount")
	ErrAmountTooLarge            = errors.New("amount too large")
)

// a size guard against unbounded input, not a business rule. postgres numeric
// happily stores tens of thousands of digits and does slow arithmetic on them.
// 100 digits sits above anything real (uint256 max is 78 digits, total ether
// supply in wei is 27) and far below the size where numeric gets expensive.
// per transfer limits are policy and belong upstream.
const MaxAmountDigits = 100

// 100 decimal digits needs at most 333 bits. counting bits avoids the string
// allocation, and being a fraction of a digit loose is irrelevant for a guard
// whose job is rejecting absurd input.
const maxAmountBits = 333

// a posting is one movement of value. naming both sides in a single record is
// what makes an unbalanced entry impossible to write down, rather than an
// error we have to detect later.
//
// Amount is always positive. direction is already carried by which field an
// account sits in, so a sign would be a second way of saying the same thing.
type Posting struct {
	Source      string   `json:"source"`
	Destination string   `json:"destination"`
	Asset       string   `json:"asset"`
	Amount      *big.Int `json:"amount"`
}

// a transaction is an ordered list of postings applied atomically. the order
// matters: money can flow through an account within one transaction.
type Postings []Posting

// returns the failing index and error for the first invalid posting,
// or (-1, nil) if all are valid.
func (p Postings) Validate() (int, error) {
	for i, posting := range p {
		if !ValidateAddress(posting.Source) {
			return i, ErrInvalidSourceAddress
		}
		if !ValidateAddress(posting.Destination) {
			return i, ErrInvalidDestinationAddress
		}
		if !ValidateAsset(posting.Asset) {
			return i, ErrInvalidAsset
		}
		if posting.Amount == nil || posting.Amount.Sign() < 0 {
			return i, ErrInvalidAmount
		}
		if posting.Amount.BitLen() > maxAmountBits {
			return i, ErrAmountTooLarge
		}
	}
	return -1, nil
}

// swaps both sides of every posting and reverses their order, so A->B, B->C
// becomes C->B, B->A. the order reversal matters: keep the original order and
// B pays A before C has paid B, so B dips negative mid transaction and a
// reversal that should succeed fails its balance check.
//
// panics on a nil Amount rather than treating it as zero. a missing amount is
// a malformed request, not a transfer of nothing, and coercing it would commit
// a no-op that looks like success.
func (p Postings) Reverse() Postings {
	rev := make(Postings, len(p))
	for i, posting := range p {
		if posting.Amount == nil {
			panic(fmt.Sprintf("ledger: postings[%d] has nil amount, call Validate first", i))
		}
		rev[len(p)-1-i] = Posting{
			Source:      posting.Destination,
			Destination: posting.Source,
			Asset:       posting.Asset,
			Amount:      new(big.Int).Set(posting.Amount),
		}
	}
	return rev
}
