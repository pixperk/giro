package storage

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Contention handling.
//
// only a deadlock or a serialization failure is worth retrying: a business
// rejection retried ten times just fails ten times more slowly.

// only contention is retryable. a business rejection retried ten times just
// fails ten times more slowly.
// the postgres error codes worth distinguishing.
const (
	deadlockDetected     = "40P01"
	serializationFailure = "40001"
	uniqueViolation      = "23505"
)

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
