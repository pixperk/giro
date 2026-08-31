package storage

import (
	"testing"
	"time"

	"github.com/pixperk/giro/internal/ledger"
)

func TestStatementReadsInEffectiveOrder(t *testing.T) {
	ctx, s, _ := testStore(t)
	commitAt(t, ctx, s, "world", "alice", 100, day(1))
	commitAt(t, ctx, s, "alice", "bob", 30, day(5))
	// a settlement file describing day 3, arriving last
	commitAt(t, ctx, s, "world", "alice", 50, day(3))

	page, err := s.ListMoves(ctx, ListMovesQuery{Filter: MoveFilter{Address: "alice"}, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 3 {
		t.Fatalf("%d moves, want 3", len(page.Items))
	}

	// a statement is the order things happened, not the order we heard
	wantDays := []int{1, 3, 5}
	for i, m := range page.Items {
		if m.EffectiveDate.Day() != wantDays[i] {
			t.Errorf("row %d is day %d, want day %d", i, m.EffectiveDate.Day(), wantDays[i])
		}
	}

	// running balance follows the effective ordering
	wantBalances := []int64{100, 150, 120}
	for i, m := range page.Items {
		if m.EffectiveVolumes == nil {
			t.Fatalf("row %d has no effective volumes", i)
		}
		if got := m.EffectiveVolumes.Balance(); got.Cmp(n(wantBalances[i])) != 0 {
			t.Errorf("row %d effective balance = %s, want %d", i, got, wantBalances[i])
		}
	}

	// direction is stated from this account's point of view
	if !page.Items[0].Incoming || page.Items[2].Incoming {
		t.Error("directions are wrong: day 1 is money in, day 5 is money out")
	}
}

func TestStatementIsPerAccount(t *testing.T) {
	ctx, s, _ := testStore(t)
	fund(t, ctx, s, "alice", 100)
	fund(t, ctx, s, "bob", 100)
	mustCommit(t, ctx, s, "alice", "bob", 30)

	alice, err := s.ListMoves(ctx, ListMovesQuery{Filter: MoveFilter{Address: "alice"}, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(alice.Items) != 2 {
		t.Errorf("alice has %d moves, want 2", len(alice.Items))
	}
	for _, m := range alice.Items {
		if m.Asset != "USD/2" {
			t.Errorf("unexpected asset %s", m.Asset)
		}
	}

	world, err := s.ListMoves(ctx, ListMovesQuery{Filter: MoveFilter{Address: "world"}, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(world.Items) != 2 {
		t.Errorf("world has %d moves, want 2", len(world.Items))
	}
}

func TestStatementFilters(t *testing.T) {
	ctx, s, _ := testStore(t)
	commitAt(t, ctx, s, "world", "alice", 100, day(1))
	commitAt(t, ctx, s, "world", "alice", 100, day(10))
	commitAt(t, ctx, s, "world", "alice", 100, day(20))

	if _, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "world", Destination: "alice", Asset: "EUR/2", Amount: n(500)},
	}, CommitOptions{Timestamp: day(15)}); err != nil {
		t.Fatal(err)
	}

	t.Run("by asset", func(t *testing.T) {
		page, err := s.ListMoves(ctx, ListMovesQuery{
			Filter: MoveFilter{Address: "alice", Asset: "EUR/2"}, Limit: 50,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 1 || page.Items[0].Asset != "EUR/2" {
			t.Errorf("got %d rows", len(page.Items))
		}
	})

	t.Run("by date range", func(t *testing.T) {
		from, to := day(5), day(16)
		page, err := s.ListMoves(ctx, ListMovesQuery{
			Filter: MoveFilter{Address: "alice", From: &from, To: &to}, Limit: 50,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 2 {
			t.Fatalf("%d rows in the window, want 2 (day 10 and day 15)", len(page.Items))
		}
		for _, m := range page.Items {
			if m.EffectiveDate.Before(from) || m.EffectiveDate.After(to) {
				t.Errorf("row on %v is outside the window", m.EffectiveDate)
			}
		}
	})
}

// effective_date is not unique, so the pagination key is (effective_date, seq).
// paginating on the date alone would skip or repeat rows sharing one.
func TestStatementPaginatesAcrossTiedDates(t *testing.T) {
	ctx, s, _ := testStore(t)
	for range 7 {
		commitAt(t, ctx, s, "world", "alice", 10, day(1))
	}
	for range 3 {
		commitAt(t, ctx, s, "world", "alice", 10, day(2))
	}

	var seen []int64
	q := ListMovesQuery{Filter: MoveFilter{Address: "alice"}, Limit: 3}
	for {
		page, err := s.ListMoves(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range page.Items {
			seen = append(seen, m.Seq)
		}
		if page.Next == "" {
			break
		}
		q = ListMovesQuery{Cursor: page.Next}
	}

	if len(seen) != 10 {
		t.Fatalf("walked %d moves, want 10", len(seen))
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] <= seen[i-1] {
			t.Fatalf("position %d has seq %d after %d: a page was repeated or misordered",
				i, seen[i], seen[i-1])
		}
	}
}

func TestStatementCursorCarriesItsFilter(t *testing.T) {
	ctx, s, _ := testStore(t)
	for range 4 {
		commitAt(t, ctx, s, "world", "alice", 10, day(1))
	}
	if _, err := s.CommitTransaction(ctx, ledger.Postings{
		{Source: "world", Destination: "alice", Asset: "EUR/2", Amount: n(1)},
	}, CommitOptions{Timestamp: day(1)}); err != nil {
		t.Fatal(err)
	}

	first, err := s.ListMoves(ctx, ListMovesQuery{
		Filter: MoveFilter{Address: "alice", Asset: "USD/2"}, Limit: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Next == "" {
		t.Fatal("expected a second page")
	}

	// the filter is deliberately absent from this call
	// neither the address nor the filter is repeated: the cursor carries both
	second, err := s.ListMoves(ctx, ListMovesQuery{Cursor: first.Next})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range second.Items {
		if m.Asset != "USD/2" {
			t.Errorf("a %s move leaked past the cursor's filter", m.Asset)
		}
	}
}

func TestStatementOfAnUnusedAccount(t *testing.T) {
	ctx, s, _ := testStore(t)
	page, err := s.ListMoves(ctx, ListMovesQuery{Filter: MoveFilter{Address: "nobody"}, Limit: 50})
	if err != nil {
		t.Fatalf("an unused account should not be an error: %v", err)
	}
	if len(page.Items) != 0 || page.Next != "" {
		t.Errorf("got %d rows", len(page.Items))
	}
}

func TestStatementIsScopedToItsLedger(t *testing.T) {
	ctx, mine, theirs, _ := twoLedgers(t)

	a, err := mine.ListMoves(ctx, ListMovesQuery{Filter: MoveFilter{Address: "users:alice"}, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	b, err := theirs.ListMoves(ctx, ListMovesQuery{Filter: MoveFilter{Address: "users:alice"}, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Items) == 0 || len(b.Items) == 0 {
		t.Fatal("both ledgers should have moves for alice")
	}
	if a.Items[0].Amount.Cmp(b.Items[0].Amount) == 0 {
		t.Errorf("both ledgers report the same first amount %s: the query is not scoped",
			a.Items[0].Amount)
	}
}

// the statement's running balance must agree with the point in time query.
func TestStatementAgreesWithBalancesAt(t *testing.T) {
	ctx, s, _ := testStore(t)
	commitAt(t, ctx, s, "world", "alice", 100, day(1))
	commitAt(t, ctx, s, "alice", "bob", 30, day(5))
	commitAt(t, ctx, s, "world", "alice", 50, day(3))

	page, err := s.ListMoves(ctx, ListMovesQuery{Filter: MoveFilter{Address: "alice"}, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}

	for _, m := range page.Items {
		at, err := s.GetBalancesAt(ctx, "alice", m.EffectiveDate)
		if err != nil {
			t.Fatal(err)
		}
		want := m.EffectiveVolumes.Balance()
		if at["USD/2"].Cmp(want) != 0 {
			t.Errorf("on %v the statement says %s and GetBalancesAt says %s",
				m.EffectiveDate.Format(time.DateOnly), want, at["USD/2"])
		}
	}
}
