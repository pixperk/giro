package ledger

import (
	"cmp"
	"fmt"
	"math/big"
	"slices"
)

// volumes are two counters per (account, asset) that only ever increase.
// balance is derived, never stored. keeping gross flow means an account that
// settled millions and now holds nothing is distinguishable from one that was
// never used, and it makes every update relative rather than absolute.
type Volumes struct {
	Input  *big.Int `json:"input"`
	Output *big.Int `json:"output"`
}

// zero value Volumes is usable: a nil counter means nothing has flowed, which
// is what an account with no row actually means.
func NewVolumes() Volumes {
	return Volumes{Input: new(big.Int), Output: new(big.Int)}
}

func (v Volumes) Balance() *big.Int {
	return new(big.Int).Sub(orZero(v.Input), orZero(v.Output))
}

func orZero(i *big.Int) *big.Int {
	if i == nil {
		return new(big.Int)
	}
	return i
}

// the volume delta a transaction applies to one (account, asset) pair.
type VolumeUpdate struct {
	Account Address
	Asset   Asset
	Input   *big.Int
	Output  *big.Int
}

// the volume deltas a transaction applies, one entry per distinct
// (account, asset) pair rather than one per posting. an account appearing in
// several postings gets a single entry with its amounts summed.
//
// source accounts accumulate Output, destinations accumulate Input. an account
// that is both in the same transaction accumulates both.
//
// the result must be sorted by account then asset. that ordering is the lock
// order used at commit time, and taking locks in a globally consistent order
// is what makes deadlock structurally impossible rather than merely unlikely.
// A nil Amount is an error rather than a panic, for the reason given on
// Reverse: this package is embedded, and a panic takes the host down.
func (p Postings) VolumeUpdates() ([]VolumeUpdate, error) {
	type key struct {
		account Address
		asset   Asset
	}

	index := make(map[key]*VolumeUpdate, 2*len(p))
	at := func(account Address, asset Asset) *VolumeUpdate {
		k := key{account, asset}
		u, ok := index[k]
		if !ok {
			u = &VolumeUpdate{
				Account: account,
				Asset:   asset,
				Input:   new(big.Int),
				Output:  new(big.Int),
			}
			index[k] = u
		}
		return u
	}

	for i, posting := range p {
		amount := posting.Amount

		// an UpTo posting may carry no ceiling at all, and that nil is a
		// legitimate "no limit" rather than a malformed request. it
		// contributes nothing to the deltas here because its real figure is
		// not known until the rows are locked; what this pass is for is naming
		// the (account, asset) pairs to lock, and those are the same either
		// way. the caller recomputes once the amount is resolved.
		if amount == nil && posting.UpTo {
			amount = new(big.Int)
		}
		if amount == nil {
			return nil, fmt.Errorf("%w: postings[%d] has a nil amount", ErrNilAmount, i)
		}

		// a self posting resolves to the same entry twice, so both counters
		// move and the balance is unchanged. no special case needed.
		src := at(posting.Source, posting.Asset)
		src.Output.Add(src.Output, amount)

		dst := at(posting.Destination, posting.Asset)
		dst.Input.Add(dst.Input, amount)
	}

	updates := make([]VolumeUpdate, 0, len(index))
	for _, u := range index {
		updates = append(updates, *u)
	}

	// map order is random, so this sort is what makes the lock order
	// deterministic across processes.
	slices.SortFunc(updates, func(a, b VolumeUpdate) int {
		return cmp.Or(
			cmp.Compare(a.Account, b.Account),
			cmp.Compare(a.Asset, b.Asset),
		)
	})
	return updates, nil
}
