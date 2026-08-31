package api

import (
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"testing"
)

func posting(source, destination, asset string, amount int64) map[string]any {
	return map[string]any{"source": source, "destination": destination, "asset": asset, "amount": amount}
}

func TestCreateLedger(t *testing.T) {
	s := newTestServer(t)

	rec := do(t, s, http.MethodPost, "/v1/ledgers/demo", nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if got := decode[Ledger](t, rec); got.Name != "demo" || got.AddedAt.IsZero() {
		t.Errorf("got %+v", got)
	}

	// a ledger has counters that allocate ids, so creating it twice would be
	// ambiguous rather than harmless
	again := do(t, s, http.MethodPost, "/v1/ledgers/demo", nil)
	if again.Code != http.StatusConflict {
		t.Errorf("second create = %d, want 409", again.Code)
	}
	if code := decode[Error](t, again).Code; code != CONFLICT {
		t.Errorf("code = %s, want CONFLICT", code)
	}
}

func TestCreateTransaction(t *testing.T) {
	s := newTestServer(t)
	base := newLedger(t, s, "demo")

	rec := do(t, s, http.MethodPost, base+"/transactions", map[string]any{
		"postings":  []map[string]any{posting("world", "users:alice", "USD/2", 10000)},
		"reference": "order-1",
		"metadata":  map[string]string{"kind": "payout"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}

	tx := decode[Transaction](t, rec)
	if tx.Id != 1 {
		t.Errorf("id = %d, want 1", tx.Id)
	}
	if tx.Reference == nil || *tx.Reference != "order-1" {
		t.Errorf("reference = %v", tx.Reference)
	}
	if tx.Metadata == nil || (*tx.Metadata)["kind"] != "payout" {
		t.Errorf("metadata = %v", tx.Metadata)
	}
	if tx.PostCommitVolumes == nil {
		t.Fatal("post commit volumes missing")
	}
	if got := (*tx.PostCommitVolumes)["users:alice"]["USD/2"].Input; got.Cmp(big.NewInt(10000)) != 0 {
		t.Errorf("alice input = %s, want 10000", got)
	}
	if tx.InsertedAt.IsZero() || tx.Timestamp.IsZero() {
		t.Error("timestamps missing")
	}
}

func TestCreateTransactionRejections(t *testing.T) {
	tests := []struct {
		why      string
		body     any
		want     int
		wantCode ErrorCode
	}{
		{
			why:      "not json at all",
			body:     "{",
			want:     http.StatusBadRequest,
			wantCode: VALIDATION,
		},
		{
			why:      "an unknown field, which usually means a typo rather than an extension",
			body:     map[string]any{"postings": []map[string]any{posting("world", "a", "USD/2", 1)}, "postingz": 1},
			want:     http.StatusBadRequest,
			wantCode: VALIDATION,
		},
		{
			why:      "no postings",
			body:     map[string]any{"postings": []map[string]any{}},
			want:     http.StatusBadRequest,
			wantCode: VALIDATION,
		},
		{
			why:      "a negative amount, which would be a withdrawal from the destination",
			body:     map[string]any{"postings": []map[string]any{posting("world", "a", "USD/2", -5)}},
			want:     http.StatusBadRequest,
			wantCode: VALIDATION,
		},
		{
			why:      "an address with a trailing space, which is not the same account",
			body:     map[string]any{"postings": []map[string]any{posting("world ", "a", "USD/2", 1)}},
			want:     http.StatusBadRequest,
			wantCode: VALIDATION,
		},
		{
			why:      "a lowercase asset",
			body:     map[string]any{"postings": []map[string]any{posting("world", "a", "usd", 1)}},
			want:     http.StatusBadRequest,
			wantCode: VALIDATION,
		},
	}

	s := newTestServer(t)
	base := newLedger(t, s, "demo")

	for _, tc := range tests {
		t.Run(tc.why, func(t *testing.T) {
			rec := do(t, s, http.MethodPost, base+"/transactions", tc.body)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
			if got := decode[Error](t, rec).Code; got != tc.wantCode {
				t.Errorf("code = %s, want %s", got, tc.wantCode)
			}
		})
	}
}

// the postings are well formed, they just cannot be applied, which is 422
// rather than 400.
func TestInsufficientFundsCarriesTheNumbers(t *testing.T) {
	s := newTestServer(t)
	base := newLedger(t, s, "demo")
	fund(t, s, base, "users:alice", 100)

	rec := do(t, s, http.MethodPost, base+"/transactions", map[string]any{
		"postings": []map[string]any{posting("users:alice", "users:bob", "USD/2", 500)},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body.String())
	}

	e := decode[Error](t, rec)
	if e.Code != INSUFFICIENTFUNDS {
		t.Fatalf("code = %s", e.Code)
	}
	if e.Details == nil {
		t.Fatal("no details, a client cannot act on the message alone")
	}
	d := *e.Details
	if d["account"] != "users:alice" || d["available"] != "100" || d["requested"] != "500" {
		t.Errorf("details = %v", d)
	}
}

func TestIdempotency(t *testing.T) {
	s := newTestServer(t)
	base := newLedger(t, s, "demo")

	body := map[string]any{"postings": []map[string]any{posting("world", "users:alice", "USD/2", 10000)}}

	first := do(t, s, http.MethodPost, base+"/transactions", body, "Idempotency-Key", "req-1")
	if first.Code != http.StatusCreated {
		t.Fatalf("%d %s", first.Code, first.Body.String())
	}
	second := do(t, s, http.MethodPost, base+"/transactions", body, "Idempotency-Key", "req-1")
	if second.Code != http.StatusCreated {
		t.Fatalf("replay = %d", second.Code)
	}

	if a, b := decode[Transaction](t, first), decode[Transaction](t, second); a.Id != b.Id {
		t.Errorf("replay returned transaction %d, original was %d", b.Id, a.Id)
	}

	balances := decode[Balances](t, do(t, s, http.MethodGet, base+"/accounts/users:alice/balances", nil))
	if balances["USD/2"].Cmp(big.NewInt(10000)) != 0 {
		t.Errorf("alice = %s, want 10000: the money must move once", balances["USD/2"])
	}

	// the half that matters: the same key with different inputs is a client
	// bug, and returning the first result would hide a payment that never
	// happened
	different := do(t, s, http.MethodPost, base+"/transactions", map[string]any{
		"postings": []map[string]any{posting("world", "users:bob", "USD/2", 50000)},
	}, "Idempotency-Key", "req-1")
	if different.Code != http.StatusConflict {
		t.Fatalf("reused key with new inputs = %d, want 409", different.Code)
	}
	if code := decode[Error](t, different).Code; code != IDEMPOTENCYMISMATCH {
		t.Errorf("code = %s", code)
	}
}

func TestDuplicateReference(t *testing.T) {
	s := newTestServer(t)
	base := newLedger(t, s, "demo")

	body := func(dest string) map[string]any {
		return map[string]any{
			"postings":  []map[string]any{posting("world", dest, "USD/2", 100)},
			"reference": "order-1",
		}
	}
	if rec := do(t, s, http.MethodPost, base+"/transactions", body("a")); rec.Code != http.StatusCreated {
		t.Fatal(rec.Body.String())
	}
	rec := do(t, s, http.MethodPost, base+"/transactions", body("b"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if code := decode[Error](t, rec).Code; code != CONFLICT {
		t.Errorf("code = %s", code)
	}
}

// json numbers above 2^53 are the whole reason amounts are typed rather than
// decoded into any.
func TestLargeAmountsSurviveTheWire(t *testing.T) {
	s := newTestServer(t)
	base := newLedger(t, s, "demo")

	const huge = "100000000000000000000000000000001"
	rec := do(t, s, http.MethodPost, base+"/transactions",
		`{"postings":[{"source":"world","destination":"whale","asset":"TOKEN/18","amount":`+huge+`}]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), huge) {
		t.Errorf("amount was altered in the response: %s", rec.Body.String())
	}

	balances := decode[Balances](t, do(t, s, http.MethodGet, base+"/accounts/whale/balances", nil))
	want, _ := new(big.Int).SetString(huge, 10)
	if balances["TOKEN/18"].Cmp(want) != 0 {
		t.Errorf("balance = %s, want %s", balances["TOKEN/18"], huge)
	}
}

func TestGetTransaction(t *testing.T) {
	s := newTestServer(t)
	base := newLedger(t, s, "demo")
	created := fund(t, s, base, "users:alice", 10000)

	rec := do(t, s, http.MethodGet, base+"/transactions/1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if got := decode[Transaction](t, rec); got.Id != created.Id {
		t.Errorf("id = %d, want %d", got.Id, created.Id)
	}

	missing := do(t, s, http.MethodGet, base+"/transactions/99", nil)
	if missing.Code != http.StatusNotFound {
		t.Errorf("missing = %d, want 404", missing.Code)
	}
	if code := decode[Error](t, missing).Code; code != NOTFOUND {
		t.Errorf("code = %s", code)
	}

	bad := do(t, s, http.MethodGet, base+"/transactions/abc", nil)
	if bad.Code != http.StatusBadRequest {
		t.Errorf("non numeric id = %d, want 400", bad.Code)
	}
}

func TestListTransactionsAndPagination(t *testing.T) {
	s := newTestServer(t)
	base := newLedger(t, s, "demo")
	for range 5 {
		fund(t, s, base, "users:alice", 10)
	}
	fund(t, s, base, "users:bob", 10)

	all := decode[TransactionPage](t, do(t, s, http.MethodGet, base+"/transactions?limit=100", nil))
	if len(all.Items) != 6 {
		t.Fatalf("%d transactions, want 6", len(all.Items))
	}

	filtered := decode[TransactionPage](t, do(t, s, http.MethodGet, base+"/transactions?account=users:bob", nil))
	if len(filtered.Items) != 1 {
		t.Errorf("%d touching bob, want 1", len(filtered.Items))
	}

	// walk every page and check nothing is skipped or repeated
	var seen []int64
	path := base + "/transactions?limit=2"
	for {
		page := decode[TransactionPage](t, do(t, s, http.MethodGet, path, nil))
		for _, tx := range page.Items {
			seen = append(seen, tx.Id)
		}
		if page.Next == nil {
			break
		}
		path = base + "/transactions?cursor=" + *page.Next
	}
	if len(seen) != 6 {
		t.Fatalf("walked %d, want 6", len(seen))
	}
	for i, id := range seen {
		if id != int64(i+1) {
			t.Errorf("position %d is id %d, want %d", i, id, i+1)
		}
	}
}

func TestListTransactionsRejectsBadParameters(t *testing.T) {
	s := newTestServer(t)
	base := newLedger(t, s, "demo")

	for _, query := range []string{"?limit=abc", "?cursor=not-base64!!"} {
		rec := do(t, s, http.MethodGet, base+"/transactions"+query, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", query, rec.Code)
		}
		if code := decode[Error](t, rec).Code; code != VALIDATION {
			t.Errorf("%s code = %s", query, code)
		}
	}
}

func TestAccounts(t *testing.T) {
	s := newTestServer(t)
	base := newLedger(t, s, "demo")
	fund(t, s, base, "users:alice", 10000)
	if rec := do(t, s, http.MethodPost, base+"/transactions", map[string]any{
		"postings": []map[string]any{posting("users:alice", "fees", "USD/2", 250)},
	}); rec.Code != http.StatusCreated {
		t.Fatal(rec.Body.String())
	}

	t.Run("volumes omitted by default", func(t *testing.T) {
		a := decode[Account](t, do(t, s, http.MethodGet, base+"/accounts/users:alice", nil))
		if a.Address != "users:alice" {
			t.Errorf("address = %q", a.Address)
		}
		if a.Volumes != nil {
			t.Errorf("volumes present without asking: %v", a.Volumes)
		}
	})

	t.Run("expand volumes", func(t *testing.T) {
		a := decode[Account](t, do(t, s, http.MethodGet, base+"/accounts/users:alice?expand=volumes", nil))
		if a.Volumes == nil {
			t.Fatal("volumes missing")
		}
		v := (*a.Volumes)["USD/2"]
		if v.Input.Cmp(big.NewInt(10000)) != 0 || v.Output.Cmp(big.NewInt(250)) != 0 {
			t.Errorf("volumes = (%s, %s), want (10000, 250)", v.Input, v.Output)
		}
	})

	t.Run("an unknown expand is an error, not silence", func(t *testing.T) {
		rec := do(t, s, http.MethodGet, base+"/accounts/users:alice?expand=nope", nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("an account that was never touched", func(t *testing.T) {
		rec := do(t, s, http.MethodGet, base+"/accounts/users:nobody", nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
		// but its balances are an empty object rather than a 404: an unused
		// address and a nonexistent one both hold nothing
		balances := do(t, s, http.MethodGet, base+"/accounts/users:nobody/balances", nil)
		if balances.Code != http.StatusOK {
			t.Errorf("balances = %d, want 200", balances.Code)
		}
		if len(decode[Balances](t, balances)) != 0 {
			t.Error("expected no balances")
		}
	})
}

func TestAggregateBalances(t *testing.T) {
	s := newTestServer(t)
	base := newLedger(t, s, "demo")
	fund(t, s, base, "users:alice", 100)
	fund(t, s, base, "users:bob", 200)
	fund(t, s, base, "fees:users:refunds", 900)

	t.Run("whole ledger is always zero", func(t *testing.T) {
		got := decode[Balances](t, do(t, s, http.MethodGet, base+"/balances", nil))
		if got["USD/2"].Sign() != 0 {
			t.Errorf("conservation = %s, want 0", got["USD/2"])
		}
	})

	t.Run("prefix matches on segment boundaries", func(t *testing.T) {
		got := decode[Balances](t, do(t, s, http.MethodGet, base+"/balances?prefix=users:", nil))
		if got["USD/2"].Cmp(big.NewInt(300)) != 0 {
			t.Errorf("users:* = %s, want 300: fees:users:refunds must not match", got["USD/2"])
		}
	})
}

func TestListLogs(t *testing.T) {
	s := newTestServer(t)
	base := newLedger(t, s, "demo")
	fund(t, s, base, "users:alice", 10000)
	fund(t, s, base, "users:bob", 20000)

	page := decode[LogPage](t, do(t, s, http.MethodGet, base+"/logs", nil))
	if len(page.Items) != 2 {
		t.Fatalf("%d entries, want 2", len(page.Items))
	}
	if page.Items[0].Type != NEWTRANSACTION {
		t.Errorf("type = %s", page.Items[0].Type)
	}
	if len(page.Items[0].Hash) != 32 {
		t.Errorf("hash is %d bytes, want a 32 byte sha256", len(page.Items[0].Hash))
	}

	// the entry is passed through as raw bytes, so an amount inside it is not
	// re-encoded through a float on the way out
	if !strings.Contains(string(page.Items[1].Data), `"amount":20000`) {
		t.Errorf("log data was re-encoded: %s", page.Items[1].Data)
	}
}

// an unknown ledger must not look like a server fault.
func TestUnknownLedger(t *testing.T) {
	s := newTestServer(t)

	rec := do(t, s, http.MethodPost, "/v1/ledgers/nope/transactions", map[string]any{
		"postings": []map[string]any{posting("world", "a", "USD/2", 1)},
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	if code := decode[Error](t, rec).Code; code != NOTFOUND {
		t.Errorf("code = %s", code)
	}
}

func TestTransactionMetadataEndpoints(t *testing.T) {
	s := newTestServer(t)
	base := newLedger(t, s, "demo")
	fund(t, s, base, "users:alice", 100)

	set := do(t, s, http.MethodPost, base+"/transactions/1/metadata",
		map[string]string{"orderId": "ord_1"})
	if set.Code != http.StatusOK {
		t.Fatalf("%d %s", set.Code, set.Body.String())
	}
	if got := decode[Transaction](t, set); (*got.Metadata)["orderId"] != "ord_1" {
		t.Errorf("metadata = %v", got.Metadata)
	}

	// merges rather than replaces
	merged := decode[Transaction](t, do(t, s, http.MethodPost, base+"/transactions/1/metadata",
		map[string]string{"provider": "stripe"}))
	if m := *merged.Metadata; m["orderId"] != "ord_1" || m["provider"] != "stripe" {
		t.Errorf("metadata = %v, want both keys", m)
	}

	deleted := decode[Transaction](t, do(t, s, http.MethodDelete, base+"/transactions/1/metadata/orderId", nil))
	if _, ok := (*deleted.Metadata)["orderId"]; ok {
		t.Errorf("key survived deletion: %v", deleted.Metadata)
	}

	missing := do(t, s, http.MethodPost, base+"/transactions/99/metadata", map[string]string{"a": "1"})
	if missing.Code != http.StatusNotFound {
		t.Errorf("missing transaction = %d, want 404", missing.Code)
	}
}

func TestAccountMetadataEndpoints(t *testing.T) {
	s := newTestServer(t)
	base := newLedger(t, s, "demo")

	// tagging an account before any money has moved through it
	created := do(t, s, http.MethodPost, base+"/accounts/users:future/metadata",
		map[string]string{"userId": "u_1"})
	if created.Code != http.StatusOK {
		t.Fatalf("%d %s", created.Code, created.Body.String())
	}
	a := decode[Account](t, created)
	if a.Address != "users:future" || (*a.Metadata)["userId"] != "u_1" {
		t.Errorf("got %+v", a)
	}

	// and it still holds nothing
	if len(decode[Balances](t, do(t, s, http.MethodGet, base+"/accounts/users:future/balances", nil))) != 0 {
		t.Error("tagging an account must not fund it")
	}

	// metadata survives a later posting, which upserts the same row
	fund(t, s, base, "users:future", 500)
	after := decode[Account](t, do(t, s, http.MethodGet, base+"/accounts/users:future", nil))
	if (*after.Metadata)["userId"] != "u_1" {
		t.Errorf("metadata lost when the account was upserted by a commit: %v", after.Metadata)
	}

	deleted := decode[Account](t, do(t, s, http.MethodDelete, base+"/accounts/users:future/metadata/userId", nil))
	if deleted.Metadata != nil {
		if _, ok := (*deleted.Metadata)["userId"]; ok {
			t.Errorf("key survived deletion: %v", deleted.Metadata)
		}
	}
}

func TestMetadataValidationOverHTTP(t *testing.T) {
	s := newTestServer(t)
	base := newLedger(t, s, "demo")
	fund(t, s, base, "users:alice", 100)

	long := strings.Repeat("x", 1025)
	tests := []struct {
		why  string
		body any
	}{
		{"nothing to set", map[string]string{}},
		{"an empty key", map[string]string{"": "v"}},
		{"an oversized value", map[string]string{"k": long}},
		{"a value that is not a string", `{"k":123}`},
	}

	for _, tc := range tests {
		t.Run(tc.why, func(t *testing.T) {
			rec := do(t, s, http.MethodPost, base+"/transactions/1/metadata", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			if got := decode[Error](t, rec).Code; got != VALIDATION {
				t.Errorf("code = %s, want VALIDATION", got)
			}
		})
	}

	bad := do(t, s, http.MethodPost, base+"/accounts/not%20an%20address/metadata", map[string]string{"a": "1"})
	if bad.Code != http.StatusBadRequest {
		t.Errorf("invalid address = %d, want 400", bad.Code)
	}
}

// metadata changes are mutations, so they belong in the chain.
func TestMetadataAppearsInTheLog(t *testing.T) {
	s := newTestServer(t)
	base := newLedger(t, s, "demo")
	fund(t, s, base, "users:alice", 100)

	do(t, s, http.MethodPost, base+"/transactions/1/metadata", map[string]string{"a": "1"})
	do(t, s, http.MethodDelete, base+"/transactions/1/metadata/a", nil)

	page := decode[LogPage](t, do(t, s, http.MethodGet, base+"/logs", nil))
	want := []LogType{NEWTRANSACTION, SETMETADATA, DELETEMETADATA}
	if len(page.Items) != len(want) {
		t.Fatalf("%d entries, want %d", len(page.Items), len(want))
	}
	for i, w := range want {
		if page.Items[i].Type != w {
			t.Errorf("entry %d is %s, want %s", i+1, page.Items[i].Type, w)
		}
	}
	if !strings.Contains(string(page.Items[1].Data), `"targetType":"TRANSACTION"`) {
		t.Errorf("log payload does not identify its target: %s", page.Items[1].Data)
	}
}

func TestRevertEndpoint(t *testing.T) {
	s := newTestServer(t)
	base := newLedger(t, s, "demo")
	fund(t, s, base, "users:alice", 10000)
	if rec := do(t, s, http.MethodPost, base+"/transactions", map[string]any{
		"postings": []map[string]any{posting("users:alice", "users:bob", "USD/2", 3000)},
	}); rec.Code != http.StatusCreated {
		t.Fatal(rec.Body.String())
	}

	// the body is optional
	rec := do(t, s, http.MethodPost, base+"/transactions/2/revert", nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}

	got := decode[Reversal](t, rec)
	if got.Original.RevertedAt == nil {
		t.Error("the original is not marked as reverted")
	}
	if got.Reversal.Id == got.Original.Id {
		t.Error("the reversal must be its own transaction")
	}
	if (*got.Reversal.Metadata)["giro/reverts"] != "2" {
		t.Errorf("reversal metadata = %v, want a giro/reverts tag", got.Reversal.Metadata)
	}
	if got.Reversal.Postings[0].Source != "users:bob" {
		t.Errorf("reversal moves %s -> %s, want the other way round",
			got.Reversal.Postings[0].Source, got.Reversal.Postings[0].Destination)
	}

	balances := decode[Balances](t, do(t, s, http.MethodGet, base+"/accounts/users:alice/balances", nil))
	if balances["USD/2"].Cmp(big.NewInt(10000)) != 0 {
		t.Errorf("alice = %s, want 10000", balances["USD/2"])
	}

	again := do(t, s, http.MethodPost, base+"/transactions/2/revert", nil)
	if again.Code != http.StatusConflict {
		t.Errorf("second revert = %d, want 409", again.Code)
	}
	if code := decode[Error](t, again).Code; code != CONFLICT {
		t.Errorf("code = %s", code)
	}
}

// spending the money first makes the reversal unapplyable, which is 422 and
// not a server fault.
func TestRevertRejectedWhenTheMoneyIsSpent(t *testing.T) {
	s := newTestServer(t)
	base := newLedger(t, s, "demo")
	fund(t, s, base, "users:alice", 10000)
	do(t, s, http.MethodPost, base+"/transactions", map[string]any{
		"postings": []map[string]any{posting("users:alice", "users:bob", "USD/2", 3000)},
	})
	do(t, s, http.MethodPost, base+"/transactions", map[string]any{
		"postings": []map[string]any{posting("users:bob", "users:carol", "USD/2", 3000)},
	})

	rec := do(t, s, http.MethodPost, base+"/transactions/2/revert", nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body.String())
	}
	if code := decode[Error](t, rec).Code; code != INSUFFICIENTFUNDS {
		t.Errorf("code = %s", code)
	}

	// and force gets it through
	forced := do(t, s, http.MethodPost, base+"/transactions/2/revert", map[string]any{"force": true})
	if forced.Code != http.StatusCreated {
		t.Fatalf("forced revert = %d: %s", forced.Code, forced.Body.String())
	}
	balances := decode[Balances](t, do(t, s, http.MethodGet, base+"/accounts/users:bob/balances", nil))
	if balances["USD/2"].Sign() >= 0 {
		t.Errorf("bob = %s, want negative: force is what it says", balances["USD/2"])
	}
}

func TestRevertAtEffectiveDateOverHTTP(t *testing.T) {
	s := newTestServer(t)
	base := newLedger(t, s, "demo")
	fund(t, s, base, "users:alice", 10000)

	created := do(t, s, http.MethodPost, base+"/transactions", map[string]any{
		"postings":  []map[string]any{posting("users:alice", "users:bob", "USD/2", 3000)},
		"timestamp": "2026-03-01T12:00:00Z",
	})
	original := decode[Transaction](t, created)

	rec := do(t, s, http.MethodPost, base+"/transactions/2/revert", map[string]any{"atEffectiveDate": true})
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if got := decode[Reversal](t, rec); !got.Reversal.Timestamp.Equal(original.Timestamp) {
		t.Errorf("reversal dated %v, want the original's %v", got.Reversal.Timestamp, original.Timestamp)
	}
}

func TestBalancesAsOfADate(t *testing.T) {
	s := newTestServer(t)
	base := newLedger(t, s, "demo")

	commit := func(from, to string, amount int64, day int) {
		t.Helper()
		rec := do(t, s, http.MethodPost, base+"/transactions", map[string]any{
			"postings":  []map[string]any{posting(from, to, "USD/2", amount)},
			"timestamp": fmt.Sprintf("2026-03-%02dT12:00:00Z", day),
		})
		if rec.Code != http.StatusCreated {
			t.Fatal(rec.Body.String())
		}
	}

	commit("world", "alice", 100, 1)
	commit("world", "alice", 50, 3)
	commit("alice", "bob", 30, 5)
	commit("world", "alice", 50, 2) // a settlement file arriving late

	for _, tc := range []struct {
		day  int
		want int64
	}{{1, 100}, {2, 150}, {3, 200}, {5, 170}} {
		at := fmt.Sprintf("2026-03-%02dT12:00:00Z", tc.day)
		got := decode[Balances](t, do(t, s, http.MethodGet,
			base+"/accounts/alice/balances?at="+at, nil))
		if got["USD/2"].Cmp(big.NewInt(tc.want)) != 0 {
			t.Errorf("balance on day %d = %s, want %d", tc.day, got["USD/2"], tc.want)
		}
	}

	// with no date it is the current view, which is the same total here but
	// reached by a different route
	now := decode[Balances](t, do(t, s, http.MethodGet, base+"/accounts/alice/balances", nil))
	if now["USD/2"].Cmp(big.NewInt(170)) != 0 {
		t.Errorf("current balance = %s, want 170", now["USD/2"])
	}
}

func TestExpandEffectiveVolumes(t *testing.T) {
	s := newTestServer(t)
	base := newLedger(t, s, "demo")
	fund(t, s, base, "users:alice", 10000)

	a := decode[Account](t, do(t, s, http.MethodGet,
		base+"/accounts/users:alice?expand=volumes,effectiveVolumes", nil))
	if a.Volumes == nil || a.EffectiveVolumes == nil {
		t.Fatalf("volumes %v, effectiveVolumes %v", a.Volumes, a.EffectiveVolumes)
	}
	if (*a.EffectiveVolumes)["USD/2"].Input.Cmp(big.NewInt(10000)) != 0 {
		t.Errorf("effective volumes = %v", *a.EffectiveVolumes)
	}

	// neither is present unless asked for
	plain := decode[Account](t, do(t, s, http.MethodGet, base+"/accounts/users:alice", nil))
	if plain.Volumes != nil || plain.EffectiveVolumes != nil {
		t.Error("volumes returned without being asked for")
	}
}

func TestBadEffectiveDateIsRejected(t *testing.T) {
	s := newTestServer(t)
	base := newLedger(t, s, "demo")
	fund(t, s, base, "users:alice", 100)

	for _, path := range []string{
		base + "/accounts/users:alice/balances?at=yesterday",
		base + "/accounts/users:alice?expand=effectiveVolumes&at=2026-13-45",
	} {
		rec := do(t, s, http.MethodGet, path, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", path, rec.Code)
		}
		if got := decode[Error](t, rec).Code; got != VALIDATION {
			t.Errorf("code = %s", got)
		}
	}
}
