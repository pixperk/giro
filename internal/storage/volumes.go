package storage

import (
	"context"
	"fmt"
	"math/big"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/pixperk/giro/internal/ledger"
)

// identifies one accounts_volumes row.
type key struct {
	account string
	asset   string
}

// pgx has no native mapping between numeric and *big.Int. pgtype.Numeric
// carries an unscaled *big.Int plus an exponent, so with Exp 0 it is exactly
// an integer and round trips without going near a float.
func numeric(i *big.Int) pgtype.Numeric {
	return pgtype.Numeric{Int: i, Exp: 0, Valid: true}
}

func bigInt(n pgtype.Numeric) (*big.Int, error) {
	if !n.Valid {
		return new(big.Int), nil
	}
	if n.Exp == 0 {
		return new(big.Int).Set(n.Int), nil
	}
	if n.Exp > 0 {
		// 1e3 stored as Int=1 Exp=3. scale it back up.
		mul := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n.Exp)), nil)
		return new(big.Int).Mul(n.Int, mul), nil
	}
	// a fractional value in a column that should only ever hold integers
	return nil, fmt.Errorf("numeric %v has a fractional part", n)
}

// lockVolumes takes an exclusive row lock on every (account, asset) the
// transaction touches and returns their volumes as they stand.
//
// two statements rather than one, deliberately. the tempting version puts the
// insert in a data modifying CTE alongside the select, but sub statements in a
// WITH clause run concurrently with the main query and their effects are not
// visible to it, so the select would never see the rows the insert just made.
// as separate statements in the same transaction, the second sees the first.
//
// updates arrive already sorted by (account, asset). that ordering is the
// whole deadlock defence, so it is preserved here and asserted again with an
// ORDER BY, because the planner is not obliged to lock in the order rows were
// listed.
func (s *Store) lockVolumes(ctx context.Context, tx pgx.Tx, updates []ledger.VolumeUpdate) (map[key]ledger.Volumes, error) {
	addresses := make([]string, len(updates))
	assets := make([]string, len(updates))
	for i, u := range updates {
		addresses[i] = u.Account
		assets[i] = u.Asset
	}

	// you cannot lock a row that does not exist, and FOR UPDATE on a missing
	// row silently locks nothing rather than failing. materialise the zero
	// rows first so there is something to lock. if this transaction rolls
	// back they disappear with it.
	//
	// under a race, ON CONFLICT DO NOTHING blocks on the other transaction's
	// uncommitted row rather than skipping it, so the two serialise here.
	if _, err := tx.Exec(ctx, `
		insert into accounts_volumes (ledger, address, asset)
		select $1, a, s from unnest($2::text[], $3::text[]) as t(a, s)
		on conflict do nothing`,
		s.ledger, addresses, assets); err != nil {
		return nil, fmt.Errorf("materialise volume rows: %w", err)
	}

	rows, err := tx.Query(ctx, `
		select address, asset, input, output
		from accounts_volumes
		where ledger = $1
		  and (address, asset) in (select * from unnest($2::text[], $3::text[]))
		order by address, asset
		for update`,
		s.ledger, addresses, assets)
	if err != nil {
		return nil, fmt.Errorf("lock volume rows: %w", err)
	}
	defer rows.Close()

	current := make(map[key]ledger.Volumes, len(updates))
	for rows.Next() {
		var k key
		var in, out pgtype.Numeric
		if err := rows.Scan(&k.account, &k.asset, &in, &out); err != nil {
			return nil, err
		}
		input, err := bigInt(in)
		if err != nil {
			return nil, err
		}
		output, err := bigInt(out)
		if err != nil {
			return nil, err
		}
		current[k] = ledger.Volumes{Input: input, Output: output}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// anything the select did not return is a row this transaction just
	// created, which is invisible to its own snapshot. it is locked either
	// way, by virtue of being ours and uncommitted.
	for _, u := range updates {
		k := key{u.Account, u.Asset}
		if _, ok := current[k]; !ok {
			current[k] = ledger.NewVolumes()
		}
	}

	return current, nil
}

// applyVolumes adds the deltas. relative, never absolute: the database
// performs the addition, so a value read before the check never becomes a
// value written after it.
func (s *Store) applyVolumes(ctx context.Context, tx pgx.Tx, updates []ledger.VolumeUpdate) error {
	batch := &pgx.Batch{}
	for _, u := range updates {
		batch.Queue(`
			update accounts_volumes
			   set input = input + $4, output = output + $5
			 where ledger = $1 and address = $2 and asset = $3`,
			s.ledger, u.Account, u.Asset, numeric(u.Input), numeric(u.Output))
	}

	results := tx.SendBatch(ctx, batch)
	defer func() { _ = results.Close() }()

	for i := range updates {
		tag, err := results.Exec()
		if err != nil {
			return fmt.Errorf("apply volumes for %s: %w", updates[i].Account, err)
		}
		if tag.RowsAffected() != 1 {
			// unreachable: lockVolumes guarantees the row exists
			return fmt.Errorf("apply volumes for %s/%s: %d rows affected",
				updates[i].Account, updates[i].Asset, tag.RowsAffected())
		}
	}
	return results.Close()
}
