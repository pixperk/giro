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

// identifies one accounts_volumes row.
type key struct {
	account ledger.Address
	asset   ledger.Asset
}

// one accounts_volumes row as it stood when the commit path locked it: the
// volumes, and whether this account is permitted to end below zero in this
// asset.
//
// the flag travels with the volumes because it is read from the same row under
// the same FOR UPDATE. reading it separately would mean a second statement
// that could see a value committed after the lock was taken, which is the one
// thing the lock exists to prevent.
type locked struct {
	ledger.Volumes
	allowNegative bool
	allowPositive bool
	// the account is closed, so it accepts nothing in either direction. a fact
	// about the address rather than the (address, asset) row, read here so the
	// commit path does not need a second query or a second lock for it.
	closed bool
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
func (s *Store) lockVolumes(ctx context.Context, tx pgx.Tx, updates []ledger.VolumeUpdate) (map[key]locked, error) {
	addresses, assets := pairs(updates)

	// you cannot lock a row that does not exist, and FOR UPDATE on a missing
	// row silently locks nothing rather than failing. materialise the zero
	// rows first so there is something to lock. if this transaction rolls
	// back they disappear with it.
	//
	// under a race, ON CONFLICT DO NOTHING blocks on the other transaction's
	// uncommitted row rather than skipping it, so the two serialise here.
	//
	// world is created already permitted to go negative. it is the account the
	// whole model leans on, so it is the built-in instance of the permission
	// rather than a name the balance guard has to know about.
	// Three statements, one round trip. They must run in this order and they
	// do: a pipelined batch is sent together and executed in sequence on the
	// server, inside the same transaction, so the locking semantics are
	// identical to issuing them one at a time. What changes is only how many
	// times the client waits for the network.
	//
	// That matters more than it looks. The lock this takes is held until
	// commit, and on a database across a network the hold time is round trips
	// rather than work -- measured at 44ms latency, a workload sharing one
	// source account ran at 2.0 tx/s against 3.4 for disjoint accounts, purely
	// because this lock spans more of the commit. Every round trip removed
	// from between here and COMMIT is worth its share of that.
	batch := &pgx.Batch{}

	// you cannot lock a row that does not exist, and FOR UPDATE on a missing
	// row silently locks nothing rather than failing. materialise the zero
	// rows first so there is something to lock. if this transaction rolls
	// back they disappear with it.
	//
	// under a race, ON CONFLICT DO NOTHING blocks on the other transaction's
	// uncommitted row rather than skipping it, so the two serialise here.
	//
	// world is created already permitted to go negative. it is the account the
	// whole model leans on, so it is the built-in instance of the permission
	// rather than a name the balance guard has to know about.
	batch.Queue(`
		insert into accounts_volumes (ledger, address, asset, allow_negative)
		select $1, a, s, a = $4 from unnest($2::text[], $3::text[]) as t(a, s)
		on conflict do nothing`,
		s.ledger, addresses, assets, ledger.WorldAccount)

	batch.Queue(`
		select address, asset, input, output, allow_negative, allow_positive
		from accounts_volumes
		where ledger = $1
		  and (address, asset) in (select * from unnest($2::text[], $3::text[]))
		order by address, asset
		for update`,
		s.ledger, addresses, assets)

	// which of these addresses are closed. one indexed lookup against the
	// partial index, so it costs a seek rather than a scan.
	//
	// deliberately not locked, and deliberately not joined onto the select
	// above: a join would answer for the pairs that came back and leave the
	// ones filled in below unanswered, and an account closed in one asset
	// accepting another is exactly the hole this is meant not to have.
	batch.Queue(`
		select address from accounts
		 where ledger = $1 and closed and address = any($2::text[])`,
		s.ledger, addresses)

	// timed and spanned because this is where a commit waits. it is the only
	// place that can measure it: from outside the engine a contended commit
	// and a slow one are the same slow request.
	lockStart := time.Now()
	lockCtx, endLock := s.start(ctx, SpanLock)
	results := tx.SendBatch(lockCtx, batch)
	defer func() { _ = results.Close() }()

	if _, err := results.Exec(); err != nil {
		endLock(err)
		return nil, fmt.Errorf("materialise volume rows: %w", err)
	}
	rows, err := results.Query()
	endLock(err)
	if err != nil {
		return nil, fmt.Errorf("lock volume rows: %w", err)
	}
	if s.observing() {
		locked := make([]ledger.Address, len(addresses))
		for i, a := range addresses {
			locked[i] = ledger.Address(a)
		}
		s.observeContention(ctx, Contention{Waited: time.Since(lockStart), Accounts: locked})
	}

	current := make(map[key]locked, len(updates))
	for rows.Next() {
		var k key
		var in, out pgtype.Numeric
		var allowNegative, allowPositive bool
		if err := rows.Scan(&k.account, &k.asset, &in, &out, &allowNegative, &allowPositive); err != nil {
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
		current[k] = locked{
			Volumes:       ledger.Volumes{Input: input, Output: output},
			allowNegative: allowNegative,
			allowPositive: allowPositive,
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	// a batch is read strictly in order: this result has to be finished before
	// the closure query's can be touched
	rows.Close()

	// anything the select did not return is a row this transaction just
	// created, which is invisible to its own snapshot. it is locked either
	// way, by virtue of being ours and uncommitted. it was created by the
	// insert above, so its permission is known without reading it back.
	for _, u := range updates {
		k := key{u.Account, u.Asset}
		if _, ok := current[k]; !ok {
			current[k] = locked{
				Volumes:       ledger.NewVolumes(),
				allowNegative: u.Account == ledger.WorldAccount,
				allowPositive: true,
			}
		}
	}

	// closure last, and by account rather than by (account, asset), because
	// that is what it is a fact about. joining it onto the select above would
	// answer for the pairs that came back and leave the ones filled in below
	// unanswered, and an account closed in one asset accepting another is
	// exactly the hole this is meant not to have.
	//
	// deliberately not locked. a lock on the accounts row taken by every
	// commit is the same shape as the foreign key to ledgers that had to be
	// removed for deadlocking, so an account closed concurrently with a commit
	// is left to VerifyClosedAccounts instead.
	closed, err := scanClosed(results)
	if err != nil {
		return nil, err
	}
	for k, v := range current {
		if closed[k.account] {
			v.closed = true
			current[k] = v
		}
	}
	return current, nil
}

// scanClosed reads the third result of the batch above.
func scanClosed(results pgx.BatchResults) (map[ledger.Address]bool, error) {
	rows, err := results.Query()
	if err != nil {
		return nil, fmt.Errorf("read closed accounts: %w", err)
	}
	defer rows.Close()

	out := map[ledger.Address]bool{}
	for rows.Next() {
		var a ledger.Address
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out[a] = true
	}
	return out, rows.Err()
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
