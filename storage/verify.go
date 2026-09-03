package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/pixperk/giro/ledger"
)

// ProjectionMismatch names one account and asset where the stored volumes
// disagree with what the log says they should be.
type ProjectionMismatch struct {
	Account      ledger.Address
	Asset        ledger.Asset
	Stored, Want ledger.Volumes
	Missing      bool // the log expects this row and there is none
	Extra        bool // the row exists and the log never mentions it
}

func (e *ProjectionMismatch) Error() string {
	switch {
	case e.Missing:
		return fmt.Sprintf("%s %s: the log says (%s, %s) and there is no row",
			e.Account, e.Asset, e.Want.Input, e.Want.Output)
	case e.Extra:
		return fmt.Sprintf("%s %s: row holds (%s, %s) and the log never mentions it",
			e.Account, e.Asset, e.Stored.Input, e.Stored.Output)
	default:
		return fmt.Sprintf("%s %s: row holds (%s, %s), the log says (%s, %s)",
			e.Account, e.Asset, e.Stored.Input, e.Stored.Output, e.Want.Input, e.Want.Output)
	}
}

// VerifyProjection replays the log and checks that accounts_volumes is exactly
// what it produces.
//
// This is the invariant that makes "the log is the source of truth" a fact
// rather than a statement of intent. The transactions, moves and volumes tables
// are a projection kept because replaying from zero on every read would be
// absurd; if they can drift from the log without anything noticing, the log is
// merely an audit trail that happens to sit alongside the real data.
//
// It also catches a commit path that writes one thing and logs another, which
// nothing else would: every other check reads the projection, so a consistent
// lie passes them all.
//
// Not on the write path. Run it in tests, on demand, or on a schedule.
func (s *Store) VerifyProjection(ctx context.Context) (checked int, err error) {
	want, seen, err := s.replayLog(ctx)
	if err != nil {
		return 0, err
	}

	stored, err := s.storedVolumes(ctx)
	if err != nil {
		return 0, err
	}

	// deterministic order, so a failure names the same row every run
	keys := make([]key, 0, len(want))
	for k := range want {
		keys = append(keys, k)
	}
	for k := range stored {
		if _, ok := want[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].account != keys[j].account {
			return keys[i].account < keys[j].account
		}
		return keys[i].asset < keys[j].asset
	})

	for _, k := range keys {
		expected, inLog := want[k]
		actual, inTable := stored[k]

		switch {
		case inLog && !inTable:
			return checked, &ProjectionMismatch{
				Account: k.account, Asset: k.asset, Want: expected, Missing: true,
			}
		case !inLog && inTable:
			// a zero row is not a lie: the commit path materialises rows it
			// needs to lock, and a rolled back attempt can leave one behind
			// only if it committed, which means the log has it too. anything
			// non zero here is real drift.
			if actual.Balance().Sign() != 0 || actual.Input.Sign() != 0 {
				return checked, &ProjectionMismatch{
					Account: k.account, Asset: k.asset, Stored: actual, Extra: true,
				}
			}
		default:
			if actual.Input.Cmp(expected.Input) != 0 || actual.Output.Cmp(expected.Output) != 0 {
				return checked, &ProjectionMismatch{
					Account: k.account, Asset: k.asset, Stored: actual, Want: expected,
				}
			}
		}
		checked++
	}

	if err := s.verifyTransactionsMatchLog(ctx, seen); err != nil {
		return checked, err
	}
	return checked, nil
}

// walks the log in order and accumulates what the volumes must be.
//
// metadata entries do not move value, so only the two transaction types
// contribute.
func (s *Store) replayLog(ctx context.Context) (map[key]ledger.Volumes, map[int64]bool, error) {
	rows, err := s.pool.Query(ctx,
		`select id, type, data from logs where ledger = $1 order by id`, s.ledger)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	volumes := map[key]ledger.Volumes{}
	transactions := map[int64]bool{}

	for rows.Next() {
		var id int64
		var typ string
		var data []byte
		if err := rows.Scan(&id, &typ, &data); err != nil {
			return nil, nil, err
		}

		var tx *ledger.Transaction
		switch ledger.LogType(typ) {
		case ledger.LogNewTransaction:
			tx = &ledger.Transaction{}
			if err := json.Unmarshal(data, tx); err != nil {
				return nil, nil, fmt.Errorf("log %d: %w", id, err)
			}
		case ledger.LogRevertedTransaction:
			var payload ledger.RevertedTransactionPayload
			if err := json.Unmarshal(data, &payload); err != nil {
				return nil, nil, fmt.Errorf("log %d: %w", id, err)
			}
			tx = payload.Transaction
		default:
			continue // metadata moves no value
		}

		if tx == nil {
			return nil, nil, fmt.Errorf("log %d carries no transaction", id)
		}
		transactions[tx.ID] = true

		for _, p := range tx.Postings {
			apply(volumes, key{p.Source, p.Asset}, p.Amount, true)
			apply(volumes, key{p.Destination, p.Asset}, p.Amount, false)
		}
	}
	return volumes, transactions, rows.Err()
}

func (s *Store) storedVolumes(ctx context.Context) (map[key]ledger.Volumes, error) {
	rows, err := s.pool.Query(ctx,
		`select address, asset, input, output from accounts_volumes where ledger = $1`, s.ledger)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[key]ledger.Volumes{}
	for rows.Next() {
		var k key
		var in, o pgtype.Numeric
		if err := rows.Scan(&k.account, &k.asset, &in, &o); err != nil {
			return nil, err
		}
		v, err := volumesFrom(in, o)
		if err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// every transaction in the table must have been logged, and vice versa. a
// transaction the log does not know about would be invisible to a replica or a
// rebuild.
func (s *Store) verifyTransactionsMatchLog(ctx context.Context, logged map[int64]bool) error {
	rows, err := s.pool.Query(ctx,
		`select id from transactions where ledger = $1 order by id`, s.ledger)
	if err != nil {
		return err
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		if !logged[id] {
			return fmt.Errorf("transaction %d exists but was never logged", id)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return err
	}

	if count != len(logged) {
		return fmt.Errorf("the log describes %d transactions and the table holds %d", len(logged), count)
	}
	return nil
}
