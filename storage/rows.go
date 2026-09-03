package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pixperk/giro/ledger"
)

// The individual inserts a commit performs, other than its moves.

// two concurrent requests carrying the same idempotency key can both miss the
// fast path. the unique index catches the loser here, and the right answer is
// the winner's transaction rather than an error.
func (s *Store) idempotencyRace(ctx context.Context, key, ikHash string, err error) (*ledger.Transaction, error) {
	if key == "" {
		return nil, nil
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != uniqueViolation {
		return nil, nil
	}

	tx, beginErr := s.pool.Begin(ctx)
	if beginErr != nil {
		return nil, beginErr
	}
	defer func() { _ = tx.Rollback(ctx) }()

	return s.findByIdempotencyKey(ctx, tx, key, ikHash)
}

func (s *Store) insertTransaction(ctx context.Context, tx pgx.Tx, t *ledger.Transaction) error {
	postings, err := json.Marshal(t.Postings)
	if err != nil {
		return err
	}
	pcv, err := json.Marshal(t.PostCommitVolumes)
	if err != nil {
		return err
	}
	metadata, err := json.Marshal(orEmpty(t.Metadata))
	if err != nil {
		return err
	}

	var reference *string
	if t.Reference != "" {
		reference = &t.Reference
	}

	err = tx.QueryRow(ctx, `
		insert into transactions
			(ledger, id, timestamp, reference, postings, metadata, sources, destinations, pc_volumes)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		returning inserted_at`,
		s.ledger, t.ID, t.Timestamp, reference, postings, metadata,
		endpoints(t.Postings, func(p ledger.Posting) string { return string(p.Source) }),
		endpoints(t.Postings, func(p ledger.Posting) string { return string(p.Destination) }),
		pcv,
	).Scan(&t.InsertedAt)
	t.InsertedAt = t.InsertedAt.UTC()

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
		return fmt.Errorf("%w: %q", ErrDuplicateReference, t.Reference)
	}
	if err != nil {
		return fmt.Errorf("insert transaction: %w", err)
	}
	return nil
}

// accounts is not authoritative: an address absent from it still has a balance
// of zero. it exists for metadata and prefix queries, so this keeps it in step
// without ever gating a posting.
//
// first_usage takes the earliest effective date ever seen, which a backdated
// transaction can move earlier. insertion_date never moves.
func (s *Store) upsertAccounts(ctx context.Context, tx pgx.Tx, updates []ledger.VolumeUpdate, timestamp time.Time) error {
	// distinct accounts, still in the sorted order the updates arrived in, so
	// these row locks are taken in the same sequence by every transaction.
	addresses := make([]string, 0, len(updates))
	for _, u := range updates {
		if len(addresses) == 0 || addresses[len(addresses)-1] != string(u.Account) {
			addresses = append(addresses, string(u.Account))
		}
	}

	segments := make([][]string, len(addresses))
	for i, a := range addresses {
		segments[i] = ledger.Address(a).Segments()
	}

	batch := &pgx.Batch{}
	for i, address := range addresses {
		batch.Queue(`
			insert into accounts (ledger, address, address_array, first_usage, insertion_date, updated_at)
			values ($1, $2, $3, $4, now(), now())
			on conflict (ledger, address) do update
			   set first_usage = least(accounts.first_usage, excluded.first_usage),
			       updated_at  = now()`,
			s.ledger, address, segments[i], timestamp)
	}

	results := tx.SendBatch(ctx, batch)
	defer func() { _ = results.Close() }()
	for range addresses {
		if _, err := results.Exec(); err != nil {
			return fmt.Errorf("upsert accounts: %w", err)
		}
	}
	return results.Close()
}

// deduplicated and sorted, for the text[] columns the gin index answers
// containment queries against.
func endpoints(p ledger.Postings, get func(ledger.Posting) string) []string {
	out := make([]string, 0, len(p))
	for _, posting := range p {
		out = append(out, get(posting))
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func orEmpty(m ledger.Metadata) ledger.Metadata {
	if m == nil {
		return ledger.Metadata{}
	}
	return m
}
