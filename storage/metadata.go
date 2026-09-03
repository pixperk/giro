package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pixperk/giro/ledger"
)

// Metadata is stored on the transactions and accounts rows and its history
// lives in the log, which is already ordered, complete and hash chained. a
// separate revision table would be a second append only history of the same
// events, with weaker guarantees than the one that is chained.
//
// so these methods update the projection and append an entry, exactly as
// committing a transaction does.
//
// postgres does the merge and the delete natively, `metadata || $x` and
// `metadata - $key`, so there is no read modify write in go and two writers
// touching different keys do not clobber each other.

var ErrEmptyMetadata = errors.New("no metadata given")

func (s *Store) SetTransactionMetadata(ctx context.Context, id int64, m ledger.Metadata) (*ledger.Transaction, error) {
	if err := validateMetadata(m); err != nil {
		return nil, err
	}

	_, err := s.mutateMetadata(ctx,
		func(ctx context.Context, tx pgx.Tx) (bool, error) {
			return s.lockAndMerge(ctx, tx, "transactions", "id", "", id, m)
		},
		ledger.LogSetMetadata,
		ledger.SetMetadataPayload{
			TargetType: ledger.TargetTransaction,
			TargetID:   strconv.FormatInt(id, 10),
			Metadata:   m,
		})
	if err != nil {
		return nil, err
	}
	return s.GetTransaction(ctx, id)
}

func (s *Store) DeleteTransactionMetadataKey(ctx context.Context, id int64, key string) (*ledger.Transaction, error) {
	if key == "" {
		return nil, ledger.ErrEmptyMetadataKey
	}

	if _, err := s.mutateMetadata(ctx,
		func(ctx context.Context, tx pgx.Tx) (bool, error) {
			return s.lockAndDeleteKey(ctx, tx, "transactions", "id", "", id, key)
		},
		ledger.LogDeleteMetadata,
		ledger.DeleteMetadataPayload{
			TargetType: ledger.TargetTransaction,
			TargetID:   strconv.FormatInt(id, 10),
			Key:        key,
		}); err != nil {
		return nil, err
	}
	return s.GetTransaction(ctx, id)
}

// SetAccountMetadata creates the account row when it does not exist.
//
// accounts are never registered, so tagging one before its first payment is
// exactly when a caller would want to: attaching a user id to a wallet is not
// something to defer until money has moved through it.
func (s *Store) SetAccountMetadata(ctx context.Context, address ledger.Address, m ledger.Metadata) (*ledger.Account, error) {
	if !address.Valid() {
		return nil, fmt.Errorf("%w: invalid address %q", ledger.ErrInvalidSourceAddress, address)
	}
	if err := validateMetadata(m); err != nil {
		return nil, err
	}

	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}

	if _, err := s.mutateMetadata(ctx,
		func(ctx context.Context, tx pgx.Tx) (bool, error) {
			now := time.Now().UTC().Truncate(time.Microsecond)
			tag, err := tx.Exec(ctx, `
				insert into accounts (ledger, address, address_array, first_usage, insertion_date, updated_at, metadata)
				values ($1, $2, $3, $4, $4, $4, $5)
				on conflict (ledger, address) do update
				   set metadata = accounts.metadata || excluded.metadata,
				       updated_at = excluded.updated_at
				 where not (accounts.metadata @> excluded.metadata)`,
				s.ledger, address, address.Segments(), now, raw)
			if err != nil {
				return false, err
			}
			return tag.RowsAffected() > 0, nil
		},
		ledger.LogSetMetadata,
		ledger.SetMetadataPayload{
			TargetType: ledger.TargetAccount,
			TargetID:   string(address),
			Metadata:   m,
		}); err != nil {
		return nil, err
	}
	return s.GetAccount(ctx, address)
}

func (s *Store) DeleteAccountMetadataKey(ctx context.Context, address ledger.Address, key string) (*ledger.Account, error) {
	if key == "" {
		return nil, ledger.ErrEmptyMetadataKey
	}

	if _, err := s.mutateMetadata(ctx,
		func(ctx context.Context, tx pgx.Tx) (bool, error) {
			return s.lockAndDeleteKey(ctx, tx, "accounts", "address", touchUpdatedAt, address, key)
		},
		ledger.LogDeleteMetadata,
		ledger.DeleteMetadataPayload{
			TargetType: ledger.TargetAccount,
			TargetID:   string(address),
			Key:        key,
		}); err != nil {
		return nil, err
	}
	return s.GetAccount(ctx, address)
}

// the shape every metadata change shares.
//
// when the change is a no-op the log is left alone. a client retrying an
// identical write is common, and recording it would fill the chain with
// entries that describe nothing happening. the trail records changes, not
// attempts.
func (s *Store) mutateMetadata(
	ctx context.Context,
	apply func(context.Context, pgx.Tx) (bool, error),
	typ ledger.LogType,
	payload any,
) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	changed, err := apply(ctx, tx)
	if err != nil {
		return false, err
	}

	if changed {
		a, err := s.allocateLogID(ctx, tx)
		if err != nil {
			return false, err
		}
		date := time.Now().UTC().Truncate(time.Microsecond)
		if err := s.appendLogEntry(ctx, tx, a, typ, payload, date, "", ""); err != nil {
			return false, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return changed, nil
}

// accounts carries updated_at and transactions does not, deliberately: the log
// records when metadata changed, so a column saying the same thing on a table
// nothing reads it from would be dead weight. accounts already has one, written
// by the commit path when it upserts.
const touchUpdatedAt = ", updated_at = now()"

// locks the target row, then merges. the lock is what stops two concurrent
// metadata writes to the same row interleaving between the existence check and
// the update.
func (s *Store) lockAndMerge(ctx context.Context, tx pgx.Tx, table, idColumn, extraSet string, id any, m ledger.Metadata) (bool, error) {
	if err := s.lockTarget(ctx, tx, table, idColumn, id); err != nil {
		return false, err
	}

	raw, err := json.Marshal(m)
	if err != nil {
		return false, err
	}

	// the guard makes an identical write a no-op rather than a redundant log
	// entry
	tag, err := tx.Exec(ctx, `update `+table+`
		   set metadata = metadata || $3`+extraSet+`
		 where ledger = $1 and `+idColumn+` = $2
		   and not (metadata @> $3)`,
		s.ledger, id, raw)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func (s *Store) lockAndDeleteKey(ctx context.Context, tx pgx.Tx, table, idColumn, extraSet string, id any, key string) (bool, error) {
	if err := s.lockTarget(ctx, tx, table, idColumn, id); err != nil {
		return false, err
	}

	// jsonb_exists rather than the ? operator, which is ambiguous next to
	// parameter placeholders
	tag, err := tx.Exec(ctx, `update `+table+`
		   set metadata = metadata - $3`+extraSet+`
		 where ledger = $1 and `+idColumn+` = $2
		   and jsonb_exists(metadata, $3)`,
		s.ledger, id, key)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// a missing target and an unchanged one both affect zero rows, so existence is
// checked separately rather than inferred.
func (s *Store) lockTarget(ctx context.Context, tx pgx.Tx, table, idColumn string, id any) error {
	var exists bool
	err := tx.QueryRow(ctx,
		`select true from `+table+` where ledger = $1 and `+idColumn+` = $2 for update`,
		s.ledger, id).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s %v", ErrNotFound, table, id)
	}
	return err
}

func validateMetadata(m ledger.Metadata) error {
	if len(m) == 0 {
		return ErrEmptyMetadata
	}
	return m.Validate()
}
