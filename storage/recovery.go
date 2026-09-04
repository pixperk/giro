package storage

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pixperk/giro/ledger"
)

// Surviving a restore, which is the one failure the rest of this package
// cannot see.
//
// # The hazard
//
// Transaction and log ids are allocated by incrementing a counter on the
// ledgers row, gapless and monotonic. A trigger refuses to let that counter go
// backwards, because reusing an id would mean two different transactions
// answering to the same name and a hash chain that cannot be verified.
//
// That trigger fires on an UPDATE. A restore does not update anything -- it
// replaces the table, or the whole data directory -- so the counter goes back
// to whatever it was at the restore point and the trigger never sees it. The
// next commit then claims an id that has already been issued, and every system
// downstream holding "giro transaction 4291" now points at a different
// transaction than it did yesterday. Nothing else in this package notices:
// conservation still holds, the chain still verifies, the projection still
// agrees. The book is internally perfect and no longer means what it meant.
//
// # Why this needs something outside the database
//
// There is no way to detect it from inside the restored database. Everything
// that could remember the higher watermark -- the ledgers row, the log,
// verification_runs -- was restored along with it, consistently, to the same
// earlier moment.
//
// So the watermark has to be kept somewhere the restore does not reach: your
// monitoring, your deployment record, the output of the last verify run that
// was shipped to a log aggregator. Tip is what you record, and CheckTip is
// what compares it afterwards.
//
// The comparison is stronger than "is the number lower". It names an id and
// the hash at that id, so it answers the question that actually matters: is
// transaction 4291 still the same transaction it was? A restore that lost
// nothing answers yes. A fork answers no, and says so before anybody builds on
// it.

// Tip is a ledger's position: the ids allocated so far and the hash at the head
// of its log.
//
// Record it after a known good verify. It is small enough to paste into a
// deployment record and specific enough to prove a restore landed where you
// think it did.
type Tip struct {
	Ledger string
	TxID   int64
	LogID  int64
	Hash   []byte
}

// String renders a tip as ledger:logID:hash, which is what --expect-tip takes.
// Empty for a ledger nothing has been written to, because there is no position
// to record yet and a zero would look like one.
func (t Tip) String() string {
	if t.LogID == 0 {
		return t.Ledger + ":0:"
	}
	return fmt.Sprintf("%s:%d:%s", t.Ledger, t.LogID, base64.RawURLEncoding.EncodeToString(t.Hash))
}

// ParseTip reads what String wrote.
func ParseTip(s string) (Tip, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return Tip{}, fmt.Errorf("malformed tip %q, want ledger:logID:hash", s)
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return Tip{}, fmt.Errorf("malformed tip %q: log id: %w", s, err)
	}
	hash, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Tip{}, fmt.Errorf("malformed tip %q: hash: %w", s, err)
	}
	return Tip{Ledger: parts[0], LogID: id, Hash: hash}, nil
}

// ChainTip reads where this ledger has got to.
func (s *Store) ChainTip(ctx context.Context) (Tip, error) {
	t := Tip{Ledger: s.ledger}
	err := s.pool.QueryRow(ctx,
		`select last_tx_id, last_log_id, coalesce(last_log_hash, '') from ledgers where name = $1`,
		s.ledger).Scan(&t.TxID, &t.LogID, &t.Hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, fmt.Errorf("%w: %q", ErrLedgerNotFound, s.ledger)
	}
	if err != nil {
		return t, fmt.Errorf("read chain tip: %w", err)
	}
	return t, nil
}

// A restore that went wrong, in the three ways it can go wrong.
var (
	// ErrChainBehind: the ledger has fewer entries than the recorded tip. The
	// restore lost transactions, and any commit from here reuses their ids.
	ErrChainBehind = errors.New("ledger is behind the recorded tip")

	// ErrChainForked: the entry at the recorded id exists with a different
	// hash. Ids have already been reused, so an id no longer names what it
	// used to.
	ErrChainForked = errors.New("ledger has forked from the recorded tip")
)

// TipMismatch says which of the two happened, with both positions.
type TipMismatch struct {
	Expected Tip
	Actual   Tip
	Kind     error // ErrChainBehind or ErrChainForked
}

func (e *TipMismatch) Error() string {
	switch {
	case errors.Is(e.Kind, ErrChainBehind):
		return fmt.Sprintf(
			"ledger %s is at log %d but was recorded at %d: a restore lost %d entries, "+
				"and committing from here reuses their ids",
			e.Actual.Ledger, e.Actual.LogID, e.Expected.LogID, e.Expected.LogID-e.Actual.LogID)
	default:
		return fmt.Sprintf(
			"ledger %s log entry %d does not match the recorded hash: ids have been reused, "+
				"so entry %d is no longer the entry it was",
			e.Actual.Ledger, e.Expected.LogID, e.Expected.LogID)
	}
}

func (e *TipMismatch) Unwrap() error { return e.Kind }

// CheckTip compares the ledger against a tip recorded before a restore.
//
// Run it immediately after restoring and before letting anything write. It is
// the only check in this package that can tell a sound restore from one that
// silently reassigned identities, because it is the only one holding
// information the restore could not reach.
func (s *Store) CheckTip(ctx context.Context, expected Tip) error {
	actual, err := s.ChainTip(ctx)
	if err != nil {
		return err
	}
	// nothing was recorded, so there is nothing to be behind
	if expected.LogID == 0 {
		return nil
	}

	var hash []byte
	err = s.pool.QueryRow(ctx,
		`select hash from logs where ledger = $1 and id = $2`,
		s.ledger, expected.LogID).Scan(&hash)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// the entry is not here at all: the restore is short of the watermark
		return &TipMismatch{Expected: expected, Actual: actual, Kind: ErrChainBehind}
	case err != nil:
		return fmt.Errorf("read log entry %d: %w", expected.LogID, err)
	}

	// the entry exists. is it the same entry?
	if !bytes.Equal(hash, expected.Hash) {
		return &TipMismatch{Expected: expected, Actual: actual, Kind: ErrChainForked}
	}
	return nil
}

// RecordRecovery resumes a restored ledger above every id it ever issued, and
// says so in the log.
//
// The counters cannot simply be bumped. Resuming above the watermark leaves a
// gap where the lost entries were, and a gap is exactly what VerifyLog exists
// to catch -- a missing id means an entry was deleted. So the gap is declared:
// this appends a RECOVERY entry naming the range it skipped, chained like
// every other entry, and verification accepts a gap only when the entry after
// it declares that gap. An undeclared gap is still a broken chain.
//
// That is the same rule as the rest of the ledger. Nothing recorded is edited,
// and a correction is something you append. An operator quietly moving a
// counter is an edit wearing a different hat.
//
// The skipped ids are never reissued. They belonged to transactions that
// really happened, and leaving them unused is what stops a replay colliding
// with a real transaction this database no longer remembers.
//
// Run it after CheckTip has told you the restore came back short, before
// letting anything write. It is a no-op if the ledger is already past the
// watermark.
func (s *Store) RecordRecovery(ctx context.Context, watermark Tip, note string) error {
	current, err := s.ChainTip(ctx)
	if err != nil {
		return err
	}
	if current.LogID >= watermark.LogID && current.TxID >= watermark.TxID {
		return nil // already past it; nothing was lost
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// re-read under the row lock: two operators running this at once must not
	// both decide they are the one appending the entry
	var lastTx, lastLog int64
	var previous []byte
	if err := tx.QueryRow(ctx,
		`select last_tx_id, last_log_id, coalesce(last_log_hash, '')
		   from ledgers where name = $1 for update`,
		s.ledger).Scan(&lastTx, &lastLog, &previous); err != nil {
		return fmt.Errorf("lock ledger %s: %w", s.ledger, err)
	}
	if lastLog >= watermark.LogID && lastTx >= watermark.TxID {
		return nil
	}

	payload, err := json.Marshal(ledger.RecoveryPayload{
		ResumedFrom:    lastLog,
		SkippedThrough: watermark.LogID,
		Note:           note,
	})
	if err != nil {
		return fmt.Errorf("encode recovery payload: %w", err)
	}

	// the entry lands immediately above the watermark, so every id the lost
	// entries held stays unused for good
	entry := &ledger.Log{
		ID:   max64(lastLog, watermark.LogID) + 1,
		Type: ledger.LogRecovery,
		Data: payload,
		Date: time.Now().UTC(),
	}
	entry.Hash = ledger.ChainHash(previous, entry.Data)

	if _, err := tx.Exec(ctx, `
		update ledgers
		   set last_tx_id  = greatest(last_tx_id, $2),
		       last_log_id = $3
		 where name = $1`,
		s.ledger, max64(lastTx, watermark.TxID), entry.ID); err != nil {
		return fmt.Errorf("advance ids for %s: %w", s.ledger, err)
	}
	if err := s.insertLog(ctx, tx, entry); err != nil {
		return fmt.Errorf("append recovery entry: %w", err)
	}
	return tx.Commit(ctx)
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
