package storage

import (
	"sync/atomic"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is scoped to a single ledger.
//
// the ledger name is seeded at construction and is never a parameter, so there
// is no call site that can forget it. the tenant boundary is structural rather
// than something every query has to remember.
type Store struct {
	pool   *pgxpool.Pool
	ledger string

	// how many times a commit has been restarted after a deadlock or
	// serialization failure. sorted lock ordering should keep this at zero, so
	// a rising count means the ordering has been broken somewhere.
	retries atomic.Int64

	// test seam, called after volumes are locked and before they are applied.
	//
	// it exists because the write path serialises on other row locks anyway,
	// which narrows the gap between the balance check and the write so far
	// that a missing FOR UPDATE is usually invisible. widening that gap is the
	// only way a test can tell the lock is doing the work rather than luck.
	afterLock func()

	// test seam, called just before COMMIT with the attempt number, zero based.
	// returning an error aborts that attempt.
	//
	// the retry loop is the code that runs when things go wrong, and it is
	// close to impossible to provoke on demand: the sorted single statement
	// lock means a commit is never the deadlock victim, because whichever
	// transaction closes the cycle is the one postgres kills, and ours always
	// acquires first and waits. injecting the failure is the only way to
	// exercise the path that recovers from it.
	beforeCommit func(attempt int) error
}

func New(pool *pgxpool.Pool, ledgerName string) *Store {
	return &Store{pool: pool, ledger: ledgerName}
}

// Retries reports how many commits have been restarted due to contention.
func (s *Store) Retries() int64 { return s.retries.Load() }
