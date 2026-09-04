package recon_test

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pixperk/giro/ledger"
	"github.com/pixperk/giro/recon"
	"github.com/pixperk/giro/storage"
)

// A day of off ramps, reconciled against the three counterparties that saw it.
//
// This is here to answer one question: is the Source interface sufficient for
// the flows this ledger exists to record? A worked example that only exercises
// the easy path proves nothing, so each source below carries one of the traps
// the design claims to handle.
//
// It is also the shape a real deployment writes. Each source is a few dozen
// lines that call an API and return records; nothing here touches accounts.

const (
	atTron   = ledger.Address("external:chain:tron:USDT")
	atKraken = ledger.Address("external:lp:kraken:USD")
	atPayer  = ledger.Address("external:bank:northwind:USD")
)

// a fixed set of lines, which is what an adapter returns once it has mapped
// somebody's API onto Record.
type statement struct {
	id, name string
	lines    []recon.Record
	holds    *big.Int // its own figure for the net position, when it can give one
}

func (s *statement) ID() string   { return s.id }
func (s *statement) Name() string { return s.name }
func (s *statement) Fetch(ctx context.Context, since time.Time) ([]recon.Record, error) {
	return s.lines, nil
}
func (s *statement) Balance(ctx context.Context, asset ledger.Asset) (*big.Int, error) {
	return s.holds, nil
}

func usdt(units int64) *big.Int {
	return new(big.Int).Mul(big.NewInt(units), big.NewInt(1_000_000))
}

func book(t *testing.T) (context.Context, *storage.Store, *pgxpool.Pool) {
	t.Helper()
	ctx, s, pool := testStore(t)
	for _, a := range []struct {
		address ledger.Address
		asset   ledger.Asset
	}{{atTron, "USDT/6"}, {atKraken, "USD/2"}, {atPayer, "USD/2"}} {
		if err := s.SetAllowNegative(ctx, a.address, a.asset, true); err != nil {
			t.Fatal(err)
		}
	}
	return ctx, s, pool
}

func commit(t *testing.T, ctx context.Context, s *storage.Store, ref string, p ledger.Postings) {
	t.Helper()
	opts := storage.CommitOptions{}
	if ref != "" {
		opts.Metadata = ledger.Metadata{recon.ExternalRefKey: ref}
	}
	if _, err := s.CommitTransaction(ctx, p, opts); err != nil {
		t.Fatalf("%s: %v", ref, err)
	}
}

// Three deposits arrive over Tron, are swept and sold, and one consolidated
// wire pays all three clients. Then each counterparty's own record of the day
// is reconciled against ours.
func TestADayOfOffRampsReconciles(t *testing.T) {
	ctx, s, pool := book(t)

	deposits := []struct {
		client string
		usdt   int64
		paid   int64 // cents wired, after fees
	}{
		{"acme", 40_000, 39_875_00},
		{"beta", 35_000, 34_890_00},
		{"gamma", 25_000, 24_910_00},
	}

	var wired int64
	for _, d := range deposits {
		wallet := ledger.Address("client:" + d.client + ":wallet")

		// the chain says money arrived, under its transaction hash
		commit(t, ctx, s, "0xdep-"+d.client, ledger.Postings{
			{Source: atTron, Destination: wallet, Asset: "USDT/6", Amount: usdt(d.usdt)},
		})
		// swept, which no counterparty sees: it is entirely ours
		commit(t, ctx, s, "", ledger.Postings{
			{Source: wallet, Destination: "treasury:usdt", Asset: "USDT/6", Amount: usdt(d.usdt)},
		})
		// sold. kraken sees the dollars, under its fill id.
		commit(t, ctx, s, "FILL-"+d.client, ledger.Postings{
			{Source: "treasury:usdt", Destination: "external:lp:kraken:USDT",
				Asset: "USDT/6", Amount: usdt(d.usdt)},
			{Source: atKraken, Destination: "ops:usd", Asset: "USD/2", Amount: big.NewInt(d.paid + 100_00)},
		})
		// paid out. all three share one wire reference, which is the point.
		commit(t, ctx, s, "WIRE-0142", ledger.Postings{
			{Source: "ops:usd", Destination: atPayer, Asset: "USD/2", Amount: big.NewInt(d.paid)},
		})
		wired += d.paid
	}
	if err := s.SetAllowNegative(ctx, "external:lp:kraken:USDT", "USDT/6", true); err != nil {
		t.Fatal(err)
	}

	// ── the chain: one line per deposit, each under its own hash ──
	chainSource := &statement{id: "tron", name: "Tron", holds: usdt(100_000)}
	for _, d := range deposits {
		chainSource.lines = append(chainSource.lines, recon.Record{
			ID: "tx-" + d.client, Reference: "0xdep-" + d.client,
			Asset: "USDT/6", Amount: usdt(d.usdt), Direction: recon.In,
		})
	}

	// ── the exchange: one line per fill ──
	krakenSource := &statement{id: "kraken", name: "Kraken"}
	for _, d := range deposits {
		krakenSource.lines = append(krakenSource.lines, recon.Record{
			ID: "led-" + d.client, Reference: "FILL-" + d.client,
			Asset: "USD/2", Amount: big.NewInt(d.paid + 100_00), Direction: recon.In,
		})
	}

	// ── the bank: ONE line for all three payouts ──
	bankSource := &statement{id: "northwind", name: "Northwind Bank", lines: []recon.Record{{
		ID: "stmt-1", Reference: "WIRE-0142",
		Asset: "USD/2", Amount: big.NewInt(wired), Direction: recon.Out,
	}}}

	for _, src := range []recon.Source{chainSource, krakenSource, bankSource} {
		if err := recon.Register(ctx, pool, "main", src); err != nil {
			t.Fatal(err)
		}
		if _, err := recon.Pull(ctx, pool, "main", src, time.Time{}); err != nil {
			t.Fatal(err)
		}
	}

	sum, err := recon.Match(ctx, pool, "main", recon.Config{})
	if err != nil {
		t.Fatal(err)
	}

	// seven lines: three deposits, three fills, one consolidated wire
	if sum.Matched != 7 || sum.Variance != 0 || len(sum.Unmatched) != 0 {
		t.Fatalf("summary = %+v, want seven clean matches", sum)
	}

	// the wire matched three movements as one set, and each carries the size
	var rows, setSize int
	if err := pool.QueryRow(ctx, `
		select count(*), max(set_size) from recon_matches
		 where ledger='main' and source='northwind'`).Scan(&rows, &setSize); err != nil {
		t.Fatal(err)
	}
	if rows != 3 || setSize != 3 {
		t.Errorf("the consolidated wire recorded %d matches of set size %d, want 3 and 3", rows, setSize)
	}

	// and the position agrees with the chain
	got, err := recon.CompareBalance(ctx, pool, "main", chainSource, atTron, "USDT/6")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Agrees() {
		t.Errorf("positions disagree: %s", got.Error())
	}

	assertConserved(t, ctx, pool)
}

// The trap worth having a test for. An outbound wire and an inbound deposit,
// same reference, same amount. Without the direction check this is a clean
// looking match that is completely wrong, and nothing downstream would ever
// question it.
func TestTheDirectionTrap(t *testing.T) {
	ctx, s, pool := book(t)

	// something to refund out of
	commit(t, ctx, s, "", ledger.Postings{
		{Source: atPayer, Destination: "ops:usd", Asset: "USD/2", Amount: big.NewInt(50_000_00)},
	})

	// a refund going out, and a deposit coming in, that happen to share a
	// reference and a size. this is not contrived: a returned payment often
	// carries the reference of the payment it returns.
	commit(t, ctx, s, "REF-9", ledger.Postings{
		{Source: "ops:usd", Destination: atPayer, Asset: "USD/2", Amount: big.NewInt(50_000_00)},
	})
	commit(t, ctx, s, "REF-9", ledger.Postings{
		{Source: atPayer, Destination: "ops:usd", Asset: "USD/2", Amount: big.NewInt(50_000_00)},
	})

	bank := &statement{id: "northwind", name: "Northwind Bank", lines: []recon.Record{{
		ID: "stmt-out", Reference: "REF-9",
		Asset: "USD/2", Amount: big.NewInt(50_000_00), Direction: recon.Out,
	}}}
	if err := recon.Register(ctx, pool, "main", bank); err != nil {
		t.Fatal(err)
	}
	if _, err := recon.Pull(ctx, pool, "main", bank, time.Time{}); err != nil {
		t.Fatal(err)
	}

	sum, err := recon.Match(ctx, pool, "main", recon.Config{})
	if err != nil {
		t.Fatal(err)
	}

	// exactly one movement went out, so exactly one match, and it is that one
	if sum.Matched != 1 {
		t.Fatalf("summary = %+v, want one match", sum)
	}
	var isSource bool
	if err := pool.QueryRow(ctx, `
		select m.is_source from recon_matches x
		  join moves m on m.ledger = x.ledger and m.seq = x.move_seq
		 where x.ledger = 'main'`).Scan(&isSource); err != nil {
		t.Fatal(err)
	}
	// money leaving credits the boundary account, so the move on it is not the
	// source. matching the other one would have been the wrong direction.
	if isSource {
		t.Error("an outbound line matched the movement that came in")
	}
}

// The case that justifies comparing positions at all.
//
// A deposit the source never mentions cannot fail to match, because no line
// was sent for it. Every line the chain did send matches, the report is clean,
// and the money is short. Only the balance notices.
func TestADepositTheSourceNeverMentions(t *testing.T) {
	ctx, s, pool := book(t)

	// two deposits on our books
	for _, c := range []string{"acme", "beta"} {
		commit(t, ctx, s, "0xdep-"+c, ledger.Postings{
			{Source: atTron, Destination: ledger.Address("client:" + c), Asset: "USDT/6", Amount: usdt(50_000)},
		})
	}

	// the chain reports only one of them, and says it sent only that much
	chainSource := &statement{id: "tron", name: "Tron", holds: usdt(50_000), lines: []recon.Record{{
		ID: "tx-acme", Reference: "0xdep-acme",
		Asset: "USDT/6", Amount: usdt(50_000), Direction: recon.In,
	}}}
	if err := recon.Register(ctx, pool, "main", chainSource); err != nil {
		t.Fatal(err)
	}
	if _, err := recon.Pull(ctx, pool, "main", chainSource, time.Time{}); err != nil {
		t.Fatal(err)
	}

	// matching is perfectly happy: every line it was given matched
	sum, err := recon.Match(ctx, pool, "main", recon.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Matched != 1 || len(sum.Unmatched) != 0 {
		t.Fatalf("summary = %+v, want a clean report", sum)
	}

	// and so is every check inside the ledger
	assertConserved(t, ctx, pool)
	for name, check := range map[string]func(context.Context) (int, error){
		"conservation": s.VerifyConservation,
		"log":          s.VerifyLog,
		"projection":   s.VerifyProjection,
	} {
		if _, err := check(ctx); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	// only the position disagrees
	got, err := recon.CompareBalance(ctx, pool, "main", chainSource, atTron, "USDT/6")
	if err != nil {
		t.Fatal(err)
	}
	if got.Agrees() {
		t.Fatal("fifty thousand went unnoticed by everything")
	}
	// negative: they say less came across than we recorded, which is the right
	// way round for a deposit we have and they have never heard of
	if got.Difference.Cmp(usdt(-50_000)) != 0 {
		t.Errorf("difference = %s, want -50,000 USDT", got.Difference)
	}
	t.Logf("reported: %s", got.Error())
}

// A statement line for a payment we have not recorded at all, which is the
// other direction of the same problem and the one matching does catch.
func TestALineForAPaymentWeNeverMade(t *testing.T) {
	ctx, _, pool := book(t)

	bank := &statement{id: "northwind", name: "Northwind Bank", lines: []recon.Record{{
		ID: "stmt-ghost", Reference: "WIRE-NOBODY-MADE",
		Asset: "USD/2", Amount: big.NewInt(12_000_00), Direction: recon.Out,
	}}}
	if err := recon.Register(ctx, pool, "main", bank); err != nil {
		t.Fatal(err)
	}
	if _, err := recon.Pull(ctx, pool, "main", bank, time.Time{}); err != nil {
		t.Fatal(err)
	}

	sum, err := recon.Match(ctx, pool, "main", recon.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Unmatched[recon.NotFound] != 1 {
		t.Fatalf("summary = %+v, want it reported as naming nothing here", sum)
	}

	// and it ages into a finding rather than sitting in a queue nobody reads
	var stale recon.StaleBreak
	if _, err := recon.Unmatched(ctx, pool, "main", 0); !errors.As(err, &stale) {
		t.Fatalf("err = %v, want StaleBreak", err)
	}
	if stale.Reference != "WIRE-NOBODY-MADE" {
		t.Errorf("reported %+v", stale)
	}
}
