package storage

import (
	"context"
	"math/big"
	"testing"

	"github.com/pixperk/giro/ledger"
)

// One client off ramp, end to end, in the accounts the business actually uses.
//
// This exists to answer one question before anything expensive is built on it:
// does the chart of accounts survive contact with a real flow? Account naming
// is the decision that is genuinely painful to change once there is data, so it
// gets exercised against real numbers rather than reasoned about.
//
// The deal: Acme sends 100,000 USDT over Tron and is quoted 0.25% plus a $25
// wire, so $99,725.00 arrives whatever the market does. Kraken pays 0.99960,
// so $99,960.00 comes back rather than $100,000.00, and the $40.00 difference
// between the price promised and the price obtained is carried by the book
// rather than by the client.

// scale 6, so one USDT is 1_000_000
func usdt(units int64) *big.Int {
	return new(big.Int).Mul(big.NewInt(units), big.NewInt(1_000_000))
}

const (
	subWallet = "client:acme:sub_wallet"
	treasury  = "treasury:usdt"
	ops       = "ops:usd"
	convFee   = "revenue:conversion_fee"
	wireFee   = "revenue:wire_fee"
	pegCost   = "cost:peg_absorption"

	chainTron  = "external:chain:tron:USDT"
	krakenUSDT = "external:lp:kraken:USDT"
	krakenUSD  = "external:lp:kraken:USD"
	bank       = "external:bank:infinitus:USD"
)

// Boundary accounts stand for something outside the ledger, and contra
// accounts are running totals of a cost. Both emit value, so both need the
// permission, and neither is inferred from its name.
//
// Flagged in both directions: which side of zero a boundary account sits on
// depends on which way money happened to flow, and that is not knowable at
// setup.
func setUpChartOfAccounts(t *testing.T, ctx context.Context, s *Store) {
	t.Helper()
	for _, a := range []struct {
		address ledger.Address
		asset   ledger.Asset
	}{
		{chainTron, "USDT/6"},
		{krakenUSDT, "USDT/6"},
		{krakenUSD, "USD/2"},
		{bank, "USD/2"},
		{pegCost, "USD/2"},
	} {
		if err := s.SetAllowNegative(ctx, a.address, a.asset, true); err != nil {
			t.Fatalf("setting up %s: %v", a.address, err)
		}
	}
}

func TestOffRampEndToEnd(t *testing.T) {
	ctx, s, pool := testStore(t)
	setUpChartOfAccounts(t, ctx, s)

	const (
		notional   = 100_000    // USDT, whole units
		fromKraken = 9_996_000  // $99,960.00 at 0.99960
		atPar      = 10_000_000 // $100,000.00 if USDT were exactly $1
		conversion = 25_000     // $250.00, 0.25%
		wire       = 2_500      // $25.00 flat
		toClient   = 9_972_500  // $99,725.00, par less both fees
	)
	peg := int64(atPar - fromKraken) // $40.00, absorbed

	// 1. the deposit arrives on chain. the sub wallet identifies the sender,
	//    which is the whole reason each client gets its own address.
	commit(t, ctx, s, "acme-deposit", ledger.Postings{
		{Source: chainTron, Destination: subWallet, Asset: "USDT/6", Amount: usdt(notional)},
	})

	// 2. swept into the treasury, because selling half a million dollars of
	//    stablecoin needs it in one place rather than across twenty addresses.
	commit(t, ctx, s, "acme-sweep", ledger.Postings{
		{Source: subWallet, Destination: treasury, Asset: "USDT/6", Amount: usdt(notional)},
	})

	// 3. the sale. two postings, one transaction, because the stablecoin
	//    leaving and the dollars arriving are one event and must not be
	//    separable. conservation holds per asset, so this is not a "swap"
	//    the ledger understands: it is two movements that commit together.
	//
	//    the rate lives in metadata. the ledger has no opinion on whether
	//    0.99960 was a fair price, which is correct, and it also means
	//    nothing here checks the arithmetic. that is step 8.
	commit(t, ctx, s, "acme-sale", ledger.Postings{
		{Source: treasury, Destination: krakenUSDT, Asset: "USDT/6", Amount: usdt(notional)},
		{Source: krakenUSD, Destination: ops, Asset: "USD/2", Amount: n(fromKraken)},
	}, "rate", "0.99960", "venue", "kraken")

	// 4. the fees, and the absorption that funds them. one transaction: the
	//    operating account holds $235.00 and owes $275.00 in fees, so booking
	//    them separately would be refused for overdrawing halfway through.
	//    the guard checks the final state, which is what makes this expressible
	//    at all.
	commit(t, ctx, s, "acme-fees", ledger.Postings{
		{Source: pegCost, Destination: ops, Asset: "USD/2", Amount: n(peg)},
		{Source: ops, Destination: convFee, Asset: "USD/2", Amount: n(conversion)},
		{Source: ops, Destination: wireFee, Asset: "USD/2", Amount: n(wire)},
	})

	// 5. the wire out.
	commit(t, ctx, s, "acme-wire", ledger.Postings{
		{Source: ops, Destination: bank, Asset: "USD/2", Amount: n(toClient)},
	})

	want := map[ledger.Address]struct {
		asset ledger.Asset
		value *big.Int
		why   string
	}{
		subWallet:  {"USDT/6", usdt(0), "swept, holds nothing"},
		treasury:   {"USDT/6", usdt(0), "sold, holds nothing"},
		chainTron:  {"USDT/6", usdt(-notional), "the chain sent us this much"},
		krakenUSDT: {"USDT/6", usdt(notional), "kraken received this much"},

		ops:       {"USD/2", n(0), "a conduit, not a store: nothing stranded"},
		krakenUSD: {"USD/2", n(-fromKraken), "kraken paid us this much"},
		bank:      {"USD/2", n(toClient), "the bank wired this much out"},
		convFee:   {"USD/2", n(conversion), "earned"},
		wireFee:   {"USD/2", n(wire), "earned"},
		pegCost:   {"USD/2", n(-peg), "cost of being off peg today"},
	}
	for address, w := range want {
		if got := balance(t, ctx, pool, address, w.asset); got.Cmp(w.value) != 0 {
			t.Errorf("%s = %s, want %s (%s)", address, got, w.value, w.why)
		}
	}

	assertConserved(t, ctx, pool)
	assertAllVerifiersPass(t, ctx, s)
}

// The payoff of splitting the boundary per counterparty: every external
// position is one balance read, and each one is a number to compare against
// that counterparty's own statement.
//
// With a single world account these would all be the same number, and telling
// them apart would mean a report over moves rather than a balance.
func TestBoundaryBalancesAreReconciliationAnchors(t *testing.T) {
	ctx, s, pool := testStore(t)
	setUpChartOfAccounts(t, ctx, s)
	runOneDeal(t, ctx, s)

	anchors := map[ledger.Address]struct {
		asset  ledger.Asset
		value  int64
		verify string
	}{
		chainTron:  {"USDT/6", -100_000_000_000, "compare against the Tron explorer"},
		krakenUSDT: {"USDT/6", 100_000_000_000, "compare against the Kraken trade history"},
		krakenUSD:  {"USD/2", -9_996_000, "compare against the Kraken settlement"},
		bank:       {"USD/2", 9_972_500, "compare against the bank statement"},
	}
	for address, a := range anchors {
		if got := balance(t, ctx, pool, address, a.asset); got.Int64() != a.value {
			t.Errorf("%s = %s, want %d (%s)", address, got, a.value, a.verify)
		}
	}

	// and the whole external position in one call, which is the shape a
	// reconciliation run wants
	external, err := s.AggregateBalances(ctx, "external:")
	if err != nil {
		t.Fatal(err)
	}
	if got := external["USDT/6"]; got == nil || got.Sign() != 0 {
		t.Errorf("external USDT/6 = %v, want 0: what came in over the chain went out to kraken", got)
	}
	if got := external["USD/2"]; got == nil || got.Int64() != -23_500 {
		t.Errorf("external USD/2 = %v, want -23500: dollars received less dollars paid out", got)
	}
}

// The business questions the chart of accounts has to answer, each as one
// prefix read rather than a report.
func TestChartOfAccountsAnswersTheBusinessQuestions(t *testing.T) {
	ctx, s, _ := testStore(t)
	setUpChartOfAccounts(t, ctx, s)
	runOneDeal(t, ctx, s)

	revenue, err := s.AggregateBalances(ctx, "revenue:")
	if err != nil {
		t.Fatal(err)
	}
	if got := revenue["USD/2"]; got == nil || got.Int64() != 27_500 {
		t.Errorf("revenue = %v, want 27500 ($275.00 gross)", got)
	}

	cost, err := s.AggregateBalances(ctx, "cost:")
	if err != nil {
		t.Fatal(err)
	}
	if got := cost["USD/2"]; got == nil || got.Int64() != -4_000 {
		t.Errorf("cost = %v, want -4000 ($40.00 of peg)", got)
	}

	// net is what the deal actually made, and it is two reads and a
	// subtraction rather than a query over transactions
	net := new(big.Int).Add(revenue["USD/2"], cost["USD/2"])
	if net.Int64() != 23_500 {
		t.Errorf("net = %s, want 23500 ($235.00)", net)
	}
}

// A settlement file arrives on Thursday describing Tuesday's wire, which is
// the normal case rather than the exception once anything is ingested from a
// bank or an exchange.
//
// The balance today is unchanged by when we found out. The balance as of
// Wednesday is not, and that is the difference effective dating exists for.
func TestBackdatedSettlementMovesTheHistoricalAnswer(t *testing.T) {
	ctx, s, pool := testStore(t)
	setUpChartOfAccounts(t, ctx, s)

	tuesday, wednesday, thursday := day(3), day(4), day(5)

	// booked on thursday, effective tuesday
	if _, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: krakenUSD, Destination: ops, Asset: "USD/2", Amount: n(9_996_000)},
	}, CommitOptions{Timestamp: tuesday, Reference: "late-settlement"}); err != nil {
		t.Fatal(err)
	}
	// and something we knew about all along, effective thursday
	if _, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: ops, Destination: bank, Asset: "USD/2", Amount: n(9_972_500)},
	}, CommitOptions{Timestamp: thursday, Reference: "the-wire"}); err != nil {
		t.Fatal(err)
	}

	asOf, err := s.GetBalancesAt(ctx, ops, wednesday)
	if err != nil {
		t.Fatal(err)
	}
	if got := asOf["USD/2"]; got == nil || got.Int64() != 9_996_000 {
		t.Errorf("ops as of wednesday = %v, want 9996000: the settlement was effective tuesday", got)
	}

	if got := balance(t, ctx, pool, ops, "USD/2"); got.Int64() != 23_500 {
		t.Errorf("ops today = %s, want 23500", got)
	}

	assertConserved(t, ctx, pool)
	assertAllVerifiersPass(t, ctx, s)
}

// One deal, twice, under the same reference. The second is refused rather than
// posting a hundred thousand dollars again.
func TestADealCannotBeBookedTwice(t *testing.T) {
	ctx, s, pool := testStore(t)
	setUpChartOfAccounts(t, ctx, s)

	postings := ledger.Postings{
		{Source: chainTron, Destination: subWallet, Asset: "USDT/6", Amount: usdt(100_000)},
	}
	commit(t, ctx, s, "acme-deposit", postings)

	if _, err := s.CommitTransaction(ctx, postings,
		CommitOptions{Reference: "acme-deposit"}); err == nil {
		t.Fatal("the same deposit was booked twice")
	}

	if got := balance(t, ctx, pool, subWallet, "USDT/6"); got.Cmp(usdt(100_000)) != 0 {
		t.Errorf("sub wallet = %s, want one deposit only", got)
	}
	assertConserved(t, ctx, pool)
}

// helpers

func commit(t *testing.T, ctx context.Context, s *Store, reference string, p ledger.Postings, metadata ...string) {
	t.Helper()
	m := ledger.Metadata{}
	for i := 0; i+1 < len(metadata); i += 2 {
		m[metadata[i]] = metadata[i+1]
	}
	if _, err := s.CommitTransaction(ctx, p, CommitOptions{Reference: reference, Metadata: m}); err != nil {
		t.Fatalf("%s: %v", reference, err)
	}
}

// the three that together say the book is what the log says it is
func assertAllVerifiersPass(t *testing.T, ctx context.Context, s *Store) {
	t.Helper()
	if _, err := s.VerifyLog(ctx); err != nil {
		t.Errorf("hash chain: %v", err)
	}
	if _, err := s.VerifyProjection(ctx); err != nil {
		t.Errorf("projection: %v", err)
	}
	if _, err := s.VerifyEffectiveVolumes(ctx); err != nil {
		t.Errorf("effective volumes: %v", err)
	}
	if _, err := s.VerifyBalancePermissions(ctx); err != nil {
		t.Errorf("balance permissions: %v", err)
	}
}

func runOneDeal(t *testing.T, ctx context.Context, s *Store) {
	t.Helper()
	commit(t, ctx, s, "acme-deposit", ledger.Postings{
		{Source: chainTron, Destination: subWallet, Asset: "USDT/6", Amount: usdt(100_000)},
	})
	commit(t, ctx, s, "acme-sweep", ledger.Postings{
		{Source: subWallet, Destination: treasury, Asset: "USDT/6", Amount: usdt(100_000)},
	})
	commit(t, ctx, s, "acme-sale", ledger.Postings{
		{Source: treasury, Destination: krakenUSDT, Asset: "USDT/6", Amount: usdt(100_000)},
		{Source: krakenUSD, Destination: ops, Asset: "USD/2", Amount: n(9_996_000)},
	}, "rate", "0.99960")
	commit(t, ctx, s, "acme-fees", ledger.Postings{
		{Source: pegCost, Destination: ops, Asset: "USD/2", Amount: n(4_000)},
		{Source: ops, Destination: convFee, Asset: "USD/2", Amount: n(25_000)},
		{Source: ops, Destination: wireFee, Asset: "USD/2", Amount: n(2_500)},
	})
	commit(t, ctx, s, "acme-wire", ledger.Postings{
		{Source: ops, Destination: bank, Asset: "USD/2", Amount: n(9_972_500)},
	})
}
