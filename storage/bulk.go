package storage

// Committing several transactions as one business event.
//
// This is atomic, and only atomic. A best effort batch is N requests with fewer
// round trips, which a caller can do in a loop; all or nothing is the thing
// that cannot be built out of single commits.

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pixperk/giro/ledger"
)

// an unbounded batch holds locks on an unbounded number of rows for as long as
// it takes to write them all, on a table other transactions are queueing for.
const MaxBatchSize = 100

var ErrBatchTooLarge = fmt.Errorf("batch exceeds %d transactions", MaxBatchSize)

// one entry in a batch. the same inputs a single commit takes, minus the
// options that belong to the request as a whole.
type BatchItem struct {
	Postings  ledger.Postings
	Timestamp time.Time
	Reference string
	Metadata  ledger.Metadata
}

// CommitBatch applies every transaction or none of them.
//
// The idempotency key, if given, covers the whole batch: replaying it returns
// the transactions the first attempt created rather than committing them again.
func (s *Store) CommitBatch(ctx context.Context, items []BatchItem, opts CommitOptions) ([]*ledger.Transaction, error) {
	if len(items) == 0 {
		return nil, ErrNoPostings
	}
	if len(items) > MaxBatchSize {
		return nil, ErrBatchTooLarge
	}
	for i, item := range items {
		if len(item.Postings) == 0 {
			return nil, &BatchItemError{Index: i, Err: ErrNoPostings}
		}
		if j, err := item.Postings.Validate(); err != nil {
			return nil, &BatchItemError{Index: i, Err: &PostingError{Index: j, Err: err}}
		}
		if err := s.checkAssets(ctx, item.Postings); err != nil {
			return nil, &BatchItemError{Index: i, Err: err}
		}
	}

	ikHash, err := batchIdempotencyHash(items, opts)
	if err != nil {
		return nil, err
	}

	for attempt := range maxAttempts {
		out, err := s.commitBatchOnce(ctx, items, opts, ikHash, attempt)
		if err == nil {
			return out, nil
		}
		if !retryable(err) {
			return nil, err
		}
		s.retries.Add(1)
		if err := backoff(ctx, attempt); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("giving up after %d attempts, contention on ledger %q", maxAttempts, s.ledger)
}

func (s *Store) commitBatchOnce(ctx context.Context, items []BatchItem, opts CommitOptions, ikHash string, attempt int) ([]*ledger.Transaction, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	if opts.IdempotencyKey != "" {
		existing, err := s.findBatchByIdempotencyKey(ctx, tx, opts.IdempotencyKey, ikHash)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			return existing, nil
		}
	}

	// take every lock the batch will need, once, in sorted order.
	//
	// without this each item would lock its own accounts as it ran, so two
	// batches holding overlapping accounts in different item orders would
	// deadlock. sorting within an item is not enough: the ordering has to hold
	// across the whole transaction.
	if err := s.lockBatch(ctx, tx, items); err != nil {
		return nil, err
	}

	out := make([]*ledger.Transaction, 0, len(items))
	for i, item := range items {
		metadata := item.Metadata
		if opts.IdempotencyKey != "" {
			// tag every member so a replay can find the whole batch. the key
			// lives on one log entry, but the batch is many transactions.
			metadata = withBatchTag(metadata, ikHash)
		}

		transaction, alloc, err := s.applyTransaction(ctx, tx, item.Postings, applyOptions{
			Timestamp: item.Timestamp,
			Reference: item.Reference,
			Metadata:  metadata,
		})
		if err != nil {
			return nil, &BatchItemError{Index: i, Err: err}
		}

		// the key goes on the first entry only: it identifies the request, and
		// the unique index would reject it on the second.
		var key, hash string
		if i == 0 {
			key, hash = opts.IdempotencyKey, ikHash
		}
		if err := s.appendLog(ctx, tx, transaction, alloc, key, hash); err != nil {
			if replayed, e := s.batchIdempotencyRace(ctx, opts.IdempotencyKey, ikHash, err); replayed != nil || e != nil {
				return replayed, e
			}
			return nil, &BatchItemError{Index: i, Err: err}
		}
		out = append(out, transaction)
	}

	if s.beforeCommit != nil {
		if err := s.beforeCommit(attempt); err != nil {
			return nil, err
		}
	}
	if opts.DryRun {
		return out, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return out, nil
}

// every (account, asset) the batch touches, deduplicated and sorted, locked in
// one statement before any item runs.
func (s *Store) lockBatch(ctx context.Context, tx pgx.Tx, items []BatchItem) error {
	seen := map[key]bool{}
	var all []ledger.VolumeUpdate

	for _, item := range items {
		for _, u := range item.Postings.VolumeUpdates() {
			k := key{u.Account, u.Asset}
			if seen[k] {
				continue
			}
			seen[k] = true
			all = append(all, u)
		}
	}

	slices.SortFunc(all, func(a, b ledger.VolumeUpdate) int {
		return cmp.Or(
			cmp.Compare(a.Account, b.Account),
			cmp.Compare(a.Asset, b.Asset),
		)
	})

	_, err := s.lockVolumes(ctx, tx, all)
	return err
}

// BatchItemError says which entry in a batch failed. without the index, a
// rejection somewhere in five hundred transactions is a needle in a haystack.
type BatchItemError struct {
	Index int
	Err   error
}

func (e *BatchItemError) Error() string {
	return fmt.Sprintf("transactions[%d]: %v", e.Index, e.Err)
}

func (e *BatchItemError) Unwrap() error { return e.Err }

func withBatchTag(m ledger.Metadata, hash string) ledger.Metadata {
	out := make(ledger.Metadata, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	out[ledger.BatchKey] = hash
	return out
}

// the hash covers every item and the request level options, so replaying a key
// with a different batch is caught the same way it is for a single commit.
func batchIdempotencyHash(items []BatchItem, opts CommitOptions) (string, error) {
	if opts.IdempotencyKey == "" {
		return "", nil
	}

	// a BatchItem is exactly the shape the idempotency hash covers, so this is
	// a conversion rather than a restatement. if the two ever diverge this
	// stops compiling, which is the right moment to think about whether the
	// hash should still cover the same thing.
	inputs := make([]idempotencyInput, len(items))
	for i, item := range items {
		inputs[i] = idempotencyInput(item)
	}

	b, err := json.Marshal(inputs)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// finds a batch already committed under this key. nil when the key is unused.
func (s *Store) findBatchByIdempotencyKey(ctx context.Context, tx pgx.Tx, key, wantHash string) ([]*ledger.Transaction, error) {
	var gotHash string
	err := tx.QueryRow(ctx,
		`select idempotency_hash from logs where ledger = $1 and idempotency_key = $2`,
		s.ledger, key).Scan(&gotHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read idempotency key: %w", err)
	}
	if gotHash != wantHash {
		return nil, fmt.Errorf("%w: key %q was used with hash %s, this request hashes to %s",
			ErrIdempotencyMismatch, key, gotHash, wantHash)
	}

	rows, err := tx.Query(ctx, `
		select id, timestamp, inserted_at, reverted_at, coalesce(reference, ''),
		       postings, metadata, pc_volumes
		  from transactions
		 where ledger = $1 and metadata->>$2 = $3
		 order by id`,
		s.ledger, ledger.BatchKey, wantHash)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*ledger.Transaction
	for rows.Next() {
		var t ledger.Transaction
		var postings, metadata, pcv []byte
		if err := rows.Scan(&t.ID, &t.Timestamp, &t.InsertedAt, &t.RevertedAt, &t.Reference,
			&postings, &metadata, &pcv); err != nil {
			return nil, err
		}
		if err := hydrateTransaction(&t, postings, metadata, pcv); err != nil {
			return nil, err
		}
		out = append(out, &t)
	}
	return out, rows.Err()
}

// two concurrent replays can both miss the lookup above; the unique index
// decides, and the loser reads the winner's batch.
func (s *Store) batchIdempotencyRace(ctx context.Context, key, ikHash string, err error) ([]*ledger.Transaction, error) {
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
	defer tx.Rollback(ctx)

	return s.findBatchByIdempotencyKey(ctx, tx, key, ikHash)
}
