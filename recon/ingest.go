package recon

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// DB is what this package needs from a database handle. Both *pgxpool.Pool and
// pgx.Tx satisfy it.
//
// An interface rather than a *storage.Store, so recon depends on a shape and
// not on the engine. The direction is the point: the ledger must never import
// this.
type DB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Register declares a source. Idempotent, so a startup routine can run it on
// every boot.
func Register(ctx context.Context, db DB, ledgerName string, s Source) error {
	if s.ID() == "" {
		return errors.New("source has no id, and staged lines are keyed by it")
	}
	_, err := db.Exec(ctx, `
		insert into recon_sources (ledger, id, name) values ($1, $2, $3)
		on conflict (ledger, id) do nothing`,
		ledgerName, s.ID(), s.Name())
	if err != nil {
		return fmt.Errorf("register source %s: %w", s.ID(), err)
	}
	return nil
}

// Ingest stages lines from one source and reports how many were new.
//
// Idempotent per (source, record id), which is what makes it safe to retry
// after a timeout that may or may not have landed, and what makes overlapping
// windows the right way to page through a statement rather than something to
// avoid. Fetching the last hour every ten minutes is correct and cheap.
//
// Every line is validated before any is staged. A file with one malformed row
// stages nothing, because a partial ingest leaves nobody able to say which
// half arrived.
func Ingest(ctx context.Context, db DB, ledgerName, sourceID string, records []Record) (staged int, err error) {
	for i, r := range records {
		if err := r.validate(); err != nil {
			return 0, fmt.Errorf("record %d: %w", i, err)
		}
	}

	for _, r := range records {
		var occurred *time.Time
		if !r.OccurredAt.IsZero() {
			at := r.OccurredAt.UTC()
			occurred = &at
		}
		var reference *string
		if r.Reference != "" {
			reference = &r.Reference
		}

		tag, err := db.Exec(ctx, `
			insert into recon_records
				(ledger, source, record_id, reference, asset, amount, direction, occurred_at, raw)
			values ($1, $2, $3, $4, $5, $6, nullif($7, ''), $8, $9)
			on conflict (ledger, source, record_id) do nothing`,
			ledgerName, sourceID, r.ID, reference, r.Asset, numeric(r.Amount),
			string(r.Direction), occurred, rawOrNull(r.Raw))
		if err != nil {
			return staged, fmt.Errorf("stage %s/%s: %w", sourceID, r.ID, err)
		}
		staged += int(tag.RowsAffected())
	}
	return staged, nil
}

// Pull fetches from a source and stages what it returns, which is the whole
// job of a scheduled reconciliation run.
func Pull(ctx context.Context, db DB, ledgerName string, s Source, since time.Time) (staged int, err error) {
	records, err := s.Fetch(ctx, since)
	if err != nil {
		return 0, fmt.Errorf("fetch from %s: %w", s.ID(), err)
	}
	return Ingest(ctx, db, ledgerName, s.ID(), records)
}

func rawOrNull(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}

// pgx has no native mapping between numeric and *big.Int. pgtype.Numeric
// carries an unscaled *big.Int plus an exponent, so with Exp 0 it is exactly
// an integer and round trips without going near a float.
func numeric(i *big.Int) pgtype.Numeric {
	return pgtype.Numeric{Int: i, Exp: 0, Valid: true}
}
