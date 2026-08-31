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
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/pixperk/giro/internal/ledger"
)

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
		endpoints(t.Postings, func(p ledger.Posting) string { return p.Source }),
		endpoints(t.Postings, func(p ledger.Posting) string { return p.Destination }),
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
	defer func() { _ = results.Close() }()
	for range addresses {
		if _, err := results.Exec(); err != nil {
			return fmt.Errorf("upsert accounts: %w", err)
		}
	}
	return results.Close()
}

// two rows per posting, one per side, each carrying the account's volumes
// immediately after that move, in both orderings.
//
// pcv accumulates forward from the pre transaction state in insertion order,
// so an account appearing in several postings gets a different running value on
// each of its moves rather than the final one repeated. it is written once and
// never touched again: it records what the ledger believed at the time.
//
// pcev is the same accumulation in effective date order, which is a different
// sequence whenever a transaction is backdated. it starts from the effective
// balance as of this transaction's timestamp rather than from the current one,
// and every move that already sits later in effective order is shifted by this
// transaction's delta.
func (s *Store) insertMoves(ctx context.Context, tx pgx.Tx, t *ledger.Transaction, before map[key]ledger.Volumes, updates []ledger.VolumeUpdate) error {
	effectiveBefore, err := s.effectiveVolumesAt(ctx, tx, updates, t.Timestamp)
	if err != nil {
		return err
	}

	running := copyVolumes(before)
	effective := copyVolumes(effectiveBefore)

	batch := &pgx.Batch{}
	queue := func(address, asset string, amount *big.Int, isSource bool) {
		k := key{address, asset}
		apply(running, k, amount, isSource)
		apply(effective, k, amount, isSource)

		v, e := running[k], effective[k]
		batch.Queue(`
			insert into moves
				(ledger, tx_id, address, asset, amount, is_source,
				 effective_date, insertion_date,
				 pcv_input, pcv_output, pcev_input, pcev_output)
			values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
			s.ledger, t.ID, address, asset, numeric(amount), isSource,
			t.Timestamp, t.InsertedAt,
			numeric(v.Input), numeric(v.Output), numeric(e.Input), numeric(e.Output))
	}

	for _, p := range t.Postings {
		queue(p.Source, p.Asset, p.Amount, true)
		queue(p.Destination, p.Asset, p.Amount, false)
	}

	results := tx.SendBatch(ctx, batch)
	for range len(t.Postings) * 2 {
		if _, err := results.Exec(); err != nil {
			_ = results.Close()
			return fmt.Errorf("insert moves: %w", err)
		}
	}
	if err := results.Close(); err != nil {
		return err
	}

	return s.shiftLaterEffectiveVolumes(ctx, tx, updates, t.Timestamp)
}

// the effective volumes of each touched account as of a date, summed from the
// moves that already sit at or before it.
//
// moves sharing this exact effective date count as before, because they were
// inserted first and effective order breaks ties by seq.
func (s *Store) effectiveVolumesAt(ctx context.Context, tx pgx.Tx, updates []ledger.VolumeUpdate, at time.Time) (map[key]ledger.Volumes, error) {
	addresses, assets := pairs(updates)

	rows, err := tx.Query(ctx, `
		select address, asset,
		       coalesce(sum(amount) filter (where not is_source), 0),
		       coalesce(sum(amount) filter (where is_source), 0)
		  from moves
		 where ledger = $1
		   and (address, asset) in (select * from unnest($2::text[], $3::text[]))
		   and effective_date <= $4
		 group by address, asset`,
		s.ledger, addresses, assets, at)
	if err != nil {
		return nil, fmt.Errorf("effective volumes: %w", err)
	}
	defer rows.Close()

	out := make(map[key]ledger.Volumes, len(updates))
	for rows.Next() {
		var k key
		var in, o pgtype.Numeric
		if err := rows.Scan(&k.account, &k.asset, &in, &o); err != nil {
			return nil, err
		}
		input, err := bigInt(in)
		if err != nil {
			return nil, err
		}
		output, err := bigInt(o)
		if err != nil {
			return nil, err
		}
		out[k] = ledger.Volumes{Input: input, Output: output}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for _, u := range updates {
		if _, ok := out[key{u.Account, u.Asset}]; !ok {
			out[key{u.Account, u.Asset}] = ledger.NewVolumes()
		}
	}
	return out, nil
}

// a backdated transaction lands before moves that already exist, so their
// effective view is now short by its delta. strictly greater than, because a
// move sharing this effective date was inserted first and therefore sits
// before these ones.
//
// when nothing is backdated this matches no rows and costs one statement per
// touched pair.
func (s *Store) shiftLaterEffectiveVolumes(ctx context.Context, tx pgx.Tx, updates []ledger.VolumeUpdate, at time.Time) error {
	batch := &pgx.Batch{}
	for _, u := range updates {
		batch.Queue(`
			update moves
			   set pcev_input = pcev_input + $5, pcev_output = pcev_output + $6
			 where ledger = $1 and address = $2 and asset = $3
			   and effective_date > $4`,
			s.ledger, u.Account, u.Asset, at, numeric(u.Input), numeric(u.Output))
	}

	results := tx.SendBatch(ctx, batch)
	for range updates {
		if _, err := results.Exec(); err != nil {
			_ = results.Close()
			return fmt.Errorf("shift effective volumes: %w", err)
		}
	}
	return results.Close()
}

func copyVolumes(in map[key]ledger.Volumes) map[key]ledger.Volumes {
	out := make(map[key]ledger.Volumes, len(in))
	for k, v := range in {
		out[k] = ledger.Volumes{
			Input:  new(big.Int).Set(v.Input),
			Output: new(big.Int).Set(v.Output),
		}
	}
	return out
}

func apply(m map[key]ledger.Volumes, k key, amount *big.Int, isSource bool) {
	v := m[k]
	if v.Input == nil {
		v = ledger.NewVolumes()
	}
	if isSource {
		v.Output = new(big.Int).Add(v.Output, amount)
	} else {
		v.Input = new(big.Int).Add(v.Input, amount)
	}
	m[k] = v
}

func pairs(updates []ledger.VolumeUpdate) (addresses, assets []string) {
	addresses = make([]string, len(updates))
	assets = make([]string, len(updates))
	for i, u := range updates {
		addresses[i] = u.Account
		assets[i] = u.Asset
	}
	return addresses, assets
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
	// jitter is proportional rather than a flat span, so it spreads a
	// thundering herd at every scale and the windows stay ordered: attempt n
	// waits somewhere in [base, 2*base), and 2*base for one attempt is exactly
	// the base of the next, so a later retry never waits less than an earlier
	// one. flat jitter overlaps the early windows and does nothing for the
	// late ones.
	base := time.Duration(1<<attempt) * time.Millisecond
	d := base + time.Duration(rand.Int64N(int64(base)))
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
