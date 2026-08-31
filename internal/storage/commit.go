package storage

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pixperk/giro/internal/ledger"
)

// Committing a transaction: the retry loop, and the sequence that runs inside
// one database transaction.

type CommitOptions struct {
	// when the movement happened economically. zero means now.
	Timestamp time.Time
	// the caller's own identifier, unique per ledger when present.
	Reference string
	Metadata  ledger.Metadata

	// replaying this key returns the original transaction instead of creating
	// a second one. a network timeout after the server committed looks exactly
	// like a request that never arrived, so every write endpoint is eventually
	// called twice.
	IdempotencyKey string

	// run the whole commit path and then roll back, returning what would have
	// happened.
	//
	// this is the real path rather than a simulation, so it cannot drift from
	// what a real commit does: the locks are taken, the balances are checked
	// against live data, the volumes and moves are written. only the COMMIT is
	// replaced by a ROLLBACK.
	//
	// nothing is consumed. no id is allocated, no idempotency key is claimed,
	// no log entry survives. the id on the returned transaction is what it
	// would have been, not a reservation.
	DryRun bool
}

// postgres can still deadlock through index and foreign key locks that sorted
// ordering does not cover. a cap turns a pathological case into a visible
// error rather than a request that hangs holding a connection.
const maxAttempts = 10

// CommitTransaction applies an ordered list of postings atomically.
//
// the retry loop sits outside the database transaction on purpose: a deadlock
// or serialization failure invalidates every value read, including the
// balances that were checked, so recovery means starting again from the lock
// rather than replaying a statement.
func (s *Store) CommitTransaction(ctx context.Context, p ledger.Postings, opts CommitOptions) (*ledger.Transaction, error) {
	if len(p) == 0 {
		return nil, ErrNoPostings
	}
	if i, err := p.Validate(); err != nil {
		return nil, &PostingError{Index: i, Err: err}
	}

	ikHash, err := idempotencyHash(p, opts)
	if err != nil {
		return nil, err
	}

	for attempt := range maxAttempts {
		tx, err := s.commitOnce(ctx, p, opts, ikHash, attempt)
		if err == nil {
			return tx, nil
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

func (s *Store) commitOnce(ctx context.Context, p ledger.Postings, opts CommitOptions, ikHash string, attempt int) (*ledger.Transaction, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	// no-op once Commit has run. this is the only path that undoes the zero
	// volume rows created while taking locks.
	defer tx.Rollback(ctx)

	// fast path for a replayed request. the unique index on the key is what
	// makes this correct under a race: two concurrent replays can both miss
	// here, and the loser is caught at insert time below.
	if opts.IdempotencyKey != "" {
		existing, err := s.findByIdempotencyKey(ctx, tx, opts.IdempotencyKey, ikHash)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			return existing, nil
		}
	}

	transaction, alloc, err := s.applyTransaction(ctx, tx, p, applyOptions{
		Timestamp: opts.Timestamp,
		Reference: opts.Reference,
		Metadata:  opts.Metadata,
	})
	if err != nil {
		return nil, err
	}

	// the log entry goes in last, still inside the same transaction, so the
	// log and the projection it describes either both land or neither does.
	if err := s.appendLog(ctx, tx, transaction, alloc, opts.IdempotencyKey, ikHash); err != nil {
		if replayed, e := s.idempotencyRace(ctx, opts.IdempotencyKey, ikHash, err); replayed != nil || e != nil {
			return replayed, e
		}
		return nil, err
	}

	if s.beforeCommit != nil {
		if err := s.beforeCommit(attempt); err != nil {
			return nil, err
		}
	}

	if opts.DryRun {
		// the deferred rollback undoes everything above, including the id
		// allocation and the zero volume rows created while taking locks
		return transaction, nil
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return transaction, nil
}

type applyOptions struct {
	Timestamp time.Time
	Reference string
	Metadata  ledger.Metadata

	// commit even if an account other than world ends below zero. only a
	// reversal offers this, and only to an operator who means it.
	Force bool
}

// applyTransaction is everything a commit does except appending the log entry
// and committing: lock, check, apply, allocate, insert.
//
// it takes an open transaction rather than starting one, because a revert
// needs to do all of this and then stamp the original row, all atomically.
func (s *Store) applyTransaction(ctx context.Context, tx pgx.Tx, p ledger.Postings, opts applyOptions) (*ledger.Transaction, allocation, error) {
	// already sorted by (account, asset) in the domain layer. that ordering is
	// the lock order, and it is deterministic across processes.
	updates := p.VolumeUpdates()

	var alloc allocation

	before, err := s.lockVolumes(ctx, tx, updates)
	if err != nil {
		return nil, alloc, err
	}

	if s.afterLock != nil {
		s.afterLock()
	}

	if !opts.Force {
		if err := checkBalances(before, updates); err != nil {
			return nil, alloc, err
		}
	}

	if err := s.applyVolumes(ctx, tx, updates); err != nil {
		return nil, alloc, err
	}

	timestamp := opts.Timestamp
	if timestamp.IsZero() {
		timestamp = time.Now()
	}
	// postgres timestamptz holds microseconds. truncating here means the value
	// we return is the value that was stored, rather than one carrying
	// precision the database silently drops on the way in.
	//
	// this is invisible on macos, whose clock is already microsecond granular,
	// and shows up immediately on linux.
	timestamp = timestamp.UTC().Truncate(time.Microsecond)

	alloc, err = s.allocate(ctx, tx)
	if err != nil {
		return nil, alloc, err
	}

	transaction := &ledger.Transaction{
		ID:                alloc.transactionID,
		Postings:          p,
		Timestamp:         timestamp,
		Reference:         opts.Reference,
		Metadata:          opts.Metadata,
		PostCommitVolumes: postCommitVolumes(before, updates),
	}

	if err := s.insertTransaction(ctx, tx, transaction); err != nil {
		return nil, alloc, err
	}
	if err := s.upsertAccounts(ctx, tx, updates, timestamp); err != nil {
		return nil, alloc, err
	}
	if err := s.insertMoves(ctx, tx, transaction, before, updates); err != nil {
		return nil, alloc, err
	}
	return transaction, alloc, nil
}

// checkBalances rejects the transaction if any account other than world would
// end below zero.
//
// the check is on the final state, so an account may pass through zero within
// a transaction. it uses the current balance and never an effective date
// balance: the money either exists now or it does not.
func checkBalances(before map[key]ledger.Volumes, updates []ledger.VolumeUpdate) error {
	for _, u := range updates {
		if u.Account == ledger.WorldAccount {
			continue
		}
		v := before[key{u.Account, u.Asset}]
		input := new(big.Int).Add(v.Input, u.Input)
		output := new(big.Int).Add(v.Output, u.Output)

		if input.Cmp(output) < 0 {
			return &InsufficientFundsError{
				Account:   u.Account,
				Asset:     u.Asset,
				Available: v.Balance(),
				Requested: u.Output,
			}
		}
	}
	return nil
}

// the final state of every touched account, frozen at commit.
func postCommitVolumes(before map[key]ledger.Volumes, updates []ledger.VolumeUpdate) ledger.PostCommitVolumes {
	out := ledger.PostCommitVolumes{}
	for _, u := range updates {
		v := before[key{u.Account, u.Asset}]
		out.Set(u.Account, u.Asset, ledger.Volumes{
			Input:  new(big.Int).Add(v.Input, u.Input),
			Output: new(big.Int).Add(v.Output, u.Output),
		})
	}
	return out
}
