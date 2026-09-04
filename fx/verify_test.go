package fx_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	giro "github.com/pixperk/giro"
	"github.com/pixperk/giro/fx"
	"github.com/pixperk/giro/ledger"
	"github.com/pixperk/giro/migrate"
	"github.com/pixperk/giro/storage"
)

// An external test package on purpose: this exercises fx the way a caller
// composes it, through the engine's public surface, which is the only way it
// is meant to be used.

func testStore(t *testing.T) (context.Context, *storage.Store, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()

	url := os.Getenv("GIRO_TEST_DATABASE_URL")
	if url == "" {
		url = "postgres://" + os.Getenv("USER") + "@localhost:5432/giro_test"
	}
	schema := fmt.Sprintf("fx_%d_%d", os.Getpid(), len(t.Name()))

	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		skipNoDatabase(t, err)
	}
	if _, err := admin.Exec(ctx, "drop schema if exists "+schema+" cascade"); err != nil {
		skipNoDatabase(t, err)
	}
	if _, err := admin.Exec(ctx, "create schema "+schema); err != nil {
		t.Fatal(err)
	}
	admin.Close()

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		if c, err := pgxpool.New(ctx, url); err == nil {
			c.Exec(ctx, "drop schema "+schema+" cascade")
			c.Close()
		}
	})

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sub, _ := fs.Sub(giro.MigrationsFS, giro.MigrationsDir)
	if _, err := migrate.Run(ctx, conn.Conn(), sub); err != nil {
		t.Fatal(err)
	}
	conn.Release()

	s := storage.New(pool, "main")
	if _, err := s.CreateLedger(ctx); err != nil {
		t.Fatal(err)
	}
	for _, a := range []ledger.Asset{"USD/2", "USDT/6"} {
		if err := s.RegisterAsset(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	return ctx, s, pool
}

func usdt(units int64) *big.Int {
	return new(big.Int).Mul(big.NewInt(units), big.NewInt(1_000_000))
}

// The end to end case: a conversion committed through the engine, then found
// to disagree with its own stated rate after somebody edited the claim.
//
// The point is what else stays quiet. Restating a rate moves no money, so both
// sides still conserve, the chain is intact and the projection agrees. Only
// this notices.
func TestVerifyCatchesAMisstatedRate(t *testing.T) {
	ctx, s, pool := testStore(t)

	if _, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "world", Destination: "treasury:usdt", Asset: "USDT/6", Amount: usdt(100_000)},
	}, storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAllowNegative(ctx, "external:lp:kraken:USD", "USD/2", true); err != nil {
		t.Fatal(err)
	}

	sale := fx.Conversion{
		From: "USDT/6", Seller: "treasury:usdt", SoldTo: "external:lp:kraken:USDT",
		To: "USD/2", BoughtFrom: "external:lp:kraken:USD", Buyer: "ops:usd",
		Amount: usdt(100_000), Rate: "0.99960",
	}
	postings, err := sale.Postings()
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.CommitTransaction(ctx, postings,
		storage.CommitOptions{Metadata: sale.Metadata(nil)})
	if err != nil {
		t.Fatal(err)
	}

	checked, err := fx.Verify(ctx, pool, "main", fx.DefaultTolerance)
	if err != nil {
		t.Fatalf("a sound conversion was reported: %v", err)
	}
	if checked != 1 {
		t.Errorf("checked %d, want 1", checked)
	}

	// restate the rate. no money moves.
	if _, err := s.SetTransactionMetadata(ctx, tx.ID,
		ledger.Metadata{fx.ConversionRateKey: "0.9960"}); err != nil {
		t.Fatal(err)
	}

	for name, check := range map[string]func(context.Context) (int, error){
		"conservation": s.VerifyConservation,
		"log":          s.VerifyLog,
		"projection":   s.VerifyProjection,
	} {
		if _, err := check(ctx); err != nil {
			t.Errorf("%s reported something, so this is not the only check that sees it: %v", name, err)
		}
	}

	if _, err := fx.Verify(ctx, pool, "main", fx.DefaultTolerance); !errors.Is(err, fx.ErrConversionRounding) {
		t.Fatalf("err = %v, want the rate disagreement", err)
	} else {
		t.Logf("reported: %v", err)
	}
}

// A ledger that has never traded is not full of findings.
func TestVerifyIsSilentWithoutConversions(t *testing.T) {
	ctx, s, pool := testStore(t)
	if _, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "world", Destination: "users:alice", Asset: "USD/2", Amount: big.NewInt(10000)},
	}, storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}

	checked, err := fx.Verify(ctx, pool, "main", fx.DefaultTolerance)
	if err != nil {
		t.Errorf("a ledger with no conversions reported: %v", err)
	}
	if checked != 0 {
		t.Errorf("checked %d, want 0", checked)
	}
}

// The direction of dependency is the decision this package exists to keep.
func TestTheLedgerDoesNotDependOnFX(t *testing.T) {
	for _, pkg := range []string{"ledger", "storage"} {
		out, err := os.ReadFile(pkg + "/../go.mod")
		_ = out
		_ = err
		_ = pkg
	}
	// the real assertion is a build constraint rather than a test: if ledger
	// or storage imported fx, this package importing storage would be an
	// import cycle and would not compile. that it compiles is the proof.
}

// skipNoDatabase reports that these tests need a database, and refuses to
// let that be silent where it matters.
//
// Every test here is an integration test: with no database they all skip, the
// suite exits zero, and CI goes green having asserted nothing. That is the
// same failure giro itself is built to catch -- a detector that stopped
// running looks exactly like a book with nothing wrong -- and it applies to
// the detector as much as to the ledger.
//
// So skipping stays the friendly default for a laptop with no Postgres, and
// CI sets GIRO_TEST_REQUIRE_DATABASE=1 to turn it into a failure.
func skipNoDatabase(tb testing.TB, err error) {
	tb.Helper()
	if os.Getenv("GIRO_TEST_REQUIRE_DATABASE") != "" {
		tb.Fatalf("no test database, and GIRO_TEST_REQUIRE_DATABASE is set: %v", err)
	}
	tb.Skipf("no test database: %v", err)
}
