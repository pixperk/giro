package storage

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pixperk/giro/ledger"
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
	// started before the validation returns rather than after them: those are
	// refusals too, and a refusal that happens to be cheap to detect is still
	// one a caller was told no. leaving them out would mean the refusal rate
	// silently excluded every malformed request.
	started := time.Now()

	// the outermost span covers retries and the backoff between them, so its
	// duration is what the caller waited rather than what the last attempt took
	ctx, endCommit := s.start(ctx, SpanCommit)
	var outcome error
	defer func() { endCommit(outcome) }()

	if len(p) == 0 {
		outcome = s.refuse(ctx, started, ErrNoPostings)
		return nil, outcome
	}
	if i, err := p.Validate(); err != nil {
		outcome = s.refuse(ctx, started, &PostingError{Index: i, Err: err})
		return nil, outcome
	}
	// before the retry loop: an unregistered asset is not a contention
	// problem, and retrying it ten times with backoff would only make the
	// answer slower.
	if err := s.checkAssets(ctx, p); err != nil {
		outcome = s.refuse(ctx, started, err)
		return nil, outcome
	}

	ikHash, err := idempotencyHash(p, opts)
	if err != nil {
		outcome = err
		return nil, outcome
	}

	for attempt := range maxAttempts {
		tx, err := s.commitOnce(ctx, p, opts, ikHash, attempt)
		if err == nil {
			s.observeCommitted(ctx, tx, p, attempt+1, time.Since(started))
			return tx, nil
		}
		if !retryable(err) {
			outcome = s.refuse(ctx, started, err)
			return nil, outcome
		}

		s.retries.Add(1)
		s.observeContention(ctx, Contention{
			Attempt: attempt, Restarted: true, Waited: time.Since(started),
		})
		if err := backoff(ctx, attempt); err != nil {
			outcome = err
			return nil, outcome
		}
	}
	s.observeRefusal(ctx, Refusal{Reason: CauseContentionGiveUp, Took: time.Since(started)})
	outcome = fmt.Errorf("giving up after %d attempts, contention on ledger %q", maxAttempts, s.ledger)
	return nil, outcome
}

// refuse reports a declined transaction and returns the error unchanged, so a
// call site stays one line and cannot report one error while returning
// another.
//
// A refusal and a broken connection are different things and must not share a
// series: CauseOf decides which this was, and anything it does not recognise
// is passed through unreported rather than being filed as the ledger saying
// no.
func (s *Store) refuse(ctx context.Context, started time.Time, err error) error {
	if cause, refused := CauseOf(err); refused {
		s.observeRefusal(ctx, Refusal{
			Reason:  cause,
			Asset:   assetOf(err),
			Account: accountOf(err),
			Took:    time.Since(started),
		})
	}
	return err
}

// observeCommitted assembles the event only when somebody is listening: the
// deduplication below allocates, and an unobserved commit must not pay for it.
func (s *Store) observeCommitted(ctx context.Context, tx *ledger.Transaction, p ledger.Postings, attempts int, took time.Duration) {
	if !s.observing() {
		return
	}
	var (
		assets    []ledger.Asset
		addresses []ledger.Address
		seenA     = map[ledger.Asset]bool{}
		seenAddr  = map[ledger.Address]bool{}
	)
	for _, posting := range p {
		if !seenA[posting.Asset] {
			seenA[posting.Asset] = true
			assets = append(assets, posting.Asset)
		}
		for _, a := range [...]ledger.Address{posting.Source, posting.Destination} {
			if !seenAddr[a] {
				seenAddr[a] = true
				addresses = append(addresses, a)
			}
		}
	}
	s.observeCommit(ctx, Commit{
		Assets:    assets,
		Postings:  len(p),
		Accounts:  len(addresses),
		Attempts:  attempts,
		Took:      took,
		Addresses: addresses,
	})
}

// assetOf and accountOf pull the two fields worth reporting out of a refusal.
// They are here rather than on the error types because they exist for
// telemetry, and an error's job is to explain itself to a person.
func assetOf(err error) ledger.Asset {
	var (
		funds  *InsufficientFundsError
		credit *UnexpectedCreditError
		asset  *UnknownAssetError
	)
	switch {
	case errors.As(err, &funds):
		return funds.Asset
	case errors.As(err, &credit):
		return credit.Asset
	case errors.As(err, &asset):
		return asset.Asset
	}
	return ""
}

func accountOf(err error) ledger.Address {
	var (
		funds  *InsufficientFundsError
		credit *UnexpectedCreditError
		closed *AccountClosedError
	)
	switch {
	case errors.As(err, &funds):
		return funds.Account
	case errors.As(err, &credit):
		return credit.Account
	case errors.As(err, &closed):
		return closed.Account
	}
	return ""
}

func (s *Store) commitOnce(ctx context.Context, p ledger.Postings, opts CommitOptions, ikHash string, attempt int) (_ *ledger.Transaction, err error) {
	ctx, end := s.start(ctx, SpanAttempt)
	defer func() { end(err) }()

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

	// the rows are locked, so what each source holds is now both known and
	// fixed. any ceiling becomes the figure it resolves to, and the volume
	// deltas are recomputed from it: the keys are unchanged, so the locks
	// already taken are still the right ones in the right order.
	if p.HasUpTo() {
		if p, err = resolveUpTo(p, before); err != nil {
			return nil, alloc, err
		}
		updates = p.VolumeUpdates()
	}

	if err := checkClosed(before, updates); err != nil {
		return nil, alloc, err
	}
	if err := checkBalances(before, updates); err != nil {
		return nil, alloc, err
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

// checkBalances rejects the transaction if any account would end outside a
// bound it is not permitted to cross.
//
// the permission is read from the row the caller already locked, so it cannot
// have changed since. world carries it from creation; anything else carries it
// because someone set it deliberately, which is how a cost account is told
// apart from a client account about to be drained.
//
// the check is on the final state, so an account may pass through zero within
// a transaction. it uses the current balance and never an effective date
// balance: the money either exists now or it does not.
func checkBalances(before map[key]locked, updates []ledger.VolumeUpdate) error {
	for _, u := range updates {
		v := before[key{u.Account, u.Asset}]
		input := new(big.Int).Add(v.Input, u.Input)
		output := new(big.Int).Add(v.Output, u.Output)

		if !v.allowNegative && input.Cmp(output) < 0 {
			return &InsufficientFundsError{
				Account:   u.Account,
				Asset:     u.Asset,
				Available: v.Balance(),
				Requested: u.Output,
			}
		}

		// the mirror. a cost account only ever leans one way, so a positive
		// balance on one means a loss was recorded as a gain: the books still
		// balance and the profit figure is wrong by twice the amount.
		if !v.allowPositive && input.Cmp(output) > 0 {
			return &UnexpectedCreditError{
				Account: u.Account,
				Asset:   u.Asset,
				Balance: new(big.Int).Sub(input, output),
			}
		}
	}
	return nil
}

// UnexpectedCreditError is returned when a posting would leave an account
// above zero that is not permitted to be.
type UnexpectedCreditError struct {
	Account ledger.Address
	Asset   ledger.Asset
	Balance *big.Int
}

func (e *UnexpectedCreditError) Error() string {
	return fmt.Sprintf("%s would hold %s %s, and is not permitted a positive balance: "+
		"a cost recorded as a gain leaves the book balanced and the profit wrong",
		e.Account, e.Balance, e.Asset)
}

// the final state of every touched account, frozen at commit.
func postCommitVolumes(before map[key]locked, updates []ledger.VolumeUpdate) ledger.PostCommitVolumes {
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

// resolveUpTo replaces the ceiling on every UpTo posting with what the source
// actually holds.
//
// It runs after lockVolumes and before checkBalances, which is the only window
// where the answer is both known and pinned: the rows are locked, so no
// concurrent commit can change a balance between deciding the amount and
// moving it. Deciding it any earlier is the read-then-write gap this exists to
// close.
//
// Lock order is unaffected. Locks are taken per (account, asset) and an
// amount does not name a row, so rewriting one cannot reorder anything. The
// volume updates do carry amounts, so they are recomputed by the caller.
//
// Postings are applied in order and a source drained by an earlier one leaves
// nothing for a later one, which is the same rule as any other posting: money
// can flow through an account within a transaction, and order is what decides
// what is there when each posting runs.
func resolveUpTo(p ledger.Postings, before map[key]locked) (ledger.Postings, error) {
	// running balances, so two postings drawing on the same account in one
	// transaction see each other rather than both claiming the whole balance
	running := make(map[key]*big.Int, len(before))
	for k, v := range before {
		running[k] = v.Balance()
	}

	out := make(ledger.Postings, len(p))
	for i, posting := range p {
		out[i] = posting
		src := key{posting.Source, posting.Asset}

		if !posting.UpTo {
			if amount := posting.Amount; amount != nil {
				running[src] = new(big.Int).Sub(orZero(running[src]), amount)
				dst := key{posting.Destination, posting.Asset}
				running[dst] = new(big.Int).Add(orZero(running[dst]), amount)
			}
			continue
		}

		// an account permitted a negative balance holds no determinate amount.
		// world is the whole outside world and a contra account is a running
		// total of a cost, and "everything either of them has" is not a
		// number. refusing is the only honest answer: the alternative is
		// picking one and calling it the balance.
		if v, ok := before[src]; ok && v.allowNegative {
			return nil, &UnboundedSweepError{Account: posting.Source, Asset: posting.Asset}
		}

		available := orZero(running[src])
		if available.Sign() < 0 {
			available = new(big.Int)
		}
		if posting.Amount != nil && posting.Amount.Cmp(available) < 0 {
			available = posting.Amount
		}

		out[i].Amount = new(big.Int).Set(available)
		out[i].UpTo = false // resolved: what is recorded is what moved

		running[src] = new(big.Int).Sub(orZero(running[src]), available)
		dst := key{posting.Destination, posting.Asset}
		running[dst] = new(big.Int).Add(orZero(running[dst]), available)
	}
	return out, nil
}

func orZero(i *big.Int) *big.Int {
	if i == nil {
		return new(big.Int)
	}
	return i
}

// UnboundedSweepError is returned when a posting asks to move everything an
// account holds and that account is permitted a negative balance, so there is
// no such amount.
type UnboundedSweepError struct {
	Account ledger.Address
	Asset   ledger.Asset
}

func (e *UnboundedSweepError) Error() string {
	return fmt.Sprintf("%s is permitted a negative balance in %s, so it holds no determinate amount to move",
		e.Account, e.Asset)
}

// checkClosed rejects a transaction touching an account that has been closed,
// in either direction.
//
// Both directions, because a closed account holds nothing and closing requires
// it to be empty, so the only thing a payment into one could do is give it a
// balance nobody is watching. An operator with a reason reopens it, moves the
// money, and closes it again: three deliberate acts that leave a trail, rather
// than a hole in the guard that quietly makes closure mean less than it says.
func checkClosed(before map[key]locked, updates []ledger.VolumeUpdate) error {
	for _, u := range updates {
		if before[key{u.Account, u.Asset}].closed {
			return &AccountClosedError{Account: u.Account}
		}
	}
	return nil
}

// AccountClosedError is returned when a posting names an account that has been
// closed.
type AccountClosedError struct {
	Account ledger.Address
}

func (e *AccountClosedError) Error() string {
	return fmt.Sprintf("%s is closed and accepts no further movement; reopen it first", e.Account)
}
