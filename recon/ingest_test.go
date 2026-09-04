package recon_test

import (
	"context"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	giro "github.com/pixperk/giro"
	"github.com/pixperk/giro/ledger"
	"github.com/pixperk/giro/migrate"
	"github.com/pixperk/giro/recon"
	"github.com/pixperk/giro/storage"
)

// An external test package on purpose: this exercises recon the way a caller
// composes it, through the engine's public surface.

// kraken is the sort of thing a deployment writes: a few dozen lines that call
// somebody's API and return normalised records. It touches no database and
// knows nothing about accounts, which is the whole point of the interface.
type kraken struct {
	lines []recon.Record
	calls int
}

func (k *kraken) ID() string   { return "kraken" }
func (k *kraken) Name() string { return "Kraken" }
func (k *kraken) Fetch(ctx context.Context, since time.Time) ([]recon.Record, error) {
	k.calls++
	return k.lines, nil
}

func testStore(t *testing.T) (context.Context, *storage.Store, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()

	url := os.Getenv("GIRO_TEST_DATABASE_URL")
	if url == "" {
		url = "postgres://" + os.Getenv("USER") + "@localhost:5432/giro_test"
	}
	schema := fmt.Sprintf("recon_%d_%d", os.Getpid(), len(t.Name()))

	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Skipf("no test database: %v", err)
	}
	if _, err := admin.Exec(ctx, "drop schema if exists "+schema+" cascade"); err != nil {
		t.Skipf("no test database: %v", err)
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

func line(id, ref string, amount int64, dir recon.Direction) recon.Record {
	return recon.Record{
		ID: id, Reference: ref, Asset: "USD/2",
		Amount: big.NewInt(amount), Direction: dir,
		OccurredAt: time.Now(), Raw: []byte(`{"id":"` + id + `"}`),
	}
}

// Staging is idempotent per (source, line id), which is what makes an ingest
// safe to retry after a timeout that may or may not have landed, and what
// makes overlapping windows the right way to page a statement rather than
// something to avoid.
func TestIngestIsIdempotent(t *testing.T) {
	ctx, _, pool := testStore(t)
	k := &kraken{lines: []recon.Record{
		line("L1", "W-1", 99_725_00, recon.Out),
		line("L2", "W-2", 12_400_00, recon.Out),
	}}
	if err := recon.Register(ctx, pool, "main", k); err != nil {
		t.Fatal(err)
	}

	staged, err := recon.Pull(ctx, pool, "main", k, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if staged != 2 {
		t.Fatalf("staged %d, want 2", staged)
	}

	// the same window again, which is what a scheduled run looks like
	staged, err = recon.Pull(ctx, pool, "main", k, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if staged != 0 {
		t.Errorf("re-ingesting staged %d, want 0", staged)
	}

	var rows int
	if err := pool.QueryRow(ctx, "select count(*) from recon_records where ledger='main'").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Errorf("%d rows staged in total, want 2", rows)
	}
}

// A file with one malformed line stages nothing. A partial ingest leaves
// nobody able to say which half arrived.
func TestAMalformedLineStagesNothing(t *testing.T) {
	ctx, _, pool := testStore(t)
	k := &kraken{}
	if err := recon.Register(ctx, pool, "main", k); err != nil {
		t.Fatal(err)
	}

	_, err := recon.Ingest(ctx, pool, "main", "kraken", []recon.Record{
		line("L1", "W-1", 100, recon.In),
		{ID: "L2", Asset: "USD/2", Amount: big.NewInt(-5)}, // a negative magnitude
	})
	if err == nil {
		t.Fatal("a negative magnitude was accepted")
	}

	var rows int
	pool.QueryRow(ctx, "select count(*) from recon_records where ledger='main'").Scan(&rows)
	if rows != 0 {
		t.Errorf("%d rows staged from a file that was refused", rows)
	}
}

func TestIngestRejectsMalformedRecords(t *testing.T) {
	ctx, _, pool := testStore(t)
	if err := recon.Register(ctx, pool, "main", &kraken{}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		why string
		r   recon.Record
	}{
		{"no id, so it cannot be staged idempotently",
			recon.Record{Asset: "USD/2", Amount: big.NewInt(1)}},
		{"an asset the ledger does not handle",
			recon.Record{ID: "x", Asset: "nonsense", Amount: big.NewInt(1)}},
		{"a zero amount, which is not a movement",
			recon.Record{ID: "x", Asset: "USD/2", Amount: big.NewInt(0)}},
		{"a direction that is neither",
			recon.Record{ID: "x", Asset: "USD/2", Amount: big.NewInt(1), Direction: "sideways"}},
	} {
		t.Run(tc.why, func(t *testing.T) {
			if _, err := recon.Ingest(ctx, pool, "main", "kraken", []recon.Record{tc.r}); err == nil {
				t.Error("accepted")
			}
		})
	}
}

// The raw line is kept, so a rule can later be replayed against what was
// actually received rather than against what we decided it meant.
func TestTheOriginalLineIsKept(t *testing.T) {
	ctx, _, pool := testStore(t)
	if err := recon.Register(ctx, pool, "main", &kraken{}); err != nil {
		t.Fatal(err)
	}
	if _, err := recon.Ingest(ctx, pool, "main", "kraken",
		[]recon.Record{line("L1", "W-1", 100, recon.In)}); err != nil {
		t.Fatal(err)
	}

	var raw string
	if err := pool.QueryRow(ctx,
		"select raw::text from recon_records where ledger='main' and record_id='L1'").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != `{"id":"L1"}` {
		t.Errorf("raw = %s, want the line as received", raw)
	}
}

// An unregistered source cannot stage lines, so a typo in a source id is an
// error rather than a queue of records nothing will ever match.
func TestAnUnknownSourceCannotStage(t *testing.T) {
	ctx, _, pool := testStore(t)
	_, err := recon.Ingest(ctx, pool, "main", "typo",
		[]recon.Record{line("L1", "W-1", 100, recon.In)})
	if err == nil {
		t.Fatal("an unregistered source staged a line")
	}
	// the foreign key, not a validation error: a typo in a source id must be
	// refused rather than becoming a queue of lines nothing will ever match
	if !strings.Contains(err.Error(), "recon_records_ledger_source_fkey") {
		t.Errorf("err = %v, want the source foreign key", err)
	}
}

// The default convention is giro's own, and a deployment that names its edges
// differently only has to say so.
func TestBoundaryIsConfigurable(t *testing.T) {
	if recon.DefaultBoundaryPrefix != "external:" {
		t.Errorf("default prefix = %q", recon.DefaultBoundaryPrefix)
	}
}

// no asset may create or destroy value, ever. asserted here as well as in the
// engine's own suite, because a reconciliation test that quietly broke the
// book while proving a match would be reporting on a fiction.
func assertConserved(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	rows, err := pool.Query(ctx, `
		select asset, (sum(input) - sum(output))::text
		  from accounts_volumes where ledger = 'main' group by asset`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var asset, drift string
		if err := rows.Scan(&asset, &drift); err != nil {
			t.Fatal(err)
		}
		if drift != "0" {
			t.Errorf("asset %s drifted by %s", asset, drift)
		}
	}
}
