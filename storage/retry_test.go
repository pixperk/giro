package storage

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pixperk/giro/ledger"
)

func deadlock() error {
	return &pgconn.PgError{Code: deadlockDetected, Message: "deadlock detected"}
}

func TestRetryableClassification(t *testing.T) {
	tests := []struct {
		why  string
		err  error
		want bool
	}{
		{"deadlock", &pgconn.PgError{Code: deadlockDetected}, true},
		{"serialization failure", &pgconn.PgError{Code: serializationFailure}, true},
		{"wrapped deadlock", fmt.Errorf("commit: %w", &pgconn.PgError{Code: deadlockDetected}), true},

		// retrying a rejection just fails more slowly
		{"unique violation", &pgconn.PgError{Code: uniqueViolation}, false},
		{"insufficient funds", &InsufficientFundsError{Account: "a", Available: big.NewInt(0), Requested: big.NewInt(1)}, false},
		{"ledger not found", ErrLedgerNotFound, false},
		{"context cancelled", context.Canceled, false},
		{"plain error", errors.New("boom"), false},
		{"nil", nil, false},
	}

	for _, tc := range tests {
		t.Run(tc.why, func(t *testing.T) {
			if got := retryable(tc.err); got != tc.want {
				t.Errorf("retryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestBackoffGrowsAndStaysBounded(t *testing.T) {
	ctx := context.Background()

	// every attempt must wait at least its base. measured time can overshoot
	// because the scheduler is not obliged to wake us promptly, so only the
	// floor is safe to assert per attempt.
	for attempt := range 7 {
		floor := time.Duration(1<<attempt) * time.Millisecond
		start := time.Now()
		if err := backoff(ctx, attempt); err != nil {
			t.Fatal(err)
		}
		if took := time.Since(start); took < floor {
			t.Errorf("attempt %d waited %v, less than its %v base", attempt, took, floor)
		}
	}

	// growth is checked across a wide gap, where the 64x difference in base
	// swamps any scheduling noise. adjacent attempts are too close to compare
	// reliably.
	early := timeBackoff(t, 0)
	late := timeBackoff(t, 6)
	if late <= early {
		t.Errorf("attempt 6 waited %v, not longer than attempt 0 at %v", late, early)
	}

	// the whole loop must stay well under any request timeout
	worst := time.Duration(0)
	for attempt := range maxAttempts {
		worst += 2 * time.Duration(1<<attempt) * time.Millisecond
	}
	if worst > 5*time.Second {
		t.Errorf("worst case backoff totals %v, too long to hold a connection", worst)
	}
}

func timeBackoff(t *testing.T, attempt int) time.Duration {
	t.Helper()
	start := time.Now()
	if err := backoff(context.Background(), attempt); err != nil {
		t.Fatal(err)
	}
	return time.Since(start)
}

// a caller who gives up must not be left waiting out the backoff.
func TestBackoffStopsOnContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	err := backoff(ctx, 9) // would otherwise sleep about half a second
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if took := time.Since(start); took > 50*time.Millisecond {
		t.Errorf("returned after %v, should be immediate", took)
	}
}

func TestCommitRetriesAndSucceeds(t *testing.T) {
	ctx, s, pool := testStore(t)

	var attempts atomic.Int64
	s.beforeCommit = func(attempt int) error {
		attempts.Add(1)
		if attempt < 3 {
			return deadlock()
		}
		return nil
	}

	tx, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "world", Destination: "users:alice", Asset: "USD/2", Amount: n(10000)},
	}, CommitOptions{})
	if err != nil {
		t.Fatalf("should have succeeded on the fourth attempt: %v", err)
	}

	if got := attempts.Load(); got != 4 {
		t.Errorf("%d attempts, want 4", got)
	}
	if got := s.Retries(); got != 3 {
		t.Errorf("%d retries recorded, want 3", got)
	}

	// the three abandoned attempts must leave nothing behind
	if tx.ID != 1 {
		t.Errorf("transaction id = %d, want 1: rolled back attempts must not consume ids", tx.ID)
	}
	if got := balance(t, ctx, pool, "users:alice", "USD/2"); got.Cmp(n(10000)) != 0 {
		t.Errorf("alice = %s, want 10000: the money must move once, not four times", got)
	}
	if got := logCount(t, ctx, pool); got != 1 {
		t.Errorf("%d log entries, want 1", got)
	}
	if _, err := s.VerifyLog(ctx); err != nil {
		t.Errorf("chain broken after retries: %v", err)
	}
	assertConserved(t, ctx, pool)
}

func TestCommitGivesUpAfterMaxAttempts(t *testing.T) {
	ctx, s, pool := testStore(t)

	var attempts atomic.Int64
	s.beforeCommit = func(int) error {
		attempts.Add(1)
		return deadlock()
	}

	_, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "world", Destination: "users:alice", Asset: "USD/2", Amount: n(10000)},
	}, CommitOptions{})

	if err == nil {
		t.Fatal("expected an error after exhausting attempts")
	}
	if !strings.Contains(err.Error(), "giving up") {
		t.Errorf("err = %v, want a giving up error", err)
	}
	if got := attempts.Load(); got != maxAttempts {
		t.Errorf("%d attempts, want %d", got, maxAttempts)
	}

	// nothing may survive a run that never committed
	if got := logCount(t, ctx, pool); got != 0 {
		t.Errorf("%d log entries, want 0", got)
	}
	var accounts int
	pool.QueryRow(ctx, "select count(*) from accounts_volumes where ledger='main'").Scan(&accounts)
	if accounts != 0 {
		t.Errorf("%d volume rows survived, want 0: the zero rows are created under the lock and must roll back", accounts)
	}
}

// a business rejection must fail once, not ten times more slowly.
func TestNonRetryableFailsImmediately(t *testing.T) {
	ctx, s, _ := testStore(t)

	var attempts atomic.Int64
	s.beforeCommit = func(int) error {
		attempts.Add(1)
		return &pgconn.PgError{Code: uniqueViolation, Message: "duplicate key"}
	}

	_, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "world", Destination: "users:alice", Asset: "USD/2", Amount: n(10000)},
	}, CommitOptions{})

	if err == nil {
		t.Fatal("expected an error")
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("%d attempts, want 1", got)
	}
	if got := s.Retries(); got != 0 {
		t.Errorf("%d retries, want 0", got)
	}
}

// cancelling mid retry must return promptly rather than working through the
// remaining attempts.
func TestRetryStopsWhenContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	_, s, _ := testStore(t)

	var attempts atomic.Int64
	s.beforeCommit = func(int) error {
		if attempts.Add(1) == 2 {
			cancel()
		}
		return deadlock()
	}

	start := time.Now()
	_, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "world", Destination: "users:alice", Asset: "USD/2", Amount: n(10000)},
	}, CommitOptions{})

	if err == nil {
		t.Fatal("expected an error")
	}
	if got := attempts.Load(); got >= maxAttempts {
		t.Errorf("%d attempts, should have stopped early on cancellation", got)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("took %v, should have returned promptly", took)
	}
}
