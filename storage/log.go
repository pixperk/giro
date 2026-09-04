package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pixperk/giro/ledger"
)

var (
	// a stored hash does not match the one recomputed from the entry and its
	// predecessor. either the row was edited or the chain was written wrongly.
	ErrChainBroken = errors.New("log chain broken")

	// the idempotency key was used before with different inputs. returning the
	// original result would be a silent success for a payment that never
	// happened, so this is an error.
	ErrIdempotencyMismatch = errors.New("idempotency key reused with different inputs")
)

// what the idempotency hash covers: the caller's request, exactly as given.
//
// the timestamp is the one supplied, not the one resolved to now(), otherwise
// two identical calls would hash differently and the guard would fire on every
// legitimate retry.
type idempotencyInput struct {
	Postings  ledger.Postings `json:"postings"`
	Timestamp time.Time       `json:"timestamp"`
	Reference string          `json:"reference"`
	Metadata  ledger.Metadata `json:"metadata"`
}

func idempotencyHash(p ledger.Postings, opts CommitOptions) (string, error) {
	// encoding/json orders struct fields by declaration and map keys
	// alphabetically, so this is deterministic for a given input.
	b, err := json.Marshal(idempotencyInput{
		Postings:  p,
		Timestamp: opts.Timestamp,
		Reference: opts.Reference,
		Metadata:  opts.Metadata,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// looks for a log already written under this idempotency key. returns nil when
// the key is unused.
func (s *Store) findByIdempotencyKey(ctx context.Context, tx pgx.Tx, key, wantHash string) (*ledger.Transaction, error) {
	var data []byte
	var gotHash string

	err := tx.QueryRow(ctx, `
		select data, idempotency_hash from logs
		where ledger = $1 and idempotency_key = $2`,
		s.ledger, key).Scan(&data, &gotHash)
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

	var t ledger.Transaction
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("decode stored transaction: %w", err)
	}
	return &t, nil
}

// appends one entry, chained to the previous hash, and moves the ledger's
// chain tip forward.
//
// data is hashed and stored as the same bytes. the column is json rather than
// jsonb precisely so those bytes survive the round trip, which is what lets
// verification hash the stored value directly.
func (s *Store) insertLog(ctx context.Context, tx pgx.Tx, l *ledger.Log) error {
	var key, hash *string
	if l.IdempotencyKey != "" {
		key, hash = &l.IdempotencyKey, &l.IdempotencyHash
	}

	// one statement, not two. the entry and the chain tip it advances are
	// independent writes -- neither reads the other -- so they cost one round
	// trip rather than two, and a commit is mostly round trips once the
	// database is on the far side of a network.
	//
	// the data modifying CTE runs both; postgres gives no ordering guarantee
	// between them and none is needed. both still pass through their own
	// triggers, the append-only guard on logs and the counter guard on
	// ledgers, exactly as they did as separate statements.
	_, err := tx.Exec(ctx, `
		with entry as (
			insert into logs (ledger, id, type, data, date, hash, idempotency_key, idempotency_hash)
			values ($1, $2, $3, $4, $5, $6, $7, $8)
		)
		update ledgers set last_log_hash = $6 where name = $1`,
		s.ledger, l.ID, string(l.Type), l.Data, l.Date, l.Hash, key, hash)
	return err
}

// VerifyLog walks the ledger's log in order and recomputes every hash.
//
// this is the check the chain exists for. it is not part of the write path:
// run it on demand, in tests, or on a schedule.
func (s *Store) VerifyLog(ctx context.Context) (checked int, err error) {
	rows, err := s.pool.Query(ctx,
		`select id, type, data, hash from logs where ledger = $1 order by id`, s.ledger)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var previous []byte
	var expectedID int64 = 1

	for rows.Next() {
		var id int64
		var kind string
		var data, stored []byte
		if err := rows.Scan(&id, &kind, &data, &stored); err != nil {
			return checked, err
		}

		// ids are allocated from a counter inside the transaction, so a gap
		// means an entry was deleted rather than rolled back.
		//
		// with one exception, and it has to declare itself. resuming a
		// restored ledger above every id it ever issued leaves a real gap
		// where the lost entries were, and pretending otherwise would mean
		// either reissuing their ids or a check that fails forever. so a
		// RECOVERY entry names the range it skipped and is believed only for
		// exactly that range. a gap in front of anything else, or in front of
		// a RECOVERY entry claiming a different range, is still an entry
		// somebody deleted.
		if id != expectedID {
			declared, err := declaredGap(kind, data)
			if err != nil {
				return checked, err
			}
			if declared == nil || declared.ResumedFrom != expectedID-1 || declared.SkippedThrough != id-1 {
				return checked, fmt.Errorf("%w: expected log %d, found %d", ErrChainBroken, expectedID, id)
			}
			expectedID = id
		}

		if want := ledger.ChainHash(previous, data); !bytes.Equal(want, stored) {
			return checked, fmt.Errorf("%w: log %d hashes to %x but stores %x",
				ErrChainBroken, id, want, stored)
		}

		previous = stored
		expectedID++
		checked++
	}
	return checked, rows.Err()
}

// declaredGap returns the recovery payload when this entry is one, so a gap in
// front of it can be checked against what it claims. Anything else returns
// nil, which the caller treats as a gap nobody accounted for.
func declaredGap(kind string, data []byte) (*ledger.RecoveryPayload, error) {
	if ledger.LogType(kind) != ledger.LogRecovery {
		return nil, nil
	}
	var p ledger.RecoveryPayload
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("%w: recovery entry does not decode: %w", ErrChainBroken, err)
	}
	return &p, nil
}
