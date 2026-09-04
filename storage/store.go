// Package storage is the engine: the commit path, the hash chained log, and
// the queries over both. It speaks postgres and knows nothing about http.
//
// A Store is scoped to one ledger. Commits to a single ledger serialise, so
// ids stay gapless and the log chain has a single writer; commits to different
// ledgers do not contend, which is where throughput comes from.
package storage

import (
	"sync"
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

	// assets already seen registered on this ledger, and whether the ledger
	// itself has been seen to exist. positive answers only: both facts are
	// permanent once true, so an entry here can never go stale. see
	// checkAssets for why that is safe and what it buys.
	registered sync.Map
	ledgerSeen atomic.Bool

	// where telemetry goes, or nil. see observer.go: nothing is computed when
	// it is nil, so an unobserved store pays for one comparison per event.
	obs Observer

	// where spans go, or nil. usually the same object as obs; see Observe.
	tracer Tracer

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
