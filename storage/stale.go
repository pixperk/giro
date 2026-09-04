package storage

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/pixperk/giro/ledger"
)

// Money that is sitting still.
//
// The case this exists for is money in flight. A wire is submitted at two and
// confirmed at six, and the question is where it lives in between. Posting it
// at submission means the books are wrong for four hours if the wire bounces;
// posting it at settlement means the payer's balance still shows money that
// has already gone. Both are the same mistake: using one event to record two.
//
// So it moves into a holding account and out again:
//
//	submitted   client:acme                 -> pending:wire:WR-2026-0142
//	settled     pending:wire:WR-2026-0142   -> external:bank:infinitus:USD
//	returned    pending:wire:WR-2026-0142   -> client:acme
//
// That needs nothing from the engine, and three useful properties fall out of
// what is already here. The payer cannot spend it, because it genuinely left
// their account and the balance guard is what stops them. Total value in
// transit is one prefix read rather than a report. And it cannot be settled
// twice, because the holding account holds exactly the amount once: a second
// settlement would overdraw an account nobody permitted.
//
// What does not fall out is a timeout. A wire that neither settles nor returns
// leaves money in the holding account forever, and nothing is wrong with the
// arithmetic while it does, which is exactly why nothing notices. Hence this.

// StaleBalance names an account holding a balance that has not moved recently.
type StaleBalance struct {
	Account  ledger.Address `json:"account"`
	Asset    ledger.Asset   `json:"asset"`
	Balance  *big.Int       `json:"balance"`
	LastMove time.Time      `json:"lastMove"`
}

func (b StaleBalance) String() string {
	return fmt.Sprintf("%s %s %s, last moved %s",
		b.Account, b.Balance, b.Asset, b.LastMove.Format(time.RFC3339))
}

// StaleBalances finds accounts under a prefix holding a non-zero balance whose
// most recent movement is older than the cutoff, oldest first.
//
// Deliberately not called anything to do with holds. It answers "money sitting
// still under this name", and the engine has no opinion about what the name
// means, which is the same position it takes on every other address. The same
// call finds dormant client accounts, and an operating account that should
// have returned to zero and did not.
//
// An empty prefix covers the whole ledger, in which case it will report every
// long-lived balance there is. That is a legitimate question and rarely the
// one being asked.
//
// It reads insertion order rather than effective dates: the question is when
// this ledger last recorded something happening, not when the thing happened.
// A settlement file booked today for last week means the account moved today.
//
// Not on the write path. Run it on a schedule, and alert on the absence of a
// run as well as on findings.
func (s *Store) StaleBalances(ctx context.Context, prefix string, olderThan time.Duration) ([]StaleBalance, error) {
	if olderThan < 0 {
		return nil, fmt.Errorf("olderThan is negative: %s", olderThan)
	}
	cutoff := time.Now().Add(-olderThan)

	rows, err := s.pool.Query(ctx, `
		select v.address, v.asset, (v.input - v.output)::text, m.last
		  from accounts_volumes v
		  join lateral (
		      select max(insertion_date) as last
		        from moves
		       where ledger = v.ledger and address = v.address and asset = v.asset
		  ) m on true
		 where v.ledger = $1
		   and ($2 = '' or v.address like $2 || '%')
		   and v.input <> v.output
		   and m.last < $3
		 order by m.last, v.address, v.asset`,
		s.ledger, prefix, cutoff)
	if err != nil {
		return nil, fmt.Errorf("stale balances: %w", err)
	}
	defer rows.Close()

	var out []StaleBalance
	for rows.Next() {
		var b StaleBalance
		var balance string
		if err := rows.Scan(&b.Account, &b.Asset, &balance, &b.LastMove); err != nil {
			return nil, err
		}
		b.Balance, _ = new(big.Int).SetString(balance, 10)
		b.LastMove = b.LastMove.UTC()
		out = append(out, b)
	}
	return out, rows.Err()
}
