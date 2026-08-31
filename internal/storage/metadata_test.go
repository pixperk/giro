package storage

import (
	"errors"
	"testing"

	"github.com/pixperk/giro/internal/ledger"
)

func logTypes(t *testing.T, s *Store) []ledger.LogType {
	t.Helper()
	page, err := s.ListLogs(t.Context(), ListLogsQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	var out []ledger.LogType
	for _, l := range page.Items {
		out = append(out, l.Type)
	}
	return out
}

func TestSetTransactionMetadataMerges(t *testing.T) {
	ctx, s, _ := testStore(t)
	tx := mustCommit(t, ctx, s, "world", "users:alice", 100)

	if _, err := s.SetTransactionMetadata(ctx, tx.ID, ledger.Metadata{"orderId": "ord_1"}); err != nil {
		t.Fatal(err)
	}
	// a second write must merge rather than replace
	got, err := s.SetTransactionMetadata(ctx, tx.ID, ledger.Metadata{"provider": "stripe"})
	if err != nil {
		t.Fatal(err)
	}

	if got.Metadata["orderId"] != "ord_1" || got.Metadata["provider"] != "stripe" {
		t.Errorf("metadata = %v, want both keys", got.Metadata)
	}

	// and overwriting one key leaves the other
	got, err = s.SetTransactionMetadata(ctx, tx.ID, ledger.Metadata{"provider": "adyen"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata["provider"] != "adyen" || got.Metadata["orderId"] != "ord_1" {
		t.Errorf("metadata = %v", got.Metadata)
	}

	if types := logTypes(t, s); len(types) != 4 {
		t.Errorf("%d log entries, want 4: one commit and three changes", len(types))
	}
}

// a client retrying an identical write is normal. recording it would fill the
// chain with entries describing nothing happening.
func TestIdenticalMetadataWritesNothing(t *testing.T) {
	ctx, s, _ := testStore(t)
	tx := mustCommit(t, ctx, s, "world", "users:alice", 100)

	m := ledger.Metadata{"orderId": "ord_1"}
	if _, err := s.SetTransactionMetadata(ctx, tx.ID, m); err != nil {
		t.Fatal(err)
	}
	before := len(logTypes(t, s))

	for range 3 {
		if _, err := s.SetTransactionMetadata(ctx, tx.ID, m); err != nil {
			t.Fatal(err)
		}
	}

	if after := len(logTypes(t, s)); after != before {
		t.Errorf("%d log entries after three identical writes, want %d", after, before)
	}
	if _, err := s.VerifyLog(ctx); err != nil {
		t.Errorf("chain broken: %v", err)
	}
}

func TestDeleteTransactionMetadataKey(t *testing.T) {
	ctx, s, _ := testStore(t)
	tx := mustCommit(t, ctx, s, "world", "users:alice", 100)

	if _, err := s.SetTransactionMetadata(ctx, tx.ID, ledger.Metadata{"a": "1", "b": "2"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.DeleteTransactionMetadataKey(ctx, tx.ID, "a")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Metadata["a"]; ok {
		t.Errorf("key a survived: %v", got.Metadata)
	}
	if got.Metadata["b"] != "2" {
		t.Errorf("key b was lost: %v", got.Metadata)
	}

	before := len(logTypes(t, s))
	// removing a key that is not there changes nothing
	if _, err := s.DeleteTransactionMetadataKey(ctx, tx.ID, "never-existed"); err != nil {
		t.Fatalf("deleting an absent key should not be an error: %v", err)
	}
	if after := len(logTypes(t, s)); after != before {
		t.Errorf("a no-op delete wrote a log entry")
	}
}

// accounts are never registered, so tagging one before any money moves is
// exactly when a caller would want to.
func TestSetAccountMetadataCreatesTheAccount(t *testing.T) {
	ctx, s, _ := testStore(t)
	if _, err := s.CreateLedger(ctx); err == nil {
		t.Fatal("the test store already creates the ledger")
	}

	a, err := s.SetAccountMetadata(ctx, "users:future", ledger.Metadata{"userId": "u_1"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Address != "users:future" || a.Metadata["userId"] != "u_1" {
		t.Errorf("got %+v", a)
	}

	// and it has no balance, because tagging is not funding
	balances, err := s.GetBalances(ctx, "users:future")
	if err != nil {
		t.Fatal(err)
	}
	if len(balances) != 0 {
		t.Errorf("balances = %v, want none", balances)
	}
}

func TestAccountMetadataSurvivesLaterPostings(t *testing.T) {
	ctx, s, _ := testStore(t)

	if _, err := s.SetAccountMetadata(ctx, "users:alice", ledger.Metadata{"userId": "u_1"}); err != nil {
		t.Fatal(err)
	}
	mustCommit(t, ctx, s, "world", "users:alice", 100)

	a, err := s.GetAccount(ctx, "users:alice")
	if err != nil {
		t.Fatal(err)
	}
	if a.Metadata["userId"] != "u_1" {
		t.Errorf("metadata was lost when the account was upserted by a commit: %v", a.Metadata)
	}
}

func TestDeleteAccountMetadataKey(t *testing.T) {
	ctx, s, _ := testStore(t)
	if _, err := s.SetAccountMetadata(ctx, "users:alice", ledger.Metadata{"a": "1", "b": "2"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.DeleteAccountMetadataKey(ctx, "users:alice", "a")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Metadata["a"]; ok || got.Metadata["b"] != "2" {
		t.Errorf("metadata = %v", got.Metadata)
	}
}

func TestMetadataRejections(t *testing.T) {
	ctx, s, _ := testStore(t)
	tx := mustCommit(t, ctx, s, "world", "users:alice", 100)

	long := make([]byte, ledger.MaxMetadataValueLength+1)
	for i := range long {
		long[i] = 'x'
	}
	tooManyKeys := ledger.Metadata{}
	for i := range ledger.MaxMetadataKeys + 1 {
		tooManyKeys[string(rune('a'+i%26))+string(rune('0'+i/26))] = "v"
	}

	tests := []struct {
		why  string
		m    ledger.Metadata
		want error
	}{
		{"nothing to set", ledger.Metadata{}, ErrEmptyMetadata},
		{"an empty key", ledger.Metadata{"": "v"}, ledger.ErrEmptyMetadataKey},
		{"an oversized value", ledger.Metadata{"k": string(long)}, ledger.ErrMetadataValueTooLong},
		{"an oversized key", ledger.Metadata{string(long[:ledger.MaxMetadataKeyLength+1]): "v"}, ledger.ErrMetadataKeyTooLong},
		{"too many keys", tooManyKeys, ledger.ErrTooManyMetadataKeys},
	}

	for _, tc := range tests {
		t.Run(tc.why, func(t *testing.T) {
			if _, err := s.SetTransactionMetadata(ctx, tx.ID, tc.m); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestMetadataOnAMissingTarget(t *testing.T) {
	ctx, s, _ := testStore(t)

	if _, err := s.SetTransactionMetadata(ctx, 99, ledger.Metadata{"a": "1"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if _, err := s.DeleteTransactionMetadataKey(ctx, 99, "a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	// but an account is created rather than rejected
	if _, err := s.SetAccountMetadata(ctx, "users:new", ledger.Metadata{"a": "1"}); err != nil {
		t.Errorf("err = %v, want the account to be created", err)
	}
}

// the whole point of routing metadata through the log.
func TestMetadataChangesAreLoggedAndChained(t *testing.T) {
	ctx, s, _ := testStore(t)
	tx := mustCommit(t, ctx, s, "world", "users:alice", 100)

	if _, err := s.SetTransactionMetadata(ctx, tx.ID, ledger.Metadata{"a": "1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteTransactionMetadataKey(ctx, tx.ID, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetAccountMetadata(ctx, "users:alice", ledger.Metadata{"userId": "u_1"}); err != nil {
		t.Fatal(err)
	}

	want := []ledger.LogType{
		ledger.LogNewTransaction,
		ledger.LogSetMetadata,
		ledger.LogDeleteMetadata,
		ledger.LogSetMetadata,
	}
	got := logTypes(t, s)
	if len(got) != len(want) {
		t.Fatalf("log types = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("log %d is %s, want %s", i+1, got[i], want[i])
		}
	}

	checked, err := s.VerifyLog(ctx)
	if err != nil {
		t.Fatalf("chain broken: %v", err)
	}
	if checked != 4 {
		t.Errorf("verified %d entries, want 4", checked)
	}
}

func TestMetadataIsScopedToItsLedger(t *testing.T) {
	ctx, mine, theirs, _ := twoLedgers(t)

	if _, err := theirs.SetAccountMetadata(ctx, "users:alice", ledger.Metadata{"owner": "theirs"}); err != nil {
		t.Fatal(err)
	}

	a, err := mine.GetAccount(ctx, "users:alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Metadata["owner"]; ok {
		t.Errorf("metadata leaked across ledgers: %v", a.Metadata)
	}

	// and setting metadata on a transaction id that exists in both
	if _, err := mine.SetTransactionMetadata(ctx, 1, ledger.Metadata{"owner": "mine"}); err != nil {
		t.Fatal(err)
	}
	other, err := theirs.GetTransaction(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := other.Metadata["owner"]; ok {
		t.Errorf("transaction metadata leaked: %v", other.Metadata)
	}
}
