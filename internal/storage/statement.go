package storage

// An account's own history.
//
// transactions are organised by transaction; almost every question a person
// asks is organised by account. moves is that same data keyed the way it is
// read, and this is the query it exists for.

import (
	"context"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/pixperk/giro/internal/ledger"
)

// one half of a posting, from the point of view of a single account.
type Move struct {
	Seq           int64     `json:"seq"`
	TransactionID int64     `json:"transactionId"`
	Asset         string    `json:"asset"`
	Amount        *big.Int  `json:"amount"`
	Incoming      bool      `json:"incoming"`
	EffectiveDate time.Time `json:"effectiveDate"`
	InsertionDate time.Time `json:"insertionDate"`

	// the account's volumes immediately after this move, in each ordering.
	// Volumes is frozen; EffectiveVolumes is rewritten when something lands
	// behind it.
	Volumes          ledger.Volumes  `json:"volumes"`
	EffectiveVolumes *ledger.Volumes `json:"effectiveVolumes,omitempty"`
}

// Balance is the derived figure a statement actually reads.
func (m Move) Balance() *big.Int { return m.Volumes.Balance() }

// the address lives in the filter rather than beside it, so the cursor carries
// it like everything else. a cursor that dropped the account it was created for
// would silently return an empty page rather than continuing the walk.
type MoveFilter struct {
	Address string     `json:"address"`
	Asset   string     `json:"asset,omitempty"`
	From    *time.Time `json:"from,omitempty"`
	To      *time.Time `json:"to,omitempty"`
}

type ListMovesQuery struct {
	Filter MoveFilter
	Limit  int
	Cursor string
}

// ListMoves returns an account's statement, oldest first, in effective date
// order.
//
// effective order rather than insertion order because that is what a statement
// means: the sequence in which things happened, not the sequence in which this
// database heard about them. the two differ whenever anything is backdated.
//
// ties are broken by seq, so two movements sharing an effective date appear in
// the order they arrived. that pair is also the pagination key, since
// effective_date alone is not unique.
func (s *Store) ListMoves(ctx context.Context, q ListMovesQuery) (Page[Move], error) {
	var page Page[Move]

	c := cursor[MoveFilter]{Filter: q.Filter, Limit: clampLimit(q.Limit)}
	if q.Cursor != "" {
		decoded, err := decodeCursor[MoveFilter](q.Cursor)
		if err != nil {
			return page, err
		}
		c = decoded
	}

	var afterDate time.Time
	if c.AfterDate != nil {
		afterDate = *c.AfterDate
	}

	rows, err := s.pool.Query(ctx, `
		select seq, tx_id, asset, amount, is_source, effective_date, insertion_date,
		       pcv_input, pcv_output, pcev_input, pcev_output
		  from moves
		 where ledger = $1 and address = $2
		   and ($3 = '' or asset = $3)
		   and ($4::timestamptz is null or effective_date >= $4)
		   and ($5::timestamptz is null or effective_date <= $5)
		   and ($6 or (effective_date, seq) > ($7, $8))
		 order by effective_date, seq
		 limit $9`,
		s.ledger, c.Filter.Address, c.Filter.Asset, c.Filter.From, c.Filter.To,
		c.After == 0, afterDate, c.After, c.Limit+1)
	if err != nil {
		return page, err
	}
	defer rows.Close()

	for rows.Next() {
		var m Move
		var amount, pcvIn, pcvOut pgtype.Numeric
		var pcevIn, pcevOut pgtype.Numeric
		var isSource bool

		if err := rows.Scan(&m.Seq, &m.TransactionID, &m.Asset, &amount, &isSource,
			&m.EffectiveDate, &m.InsertionDate, &pcvIn, &pcvOut, &pcevIn, &pcevOut); err != nil {
			return page, err
		}

		if m.Amount, err = bigInt(amount); err != nil {
			return page, err
		}
		if m.Volumes, err = volumesFrom(pcvIn, pcvOut); err != nil {
			return page, err
		}
		// null until effective volumes were computed, which is every move
		// written since they existed
		if pcevIn.Valid && pcevOut.Valid {
			v, err := volumesFrom(pcevIn, pcevOut)
			if err != nil {
				return page, err
			}
			m.EffectiveVolumes = &v
		}

		m.Incoming = !isSource
		m.EffectiveDate = m.EffectiveDate.UTC()
		m.InsertionDate = m.InsertionDate.UTC()
		page.Items = append(page.Items, m)
	}
	if err := rows.Err(); err != nil {
		return page, err
	}

	if len(page.Items) <= c.Limit {
		return page, nil
	}

	page.Items = page.Items[:c.Limit]
	last := page.Items[len(page.Items)-1]
	next, err := encodeCursor(cursor[MoveFilter]{
		Filter:    c.Filter,
		After:     last.Seq,
		AfterDate: &last.EffectiveDate,
		Limit:     c.Limit,
	})
	if err != nil {
		return page, err
	}
	page.Next = next
	return page, nil
}
