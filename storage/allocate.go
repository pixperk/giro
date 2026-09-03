package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pixperk/giro/ledger"
)

// Id allocation and log appending.
//
// both counters and the chain tip come from one exclusive lock on the ledgers
// row, which is why gapless ids cost nothing on top of a synchronous hash
// chain: the lock the chain needs is the lock the ids need.

type allocation struct {
	transactionID int64
	logID         int64
	previousHash  []byte
}

// one statement takes both counters and the chain tip, from a row this
// transaction already holds an exclusive lock on. ids come from a counter
// rather than a sequence so a rollback un-allocates them and the log has no
// gaps, which is what makes a missing entry detectable during verification.
func (s *Store) allocate(ctx context.Context, tx pgx.Tx) (allocation, error) {
	var a allocation
	err := tx.QueryRow(ctx, `
		update ledgers
		   set last_tx_id = last_tx_id + 1,
		       last_log_id = last_log_id + 1
		 where name = $1
		returning last_tx_id, last_log_id, last_log_hash`,
		s.ledger).Scan(&a.transactionID, &a.logID, &a.previousHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, fmt.Errorf("%w: %q", ErrLedgerNotFound, s.ledger)
	}
	if err != nil {
		return a, fmt.Errorf("allocate ids: %w", err)
	}
	return a, nil
}

// serialise the transaction once, hash those exact bytes, store both.
func (s *Store) appendLog(ctx context.Context, tx pgx.Tx, t *ledger.Transaction, a allocation, key, ikHash string) error {
	return s.appendLogEntry(ctx, tx, a, ledger.LogNewTransaction, t, t.InsertedAt, key, ikHash)
}

// the shared tail of every mutation: marshal the payload, chain its hash onto
// the previous entry, insert, and move the ledger's chain tip forward.
func (s *Store) appendLogEntry(
	ctx context.Context, tx pgx.Tx, a allocation,
	typ ledger.LogType, payload any, date time.Time, key, ikHash string,
) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	return s.insertLog(ctx, tx, &ledger.Log{
		ID:              a.logID,
		Type:            typ,
		Date:            date,
		Data:            data,
		Hash:            ledger.ChainHash(a.previousHash, data),
		IdempotencyKey:  key,
		IdempotencyHash: ikHash,
	})
}

// allocateLogID takes only a log id, for mutations that are not transactions.
//
// the same exclusive lock on the ledgers row, so metadata changes and commits
// serialise against each other and the chain stays ordered.
func (s *Store) allocateLogID(ctx context.Context, tx pgx.Tx) (allocation, error) {
	var a allocation
	err := tx.QueryRow(ctx, `
		update ledgers
		   set last_log_id = last_log_id + 1
		 where name = $1
		returning last_log_id, last_log_hash`,
		s.ledger).Scan(&a.logID, &a.previousHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, fmt.Errorf("%w: %q", ErrLedgerNotFound, s.ledger)
	}
	if err != nil {
		return a, fmt.Errorf("allocate log id: %w", err)
	}
	return a, nil
}
