package storage

import (
	"context"
	"errors"
	"testing"

	"github.com/pixperk/giro/ledger"
)

// the fourth invariant: replaying the log must reproduce the projection.
//
// this is the one that makes "the log is the source of truth" a fact rather
// than a statement of intent, and the only check that would catch a commit path
// writing one thing and logging another. every other assertion reads the
// projection, so a consistent lie passes all of them.
func assertProjectionMatchesTheLog(t *testing.T, ctx context.Context, s *Store) {
	t.Helper()
	if _, err := s.VerifyProjection(ctx); err != nil {
		t.Errorf("the projection disagrees with the log: %v", err)
	}
}

func TestProjectionMatchesTheLog(t *testing.T) {
	ctx, s, _ := testStore(t)
	fund(t, ctx, s, "users:alice", 10000)
	fund(t, ctx, s, "users:bob", 5000)
	mustCommit(t, ctx, s, "users:alice", "users:bob", 2500)

	checked, err := s.VerifyProjection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if checked != 3 {
		t.Errorf("checked %d account/asset pairs, want 3", checked)
	}
}

// a reversal moves value, so the replay has to account for it.
func TestProjectionAfterRevertAndMetadata(t *testing.T) {
	ctx, s, _ := testStore(t)
	fund(t, ctx, s, "users:alice", 10000)
	payment := mustCommit(t, ctx, s, "users:alice", "users:bob", 3000)

	if _, err := s.SetTransactionMetadata(ctx, payment.ID, ledger.Metadata{"a": "1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RevertTransaction(ctx, payment.ID, RevertOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetAccountMetadata(ctx, "users:carol", ledger.Metadata{"b": "2"}); err != nil {
		t.Fatal(err)
	}

	assertProjectionMatchesTheLog(t, ctx, s)
}

// the check has to notice a projection that quietly disagrees with its own log.
func TestProjectionCatchesTamperedVolumes(t *testing.T) {
	ctx, s, pool := testStore(t)
	withoutGuards(t, ctx, pool, "accounts_volumes")
	fund(t, ctx, s, "users:alice", 10000)

	if _, err := pool.Exec(ctx,
		`update accounts_volumes set input = input + 1 where address = 'users:alice'`); err != nil {
		t.Fatal(err)
	}

	_, err := s.VerifyProjection(ctx)
	var mismatch *ProjectionMismatch
	if !errors.As(err, &mismatch) {
		t.Fatalf("err = %v, want ProjectionMismatch", err)
	}
	if mismatch.Account != "users:alice" {
		t.Errorf("named %s, want users:alice", mismatch.Account)
	}
}

func TestProjectionCatchesAnUnloggedTransaction(t *testing.T) {
	ctx, s, pool := testStore(t)
	fund(t, ctx, s, "users:alice", 10000)

	// a transaction row with no log entry is invisible to a replica or a
	// rebuild, so it must not pass
	if _, err := pool.Exec(ctx, `
		insert into transactions (ledger, id, timestamp, postings, sources, destinations)
		values ('main', 99, now(), '[]'::jsonb, '{}', '{}')`); err != nil {
		t.Fatal(err)
	}

	if _, err := s.VerifyProjection(ctx); err == nil {
		t.Fatal("an unlogged transaction passed verification")
	}
}

func TestProjectionCatchesADeletedVolumeRow(t *testing.T) {
	ctx, s, pool := testStore(t)
	withoutGuards(t, ctx, pool, "accounts_volumes")
	fund(t, ctx, s, "users:alice", 10000)

	if _, err := pool.Exec(ctx,
		`delete from accounts_volumes where address = 'users:alice'`); err != nil {
		t.Fatal(err)
	}

	_, err := s.VerifyProjection(ctx)
	var mismatch *ProjectionMismatch
	if !errors.As(err, &mismatch) || !mismatch.Missing {
		t.Fatalf("err = %v, want a Missing ProjectionMismatch", err)
	}
}

// whatever sequence of operations, the projection must stay derivable from the
// log.
func TestProjectionSurvivesRandomActivity(t *testing.T) {
	ctx, s, _ := testStore(t)

	accounts := []ledger.Address{"alice", "bob", "carol", "fees:platform"}
	rng := seededRand(t)
	for _, a := range accounts {
		fund(t, ctx, s, a, 1_000_000)
	}

	var committed []int64
	for range 40 {
		switch rng.IntN(4) {
		case 0:
			if len(committed) > 0 {
				id := committed[rng.IntN(len(committed))]
				_, _ = s.RevertTransaction(ctx, id, RevertOptions{})
			}
		case 1:
			if len(committed) > 0 {
				id := committed[rng.IntN(len(committed))]
				_, _ = s.SetTransactionMetadata(ctx, id, ledger.Metadata{"k": "v"})
			}
		default:
			from := accounts[rng.IntN(len(accounts))]
			to := accounts[rng.IntN(len(accounts))]
			if from == to {
				to = "world"
			}
			tx, err := s.CommitTransaction(ctx, ledger.Postings{
				{Source: from, Destination: to, Asset: "USD/2", Amount: n(rng.Int64N(1000) + 1)},
			}, CommitOptions{})
			if err == nil {
				committed = append(committed, tx.ID)
			}
		}
	}

	checked, err := s.VerifyProjection(ctx)
	if err != nil {
		t.Fatalf("after random activity: %v", err)
	}
	t.Logf("verified %d account/asset pairs against the log", checked)

	// and the other three invariants still hold on the same data
	if _, err := s.VerifyLog(ctx); err != nil {
		t.Errorf("chain: %v", err)
	}
	if _, err := s.VerifyEffectiveVolumes(ctx); err != nil {
		t.Errorf("effective volumes: %v", err)
	}
}

func TestProjectionIsScopedToItsLedger(t *testing.T) {
	ctx, mine, theirs, _ := twoLedgers(t)

	if _, err := mine.VerifyProjection(ctx); err != nil {
		t.Errorf("mine: %v", err)
	}
	if _, err := theirs.VerifyProjection(ctx); err != nil {
		t.Errorf("theirs: %v", err)
	}
}
