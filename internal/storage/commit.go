package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pixperk/giro/internal/ledger"
)

type CommitOptions struct {
	// when the movement happened economically. zero means now.
	Timestamp time.Time
	// the caller's own identifier, unique per ledger when present.
	Reference string
	Metadata  ledger.Metadata
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

	for attempt := range maxAttempts {
		tx, err := s.commitOnce(ctx, p, opts)
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

func (s *Store) commitOnce(ctx context.Context, p ledger.Postings, opts CommitOptions) (*ledger.Transaction, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	// no-op once Commit has run. this is the only path that undoes the zero
	// volume rows created while taking locks.
	defer tx.Rollback(ctx)

	// already sorted by (account, asset) in the domain layer. that ordering is
	// the lock order, and it is deterministic across processes.
	updates := p.VolumeUpdates()

	before, err := s.lockVolumes(ctx, tx, updates)
	if err != nil {
		return nil, err
	}

	if s.afterLock != nil {
		s.afterLock()
	}

	if err := checkBalances(before, updates); err != nil {
		return nil, err
	}

	if err := s.applyVolumes(ctx, tx, updates); err != nil {
		return nil, err
	}

	timestamp := opts.Timestamp
	if timestamp.IsZero() {
		timestamp = time.Now()
	}
	timestamp = timestamp.UTC()

	id, err := s.allocateTransactionID(ctx, tx)
	if err != nil {
		return nil, err
	}

	transaction := &ledger.Transaction{
		ID:                id,
		Postings:          p,
		Timestamp:         timestamp,
		Reference:         opts.Reference,
		Metadata:          opts.Metadata,
		PostCommitVolumes: postCommitVolumes(before, updates),
	}

	if err := s.insertTransaction(ctx, tx, transaction); err != nil {
		return nil, err
	}
	if err := s.upsertAccounts(ctx, tx, updates, timestamp); err != nil {
		return nil, err
	}
	if err := s.insertMoves(ctx, tx, transaction, before); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return transaction, nil
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

func (s *Store) allocateTransactionID(ctx context.Context, tx pgx.Tx) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx,
		`update ledgers set last_tx_id = last_tx_id + 1 where name = $1 returning last_tx_id`,
		s.ledger).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: %q", ErrLedgerNotFound, s.ledger)
	}
	if err != nil {
		return 0, fmt.Errorf("allocate transaction id: %w", err)
	}
	return id, nil
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
		endpoints(t.Postings, func(p ledger.Posting) string { return p.Source }),
		endpoints(t.Postings, func(p ledger.Posting) string { return p.Destination }),
		pcv,
	).Scan(&t.InsertedAt)

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
		if len(addresses) == 0 || addresses[len(addresses)-1] != u.Account {
			addresses = append(addresses, u.Account)
		}
	}

	segments := make([][]string, len(addresses))
	for i, a := range addresses {
		segments[i] = ledger.Segments(a)
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
	defer results.Close()
	for range addresses {
		if _, err := results.Exec(); err != nil {
			return fmt.Errorf("upsert accounts: %w", err)
		}
	}
	return results.Close()
}

// two rows per posting, one per side, each carrying the account's volumes
// immediately after that move.
//
// the snapshots are accumulated forward from the pre transaction state, so an
// account appearing in several postings gets a different running value on each
// of its moves rather than the final one repeated.
//
// pcev is left null. it is only knowable once effective date ordering is
// implemented, and null means "not computed yet" rather than zero.
func (s *Store) insertMoves(ctx context.Context, tx pgx.Tx, t *ledger.Transaction, before map[key]ledger.Volumes) error {
	running := make(map[key]ledger.Volumes, len(before))
	for k, v := range before {
		running[k] = ledger.Volumes{
			Input:  new(big.Int).Set(v.Input),
			Output: new(big.Int).Set(v.Output),
		}
	}

	batch := &pgx.Batch{}
	queue := func(address, asset string, amount *big.Int, isSource bool) {
		k := key{address, asset}
		v := running[k]
		if isSource {
			v.Output = new(big.Int).Add(v.Output, amount)
		} else {
			v.Input = new(big.Int).Add(v.Input, amount)
		}
		running[k] = v

		batch.Queue(`
			insert into moves
				(ledger, tx_id, address, asset, amount, is_source,
				 effective_date, insertion_date, pcv_input, pcv_output)
			values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			s.ledger, t.ID, address, asset, numeric(amount), isSource,
			t.Timestamp, t.InsertedAt, numeric(v.Input), numeric(v.Output))
	}

	for _, p := range t.Postings {
		queue(p.Source, p.Asset, p.Amount, true)
		queue(p.Destination, p.Asset, p.Amount, false)
	}

	results := tx.SendBatch(ctx, batch)
	defer results.Close()
	for range len(t.Postings) * 2 {
		if _, err := results.Exec(); err != nil {
			return fmt.Errorf("insert moves: %w", err)
		}
	}
	return results.Close()
}

const (
	deadlockDetected     = "40P01"
	serializationFailure = "40001"
	uniqueViolation      = "23505"
)

// only contention is retryable. a business rejection retried ten times just
// fails ten times more slowly.
func retryable(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == deadlockDetected || pgErr.Code == serializationFailure
}

func backoff(ctx context.Context, attempt int) error {
	// jittered, so retries from a thundering herd do not line up again
	d := time.Duration(1<<attempt)*time.Millisecond + time.Duration(rand.IntN(2000))*time.Microsecond
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
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
