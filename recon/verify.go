package recon

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/pixperk/giro/ledger"
)

// The check that belongs beside the ledger's own.
//
// Matching produces a queue of things a person should look at, and the failure
// mode of a queue is that nobody looks. A line that has sat unmatched for a
// week is not a smaller problem than a broken hash chain; it is a slower one,
// and it is the reason reconciliation exists at all.

// StaleBreak is a staged line nobody has resolved.
type StaleBreak struct {
	Source    string
	RecordID  string
	Reference string
	Asset     ledger.Asset
	Amount    *big.Int
	Since     time.Time
}

func (b StaleBreak) Error() string {
	ref := b.Reference
	if ref == "" {
		ref = "no reference"
	}
	return fmt.Sprintf("%s/%s (%s, %s %s) unmatched since %s",
		b.Source, b.RecordID, ref, b.Amount, b.Asset, b.Since.Format(time.RFC3339))
}

// Unmatched reports staged lines older than a cutoff, oldest first, and how
// many staged lines it examined.
//
// The count is of everything staged rather than of findings, for the same
// reason every other check here reports one: a run against a ledger nothing
// has been ingested into has looked at nothing, and that is not the same as
// finding nothing wrong.
//
// A grace period rather than reporting everything unmatched, because a line
// that arrived a minute ago has not failed to reconcile, it simply has not
// been reconciled yet. Settlement files land before the movements they
// describe often enough that treating every fresh line as a break would drown
// the real ones.
func Unmatched(ctx context.Context, db DB, ledgerName string, olderThan time.Duration) (checked int, err error) {
	if olderThan < 0 {
		return 0, fmt.Errorf("olderThan is negative: %s", olderThan)
	}
	cutoff := time.Now().Add(-olderThan)

	rows, err := db.Query(ctx, `
		select source, record_id, coalesce(reference, ''), asset, amount::text, ingested_at
		  from recon_records
		 where ledger = $1 and matched_count = 0 and ingested_at < $2
		 order by ingested_at, source, record_id`,
		ledgerName, cutoff)
	if err != nil {
		return 0, fmt.Errorf("unmatched records: %w", err)
	}
	defer rows.Close()

	var found []error
	for rows.Next() {
		var b StaleBreak
		var amount string
		if err := rows.Scan(&b.Source, &b.RecordID, &b.Reference, &b.Asset, &amount, &b.Since); err != nil {
			return 0, err
		}
		b.Amount, _ = new(big.Int).SetString(amount, 10)
		b.Since = b.Since.UTC()
		found = append(found, b)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	if err := db.QueryRow(ctx,
		"select count(*) from recon_records where ledger = $1", ledgerName).Scan(&checked); err != nil {
		return 0, err
	}
	return checked, errors.Join(found...)
}

// Check runs a matching pass and then reports what is still outstanding,
// shaped for giro verify.
//
// Matching first, because a check that reports breaks without having tried to
// resolve them is reporting the state of the last run rather than the state of
// the book.
func Check(ctx context.Context, db DB, ledgerName string, cfg Config, olderThan time.Duration) (checked int, err error) {
	if _, err := Match(ctx, db, ledgerName, cfg); err != nil {
		return 0, err
	}
	return Unmatched(ctx, db, ledgerName, olderThan)
}
