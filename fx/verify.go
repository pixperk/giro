package fx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/pixperk/giro/ledger"
)

// DefaultTolerance is how far a conversion may sit from its stated rate before
// it counts as a disagreement, in minor units of the received asset.
//
// One, because truncation is expected rather than exceptional: a scale 6 asset
// converting into a scale 2 one lands between cents almost every time. A
// larger tolerance would stop being an allowance for rounding and start being
// somewhere to hide.
const DefaultTolerance = 1

// Querier is what Verify needs, which is one query. Both *pgxpool.Pool and
// pgx.Tx satisfy it.
//
// An interface rather than a *storage.Store, so this package depends on a
// shape and not on the engine. The direction is the point: the ledger must
// never import this, because that would be the core learning about exchange
// rates by the back door.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Verify recomputes every transaction on a ledger that states an exchange rate
// and reports any whose amounts disagree with it.
//
// It answers one question and deliberately not the other. Whether the rate was
// a fair price is a pricing question, and neither this package nor the ledger
// has any way to know. Whether the amounts match the rate recorded beside them
// is arithmetic, and a book that disagrees with its own stated rate is wrong
// whatever the market did.
//
// So this does not catch a rate that is simply wrong. Record 0.9960 in both the
// rate and the amounts and everything agrees while the trade is nine thousand
// dollars light. Only comparing against the venue's own statement finds that,
// which is reconciliation and lives above even this.
//
// Transactions stating no rate are not conversions and are not examined, so a
// ledger that never trades is not suddenly full of findings.
func Verify(ctx context.Context, db Querier, ledgerName string, tolerance int64) (checked int, err error) {
	rows, err := db.Query(ctx, `
		select id, postings, metadata
		  from transactions
		 where ledger = $1 and jsonb_exists(metadata, $2)
		 order by id`,
		ledgerName, ConversionRateKey)
	if err != nil {
		return 0, fmt.Errorf("verify conversions: %w", err)
	}
	defer rows.Close()

	var found []error
	for rows.Next() {
		var t ledger.Transaction
		var postings, metadata []byte
		if err := rows.Scan(&t.ID, &postings, &metadata); err != nil {
			return checked, err
		}
		if err := json.Unmarshal(postings, &t.Postings); err != nil {
			return checked, fmt.Errorf("transaction %d postings: %w", t.ID, err)
		}
		if err := json.Unmarshal(metadata, &t.Metadata); err != nil {
			return checked, fmt.Errorf("transaction %d metadata: %w", t.ID, err)
		}
		checked++
		if err := Check(&t, tolerance); err != nil {
			found = append(found, err)
		}
	}
	if err := rows.Err(); err != nil {
		return checked, err
	}
	return checked, errors.Join(found...)
}
