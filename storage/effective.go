package storage

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/pixperk/giro/ledger"
)

// GetBalancesAt returns what an account held on an effective date.
//
// this reads the stored snapshot rather than summing history, so the cost does
// not grow with the account's age: one index seek per asset on
// (ledger, address, asset, effective_date desc, seq desc).
//
// the snapshot is maintained on write, which is the fast half of a trade whose
// slow half lives in VerifyEffectiveVolumes.
func (s *Store) GetBalancesAt(ctx context.Context, address ledger.Address, at time.Time) (map[ledger.Asset]*big.Int, error) {
	rows, err := s.pool.Query(ctx, `
		select distinct on (asset) asset, pcev_input, pcev_output
		  from moves
		 where ledger = $1 and address = $2 and effective_date <= $3
		 order by asset, effective_date desc, seq desc`,
		s.ledger, address, at.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[ledger.Asset]*big.Int{}
	for rows.Next() {
		var asset ledger.Asset
		var in, o pgtype.Numeric
		if err := rows.Scan(&asset, &in, &o); err != nil {
			return nil, err
		}
		input, err := bigInt(in)
		if err != nil {
			return nil, err
		}
		output, err := bigInt(o)
		if err != nil {
			return nil, err
		}
		out[asset] = new(big.Int).Sub(input, output)
	}
	return out, rows.Err()
}

// GetEffectiveVolumesAt is the same read, keeping both counters.
func (s *Store) GetEffectiveVolumesAt(ctx context.Context, address ledger.Address, at time.Time) (map[ledger.Asset]ledger.Volumes, error) {
	rows, err := s.pool.Query(ctx, `
		select distinct on (asset) asset, pcev_input, pcev_output
		  from moves
		 where ledger = $1 and address = $2 and effective_date <= $3
		 order by asset, effective_date desc, seq desc`,
		s.ledger, address, at.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[ledger.Asset]ledger.Volumes{}
	for rows.Next() {
		var asset ledger.Asset
		var in, o pgtype.Numeric
		if err := rows.Scan(&asset, &in, &o); err != nil {
			return nil, err
		}
		input, err := bigInt(in)
		if err != nil {
			return nil, err
		}
		output, err := bigInt(o)
		if err != nil {
			return nil, err
		}
		out[asset] = ledger.Volumes{Input: input, Output: output}
	}
	return out, rows.Err()
}

// EffectiveVolumesMismatch names one move whose stored snapshot disagrees with
// a replay of the history before it.
type EffectiveVolumesMismatch struct {
	Seq          int64
	Account      ledger.Address
	Asset        ledger.Asset
	Stored, Want ledger.Volumes
}

func (e *EffectiveVolumesMismatch) Error() string {
	return fmt.Sprintf("move %d (%s %s): stored (%s, %s), replay says (%s, %s)",
		e.Seq, e.Account, e.Asset, e.Stored.Input, e.Stored.Output, e.Want.Input, e.Want.Output)
}

// VerifyEffectiveVolumes recomputes every move's effective snapshot from
// scratch and compares it to what is stored.
//
// this is the slow, obviously correct implementation: walk each account and
// asset in effective date order and accumulate. maintaining the snapshot on
// write is an optimisation, and an optimisation nothing checks is a guess.
//
// it is not on the write path. run it in tests, on demand, or on a schedule.
func (s *Store) VerifyEffectiveVolumes(ctx context.Context) (checked int, err error) {
	rows, err := s.pool.Query(ctx, `
		select seq, address, asset, amount, is_source, pcev_input, pcev_output
		  from moves
		 where ledger = $1
		 order by address, asset, effective_date, seq`,
		s.ledger)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	running := map[key]ledger.Volumes{}

	for rows.Next() {
		var seq int64
		var k key
		var isSource bool
		var amount, storedIn, storedOut pgtype.Numeric

		if err := rows.Scan(&seq, &k.account, &k.asset, &amount, &isSource, &storedIn, &storedOut); err != nil {
			return checked, err
		}

		value, err := bigInt(amount)
		if err != nil {
			return checked, err
		}
		apply(running, k, value, isSource)

		stored, err := volumesFrom(storedIn, storedOut)
		if err != nil {
			return checked, err
		}

		want := running[k]
		if stored.Input.Cmp(want.Input) != 0 || stored.Output.Cmp(want.Output) != 0 {
			return checked, &EffectiveVolumesMismatch{
				Seq: seq, Account: k.account, Asset: k.asset,
				Stored: stored, Want: want,
			}
		}
		checked++
	}
	return checked, rows.Err()
}

func volumesFrom(in, out pgtype.Numeric) (ledger.Volumes, error) {
	input, err := bigInt(in)
	if err != nil {
		return ledger.Volumes{}, err
	}
	output, err := bigInt(out)
	if err != nil {
		return ledger.Volumes{}, err
	}
	return ledger.Volumes{Input: input, Output: output}, nil
}
