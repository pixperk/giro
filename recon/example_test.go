package recon_test

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pixperk/giro/ledger"
	"github.com/pixperk/giro/recon"
	"github.com/pixperk/giro/storage"
)

// bank is the adapter you write per counterparty: it calls somebody's API and
// maps the answer onto Record. It touches no database and knows nothing about
// accounts.
type bankStatement struct{}

func (bankStatement) ID() string   { return "infinitus" }
func (bankStatement) Name() string { return "Infinitus Pay" }

func (bankStatement) Fetch(ctx context.Context, since time.Time) ([]recon.Record, error) {
	// in a real adapter: call the API, map its rows. returning a line you have
	// returned before is fine and expected -- staging is idempotent, so
	// overlapping windows are the safe way to page a statement.
	return []recon.Record{{
		ID:         "STMT-88213",
		Reference:  "WIRE-2026-0142",
		Asset:      "USD/2",
		Amount:     big.NewInt(99_725_00),
		Direction:  recon.Out,
		OccurredAt: time.Now(),
	}}, nil
}

// The whole flow: record a payment, ingest what the bank says, and see whether
// the two agree.
//
// This compiles as part of the test suite, so the quickstart in README.md
// cannot drift away from the API it documents.
func Example() {
	ctx := context.Background()
	var pool *pgxpool.Pool // your pgxpool, migrated

	s := storage.New(pool, "main")
	source := bankStatement{}

	// once, at startup. both are idempotent.
	if err := s.RegisterAsset(ctx, "USD/2"); err != nil {
		log.Fatal(err)
	}
	if err := recon.Register(ctx, pool, "main", source); err != nil {
		log.Fatal(err)
	}

	// the boundary account stands for the bank, and is permitted a negative
	// balance because it is the outside world's side of the book
	const atBank = ledger.Address("external:bank:infinitus:USD")
	if err := s.SetAllowNegative(ctx, atBank, "USD/2", true); err != nil {
		log.Fatal(err)
	}

	// a payment, stamped with the reference the bank will use for it
	if _, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "client:acme", Destination: atBank, Asset: "USD/2", Amount: big.NewInt(99_725_00)},
	}, storage.CommitOptions{
		Metadata: ledger.Metadata{recon.ExternalRefKey: "WIRE-2026-0142"},
	}); err != nil {
		log.Fatal(err)
	}

	// on a schedule: fetch what the bank says, then pair it with what we say
	if _, err := recon.Pull(ctx, pool, "main", source, time.Now().Add(-time.Hour)); err != nil {
		log.Fatal(err)
	}
	sum, err := recon.Match(ctx, pool, "main", recon.Config{})
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println(sum.Matched, sum.Variance, sum.Unmatched)
}
