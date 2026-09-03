package storage

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pixperk/giro/ledger"
)

var ErrAlreadyReverted = errors.New("transaction already reverted")

type RevertOptions struct {
	// date the reversal with the original's effective date rather than now.
	//
	// off by default. a reversal is something that happens when it happens,
	// and backdating one rewrites what historical balances say about a period
	// that has probably already been reported on.
	AtEffectiveDate bool

	// commit even if an account other than world ends below zero.
	//
	// a reversal can legitimately fail: if the money has since been spent, it
	// is not there to give back, and forcing it manufactures a negative
	// balance. this exists for an operator who has decided that is the lesser
	// problem, and should be hard to reach.
	Force bool
}

// Reversal is the pair: what was undone, and what undid it.
type Reversal struct {
	Original *ledger.Transaction `json:"original"`
	Reversal *ledger.Transaction `json:"reversal"`
}

// RevertTransaction corrects a transaction by committing its inverse.
//
// nothing is edited or deleted. the original keeps its postings forever and
// gains a reverted_at mark saying a correction exists; the correction is an
// ordinary transaction with its own id, going through the same locking,
// balance checks and log append as any other.
func (s *Store) RevertTransaction(ctx context.Context, id int64, opts RevertOptions) (*Reversal, error) {
	for attempt := range maxAttempts {
		result, err := s.revertOnce(ctx, id, opts)
		if err == nil {
			return result, nil
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

func (s *Store) revertOnce(ctx context.Context, id int64, opts RevertOptions) (*Reversal, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	original, err := s.lockTransaction(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if original.RevertedAt != nil {
		return nil, fmt.Errorf("%w: transaction %d was reverted at %s",
			ErrAlreadyReverted, id, original.RevertedAt.Format(time.RFC3339))
	}

	timestamp := time.Now()
	if opts.AtEffectiveDate {
		timestamp = original.Timestamp
	}

	// Reverse swaps both sides of every posting and reverses their order. the
	// order matters: keeping it would pay the first account back before the
	// last had returned anything, so an intermediate account dips negative and
	// a reversal that should succeed fails its balance check.
	reversal, alloc, err := s.applyTransaction(ctx, tx, original.Postings.Reverse(), applyOptions{
		Timestamp: timestamp,
		Force:     opts.Force,
		Metadata:  ledger.Metadata{ledger.RevertsKey: strconv.FormatInt(id, 10)},
	})
	if err != nil {
		return nil, err
	}

	// stamped inside the same transaction as the reversal, which is what stops
	// two concurrent reverts both passing the check above and refunding twice.
	revertedAt := reversal.Timestamp
	if _, err := tx.Exec(ctx,
		`update transactions set reverted_at = $3 where ledger = $1 and id = $2`,
		s.ledger, id, revertedAt); err != nil {
		return nil, fmt.Errorf("mark reverted: %w", err)
	}
	original.RevertedAt = &revertedAt

	if err := s.appendLogEntry(ctx, tx, alloc, ledger.LogRevertedTransaction,
		ledger.RevertedTransactionPayload{RevertedTransactionID: id, Transaction: reversal},
		reversal.InsertedAt, "", ""); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return &Reversal{Original: original, Reversal: reversal}, nil
}

// takes an exclusive lock on the row so the reverted_at check and the stamp
// that follows cannot be interleaved by another revert of the same id.
func (s *Store) lockTransaction(ctx context.Context, tx pgx.Tx, id int64) (*ledger.Transaction, error) {
	var t ledger.Transaction
	var postings, metadata, pcv []byte

	err := tx.QueryRow(ctx, `
		select id, timestamp, inserted_at, reverted_at, coalesce(reference, ''),
		       postings, metadata, pc_volumes
		  from transactions
		 where ledger = $1 and id = $2
		   for update`,
		s.ledger, id,
	).Scan(&t.ID, &t.Timestamp, &t.InsertedAt, &t.RevertedAt, &t.Reference,
		&postings, &metadata, &pcv)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: transaction %d", ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}

	if err := hydrateTransaction(&t, postings, metadata, pcv); err != nil {
		return nil, err
	}
	return &t, nil
}
