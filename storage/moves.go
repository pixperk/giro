package storage

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/pixperk/giro/ledger"
)

// Writing the moves a transaction produces, and maintaining the two volume
// histories they carry.
//
// pcv follows insertion order and is written once. pcev follows effective date
// order, a different sequence whenever a transaction is backdated, so it has to
// be maintained rather than merely computed.

// two rows per posting, one per side, each carrying the account's volumes
// immediately after that move, in both orderings.
//
// pcv accumulates forward from the pre transaction state in insertion order,
// so an account appearing in several postings gets a different running value on
// each of its moves rather than the final one repeated. it is written once and
// never touched again: it records what the ledger believed at the time.
//
// pcev is the same accumulation in effective date order, which is a different
// sequence whenever a transaction is backdated. it starts from the effective
// balance as of this transaction's timestamp rather than from the current one,
// and every move that already sits later in effective order is shifted by this
// transaction's delta.
func (s *Store) insertMoves(ctx context.Context, tx pgx.Tx, t *ledger.Transaction, before map[key]ledger.Volumes, updates []ledger.VolumeUpdate) error {
	effectiveBefore, err := s.effectiveVolumesAt(ctx, tx, updates, t.Timestamp)
	if err != nil {
		return err
	}

	running := copyVolumes(before)
	effective := copyVolumes(effectiveBefore)

	batch := &pgx.Batch{}
	queue := func(address, asset string, amount *big.Int, isSource bool) {
		k := key{address, asset}
		apply(running, k, amount, isSource)
		apply(effective, k, amount, isSource)

		v, e := running[k], effective[k]
		batch.Queue(`
			insert into moves
				(ledger, tx_id, address, asset, amount, is_source,
				 effective_date, insertion_date,
				 pcv_input, pcv_output, pcev_input, pcev_output)
			values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
			s.ledger, t.ID, address, asset, numeric(amount), isSource,
			t.Timestamp, t.InsertedAt,
			numeric(v.Input), numeric(v.Output), numeric(e.Input), numeric(e.Output))
	}

	for _, p := range t.Postings {
		queue(p.Source, p.Asset, p.Amount, true)
		queue(p.Destination, p.Asset, p.Amount, false)
	}

	results := tx.SendBatch(ctx, batch)
	for range len(t.Postings) * 2 {
		if _, err := results.Exec(); err != nil {
			_ = results.Close()
			return fmt.Errorf("insert moves: %w", err)
		}
	}
	if err := results.Close(); err != nil {
		return err
	}

	return s.shiftLaterEffectiveVolumes(ctx, tx, updates, t.Timestamp)
}

// the effective volumes of each touched account as of a date.
//
// this reads the snapshot on the latest move at or before that date rather than
// summing the account's history. summing is the obvious implementation and it
// is O(n) in the account's age, executed on every write: measured, commit cost
// grew from 778us to 1451us as one account went from 200 moves to 3000.
//
// the same index that makes GetBalancesAt a seek serves this, and there is no
// reason the write path should be slower at it than the read path.
//
// moves sharing this exact effective date count as before, because they were
// inserted first and effective order breaks ties by seq.
func (s *Store) effectiveVolumesAt(ctx context.Context, tx pgx.Tx, updates []ledger.VolumeUpdate, at time.Time) (map[key]ledger.Volumes, error) {
	addresses, assets := pairs(updates)

	rows, err := tx.Query(ctx, `
		select distinct on (address, asset) address, asset, pcev_input, pcev_output
		  from moves
		 where ledger = $1
		   and (address, asset) in (select * from unnest($2::text[], $3::text[]))
		   and effective_date <= $4
		 order by address, asset, effective_date desc, seq desc`,
		s.ledger, addresses, assets, at)
	if err != nil {
		return nil, fmt.Errorf("effective volumes: %w", err)
	}
	defer rows.Close()

	out := make(map[key]ledger.Volumes, len(updates))
	for rows.Next() {
		var k key
		var in, o pgtype.Numeric
		if err := rows.Scan(&k.account, &k.asset, &in, &o); err != nil {
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
		out[k] = ledger.Volumes{Input: input, Output: output}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for _, u := range updates {
		if _, ok := out[key{u.Account, u.Asset}]; !ok {
			out[key{u.Account, u.Asset}] = ledger.NewVolumes()
		}
	}
	return out, nil
}

// a backdated transaction lands before moves that already exist, so their
// effective view is now short by its delta. strictly greater than, because a
// move sharing this effective date was inserted first and therefore sits
// before these ones.
//
// when nothing is backdated this matches no rows and costs one statement per
// touched pair.
func (s *Store) shiftLaterEffectiveVolumes(ctx context.Context, tx pgx.Tx, updates []ledger.VolumeUpdate, at time.Time) error {
	batch := &pgx.Batch{}
	for _, u := range updates {
		batch.Queue(`
			update moves
			   set pcev_input = pcev_input + $5, pcev_output = pcev_output + $6
			 where ledger = $1 and address = $2 and asset = $3
			   and effective_date > $4`,
			s.ledger, u.Account, u.Asset, at, numeric(u.Input), numeric(u.Output))
	}

	results := tx.SendBatch(ctx, batch)
	for range updates {
		if _, err := results.Exec(); err != nil {
			_ = results.Close()
			return fmt.Errorf("shift effective volumes: %w", err)
		}
	}
	return results.Close()
}

func copyVolumes(in map[key]ledger.Volumes) map[key]ledger.Volumes {
	out := make(map[key]ledger.Volumes, len(in))
	for k, v := range in {
		out[k] = ledger.Volumes{
			Input:  new(big.Int).Set(v.Input),
			Output: new(big.Int).Set(v.Output),
		}
	}
	return out
}

func apply(m map[key]ledger.Volumes, k key, amount *big.Int, isSource bool) {
	v := m[k]
	if v.Input == nil {
		v = ledger.NewVolumes()
	}
	if isSource {
		v.Output = new(big.Int).Add(v.Output, amount)
	} else {
		v.Input = new(big.Int).Add(v.Input, amount)
	}
	m[k] = v
}

func pairs(updates []ledger.VolumeUpdate) (addresses, assets []string) {
	addresses = make([]string, len(updates))
	assets = make([]string, len(updates))
	for i, u := range updates {
		addresses[i] = u.Account
		assets[i] = u.Asset
	}
	return addresses, assets
}
