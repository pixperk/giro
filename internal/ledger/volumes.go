package ledger

import (
	"cmp"
	"fmt"
	"math/big"
	"slices"
	"strings"
)

// volumes are two counters per (account, asset) that only ever increase.
// balance is derived, never stored. keeping gross flow means an account that
// settled millions and now holds nothing is distinguishable from one that was
// never used, and it makes every update relative rather than absolute.
type Volumes struct {
	Input, Output *big.Int
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
	Account, Asset string
	Input, Output  *big.Int
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
func (p Postings) VolumeUpdates() []VolumeUpdate {
	type key struct{ account, asset string }

	index := make(map[key]*VolumeUpdate, 2*len(p))
	at := func(account, asset string) *VolumeUpdate {
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
		if posting.Amount == nil {
			panic(fmt.Sprintf("ledger: postings[%d] has nil amount, call Validate first", i))
		}

		// a self posting resolves to the same entry twice, so both counters
		// move and the balance is unchanged. no special case needed.
		src := at(posting.Source, posting.Asset)
		src.Output.Add(src.Output, posting.Amount)

		dst := at(posting.Destination, posting.Asset)
		dst.Input.Add(dst.Input, posting.Amount)
	}

	updates := make([]VolumeUpdate, 0, len(index))
	for _, u := range index {
		updates = append(updates, *u)
	}

	// map order is random, so this sort is what makes the lock order
	// deterministic across processes.
	slices.SortFunc(updates, func(a, b VolumeUpdate) int {
		return cmp.Or(
			strings.Compare(a.Account, b.Account),
			strings.Compare(a.Asset, b.Asset),
		)
	})
	return updates
}
