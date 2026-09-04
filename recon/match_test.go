package recon_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pixperk/giro/ledger"
	"github.com/pixperk/giro/recon"
	"github.com/pixperk/giro/storage"
)

// The bank account money leaves through, and the chain it arrives over.
const (
	bank  = ledger.Address("external:bank:infinitus:USD")
	chain = ledger.Address("external:chain:tron:USDT")
)

func setup(t *testing.T) (context.Context, *storage.Store, *pgxpool.Pool) {
	t.Helper()
	ctx, s, pool := testStore(t)
	for _, a := range []struct {
		address ledger.Address
		asset   ledger.Asset
	}{{bank, "USD/2"}, {chain, "USDT/6"}} {
		if err := s.SetAllowNegative(ctx, a.address, a.asset, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := recon.Register(ctx, pool, "main", &kraken{}); err != nil {
		t.Fatal(err)
	}
	return ctx, s, pool
}

// pay records a wire out of the bank, under the reference the bank will use.
//
// The shared key goes in metadata and not in Reference, because Reference is
// unique per ledger and a consolidated wire is several transactions arriving
// under one string. That constraint is what makes the two fields different
// things rather than two names for one.
func pay(t *testing.T, ctx context.Context, s *storage.Store, client string, cents int64, ref string) {
	t.Helper()
	if _, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "world", Destination: ledger.Address(client), Asset: "USD/2", Amount: big.NewInt(cents)},
	}, storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: ledger.Address(client), Destination: bank, Asset: "USD/2", Amount: big.NewInt(cents)},
	}, storage.CommitOptions{
		Metadata: ledger.Metadata{recon.ExternalRefKey: ref},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestOneLineMatchesOneMovement(t *testing.T) {
	ctx, s, pool := setup(t)
	pay(t, ctx, s, "client:acme", 99_725_00, "WIRE-1")

	if _, err := recon.Ingest(ctx, pool, "main", "kraken",
		[]recon.Record{line("L1", "WIRE-1", 99_725_00, recon.Out)}); err != nil {
		t.Fatal(err)
	}

	sum, err := recon.Match(ctx, pool, "main", recon.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Matched != 1 || sum.Variance != 0 || len(sum.Unmatched) != 0 {
		t.Fatalf("summary = %+v, want one clean match", sum)
	}

	// running again changes nothing, which is what makes it schedulable
	again, err := recon.Match(ctx, pool, "main", recon.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if again.Matched != 0 {
		t.Errorf("a second run matched %d, want 0", again.Matched)
	}
}

// The case worth having: one bank line paying several of our transactions.
// It matches only because the set sums to the line exactly.
func TestOneLinePayingSeveralMovements(t *testing.T) {
	ctx, s, pool := setup(t)
	pay(t, ctx, s, "client:acme", 40_000_00, "BATCH-1")
	pay(t, ctx, s, "client:beta", 35_000_00, "BATCH-1")
	pay(t, ctx, s, "client:gamma", 25_000_00, "BATCH-1")

	if _, err := recon.Ingest(ctx, pool, "main", "kraken",
		[]recon.Record{line("L1", "BATCH-1", 100_000_00, recon.Out)}); err != nil {
		t.Fatal(err)
	}

	sum, err := recon.Match(ctx, pool, "main", recon.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Matched != 1 || len(sum.Unmatched) != 0 {
		t.Fatalf("summary = %+v, want the consolidated wire matched", sum)
	}

	// every movement in the set is recorded, and each carries the set's size
	var rows, setSize int
	if err := pool.QueryRow(ctx,
		"select count(*), max(set_size) from recon_matches where ledger='main'").Scan(&rows, &setSize); err != nil {
		t.Fatal(err)
	}
	if rows != 3 || setSize != 3 {
		t.Errorf("%d matches with set size %d, want 3 and 3", rows, setSize)
	}
}

// A set that does not add up is indistinguishable from two unrelated things
// sharing a reference, so it stays unmatched rather than being recorded as a
// partial match and dropping out of the queue.
func TestASetThatDoesNotAddUpStaysUnmatched(t *testing.T) {
	ctx, s, pool := setup(t)
	pay(t, ctx, s, "client:acme", 40_000_00, "SHARED")
	pay(t, ctx, s, "client:beta", 35_000_00, "SHARED")

	if _, err := recon.Ingest(ctx, pool, "main", "kraken",
		[]recon.Record{line("L1", "SHARED", 90_000_00, recon.Out)}); err != nil {
		t.Fatal(err)
	}

	sum, err := recon.Match(ctx, pool, "main", recon.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Matched != 0 {
		t.Errorf("matched %d, want none: 40,000 and 35,000 do not make 90,000", sum.Matched)
	}
	if sum.Unmatched[recon.Ambiguous] != 1 {
		t.Errorf("unmatched = %v, want one ambiguous", sum.Unmatched)
	}
}

// The direction trap. Same amount, same reference, opposite direction: without
// the check this is a clean-looking match that is completely wrong.
func TestAnOutboundLineDoesNotMatchAnInboundMovement(t *testing.T) {
	ctx, s, pool := setup(t)

	// money arriving over the chain, under a reference
	if _, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: chain, Destination: "client:acme", Asset: "USDT/6", Amount: big.NewInt(100)},
	}, storage.CommitOptions{Reference: "AMBIG", Metadata: ledger.Metadata{recon.ExternalRefKey: "AMBIG"}}); err != nil {
		t.Fatal(err)
	}

	// a line claiming the same reference and size went the other way
	if _, err := recon.Ingest(ctx, pool, "main", "kraken", []recon.Record{{
		ID: "L1", Reference: "AMBIG", Asset: "USDT/6",
		Amount: big.NewInt(100), Direction: recon.Out, OccurredAt: time.Now(),
	}}); err != nil {
		t.Fatal(err)
	}

	sum, err := recon.Match(ctx, pool, "main", recon.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Matched != 0 {
		t.Errorf("an outbound line matched an inbound movement")
	}
	if sum.Unmatched[recon.NotFound] != 1 {
		t.Errorf("unmatched = %v, want it reported as not found", sum.Unmatched)
	}

	// and the same line the right way round does match
	if _, err := recon.Ingest(ctx, pool, "main", "kraken", []recon.Record{{
		ID: "L2", Reference: "AMBIG", Asset: "USDT/6",
		Amount: big.NewInt(100), Direction: recon.In, OccurredAt: time.Now(),
	}}); err != nil {
		t.Fatal(err)
	}
	sum, err = recon.Match(ctx, pool, "main", recon.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Matched != 1 {
		t.Errorf("the inbound line did not match: %+v", sum)
	}
}

// A line whose amount disagrees is still paired, and the difference recorded.
// Somebody thinks a different amount moved, and that wants a person rather
// than a silent adjustment.
func TestADisagreeingAmountIsAVarianceNotAMatch(t *testing.T) {
	ctx, s, pool := setup(t)
	pay(t, ctx, s, "client:acme", 99_725_00, "WIRE-1")

	if _, err := recon.Ingest(ctx, pool, "main", "kraken",
		[]recon.Record{line("L1", "WIRE-1", 99_720_00, recon.Out)}); err != nil {
		t.Fatal(err)
	}

	sum, err := recon.Match(ctx, pool, "main", recon.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Variance != 1 || sum.Matched != 0 {
		t.Fatalf("summary = %+v, want one variance", sum)
	}

	var variance string
	if err := pool.QueryRow(ctx,
		"select variance::text from recon_matches where ledger='main'").Scan(&variance); err != nil {
		t.Fatal(err)
	}
	if variance != "-500" {
		t.Errorf("variance = %s, want -500: they say five dollars less moved", variance)
	}
}

// Four different problems, four different people fixing them.
func TestBreaksAreClassified(t *testing.T) {
	ctx, s, pool := setup(t)
	pay(t, ctx, s, "client:acme", 10_000, "KNOWN")

	if _, err := recon.Ingest(ctx, pool, "main", "kraken", []recon.Record{
		{ID: "L1", Asset: "USD/2", Amount: big.NewInt(1), Direction: recon.Out}, // no reference at all
		line("L2", "NOWHERE", 5_000, recon.Out),                                 // names nothing here
	}); err != nil {
		t.Fatal(err)
	}

	sum, err := recon.Match(ctx, pool, "main", recon.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Unmatched[recon.NoReference] != 1 {
		t.Errorf("unmatched = %v, want a line with no reference", sum.Unmatched)
	}
	if sum.Unmatched[recon.NotFound] != 1 {
		t.Errorf("unmatched = %v, want a reference naming nothing", sum.Unmatched)
	}
}

// A deployment that names its edges differently only has to say so.
func TestTheBoundaryPrefixIsConfigurable(t *testing.T) {
	ctx, s, pool := testStore(t)
	if err := s.SetAllowNegative(ctx, "edge:bank", "USD/2", true); err != nil {
		t.Fatal(err)
	}
	if err := recon.Register(ctx, pool, "main", &kraken{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "edge:bank", Destination: "client:acme", Asset: "USD/2", Amount: big.NewInt(500)},
	}, storage.CommitOptions{Reference: "E-1", Metadata: ledger.Metadata{recon.ExternalRefKey: "E-1"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := recon.Ingest(ctx, pool, "main", "kraken",
		[]recon.Record{line("L1", "E-1", 500, recon.In)}); err != nil {
		t.Fatal(err)
	}

	// giro's default does not recognise this ledger's edges
	sum, err := recon.Match(ctx, pool, "main", recon.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Matched != 0 {
		t.Error("the default prefix matched an edge it should not know about")
	}

	sum, err = recon.Match(ctx, pool, "main", recon.Config{BoundaryPrefix: "edge:"})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Matched != 1 {
		t.Errorf("summary = %+v, want the configured prefix to match", sum)
	}
}
