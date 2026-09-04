package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pixperk/giro/storage"
)

// a server whose database is gone, so every handler takes its error path.
func brokenServer(t *testing.T) *Server {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), testURL())
	if err != nil {
		skipNoDatabase(t, err)
	}
	pool.Close()
	return NewServer(func(name string) *storage.Store { return storage.New(pool, name) })
}

// an internal error must not describe the database to the caller. the message
// can carry table names, sql and account addresses, and the client can do
// nothing with any of it.
func TestInternalErrorsDoNotLeak(t *testing.T) {
	s := brokenServer(t)

	requests := []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/v1/ledgers/demo", nil},
		{http.MethodGet, "/v1/ledgers/demo/transactions", nil},
		{http.MethodGet, "/v1/ledgers/demo/transactions/1", nil},
		{http.MethodGet, "/v1/ledgers/demo/accounts/users:alice", nil},
		{http.MethodGet, "/v1/ledgers/demo/accounts/users:alice/balances", nil},
		{http.MethodGet, "/v1/ledgers/demo/balances", nil},
		{http.MethodGet, "/v1/ledgers/demo/logs", nil},
		{http.MethodPost, "/v1/ledgers/demo/transactions", map[string]any{
			"postings": []map[string]any{posting("world", "a", "USD/2", 1)},
		}},
	}

	for _, r := range requests {
		t.Run(r.method+" "+r.path, func(t *testing.T) {
			rec := do(t, s, r.method, r.path, r.body)

			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
			}

			e := decode[Error](t, rec)
			if e.Code != INTERNAL {
				t.Errorf("code = %s, want INTERNAL", e.Code)
			}
			if e.Message != "internal error" {
				t.Errorf("message = %q, want a generic one", e.Message)
			}

			// the specific things a leaked driver error would contain
			body := strings.ToLower(rec.Body.String())
			for _, leak := range []string{"pgx", "pool", "select", "insert", "accounts_volumes", "schema"} {
				if strings.Contains(body, leak) {
					t.Errorf("response mentions %q: %s", leak, rec.Body.String())
				}
			}
		})
	}
}

// the status code carries the class of problem, so a client can retry a 5xx
// and must not retry a 4xx.
func TestErrorStatusMapping(t *testing.T) {
	s := newTestServer(t)
	base := newLedger(t, s, "demo")
	fund(t, s, base, "users:alice", 100)

	tests := []struct {
		why      string
		method   string
		path     string
		body     any
		headers  []string
		wantCode int
		wantErr  ErrorCode
	}{
		{
			why:    "insufficient funds is unprocessable, not malformed",
			method: http.MethodPost, path: base + "/transactions",
			body:     map[string]any{"postings": []map[string]any{posting("users:alice", "b", "USD/2", 999)}},
			wantCode: http.StatusUnprocessableEntity, wantErr: INSUFFICIENTFUNDS,
		},
		{
			why:    "a missing transaction",
			method: http.MethodGet, path: base + "/transactions/404",
			wantCode: http.StatusNotFound, wantErr: NOTFOUND,
		},
		{
			why:    "a missing account",
			method: http.MethodGet, path: base + "/accounts/users:ghost",
			wantCode: http.StatusNotFound, wantErr: NOTFOUND,
		},
		{
			why:    "a ledger that does not exist",
			method: http.MethodPost, path: "/v1/ledgers/absent/transactions",
			body:     map[string]any{"postings": []map[string]any{posting("world", "a", "USD/2", 1)}},
			wantCode: http.StatusNotFound, wantErr: NOTFOUND,
		},
		{
			why:    "creating a ledger twice",
			method: http.MethodPost, path: base,
			wantCode: http.StatusConflict, wantErr: CONFLICT,
		},
		{
			why:    "a malformed cursor",
			method: http.MethodGet, path: base + "/logs?cursor=%%%",
			wantCode: http.StatusBadRequest, wantErr: VALIDATION,
		},
	}

	for _, tc := range tests {
		t.Run(tc.why, func(t *testing.T) {
			rec := do(t, s, tc.method, tc.path, tc.body, tc.headers...)
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.wantCode, rec.Body.String())
			}
			if got := decode[Error](t, rec).Code; got != tc.wantErr {
				t.Errorf("code = %s, want %s", got, tc.wantErr)
			}
		})
	}
}

// the key is echoed back on the log entry, so an operator can find the request
// that produced a transaction.
func TestLogCarriesTheIdempotencyKey(t *testing.T) {
	s := newTestServer(t)
	base := newLedger(t, s, "demo")

	for _, key := range []string{"req-abc", "req-def"} {
		if rec := do(t, s, http.MethodPost, base+"/transactions", map[string]any{
			"postings": []map[string]any{posting("world", "users:alice", "USD/2", 100)},
		}, "Idempotency-Key", key); rec.Code != http.StatusCreated {
			t.Fatal(rec.Body.String())
		}
	}

	page := decode[LogPage](t, do(t, s, http.MethodGet, base+"/logs", nil))
	for i, want := range []string{"req-abc", "req-def"} {
		got := page.Items[i].IdempotencyKey
		if got == nil || *got != want {
			t.Errorf("entry %d key = %v, want %s", i, got, want)
		}
	}
}

// A write with no Idempotency-Key is refused, and this is the reason.
//
// A connection lost after the server commits but before the client hears
// leaves the caller unable to tell whether the payment landed. That window
// cannot be closed -- it is a property of networks -- so the only remedy is a
// key the caller can retry under. The fault-injection tests in storage prove
// the remedy works; this makes sure a caller cannot skip it by accident.
func TestAWriteWithoutAnIdempotencyKeyIsRefused(t *testing.T) {
	s := newTestServer(t)
	base := newLedger(t, s, "demo")
	body := map[string]any{
		"postings": []map[string]any{posting("world", "users:alice", "USD/2", 100)},
	}

	for _, path := range []string{base + "/transactions", base + "/transactions/bulk"} {
		t.Run(path, func(t *testing.T) {
			payload := body
			if strings.HasSuffix(path, "/bulk") {
				payload = map[string]any{"transactions": []map[string]any{body}}
			}
			rec := do(t, s, http.MethodPost, path, payload, "Idempotency-Key", "")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			got := decode[Error](t, rec)
			if got.Code != IDEMPOTENCYKEYREQUIRED {
				t.Errorf("code = %s, want %s", got.Code, IDEMPOTENCYKEYREQUIRED)
			}
			// the message has to say what to do, not just what is missing
			if !strings.Contains(got.Message, "Idempotency-Key") {
				t.Errorf("message does not name the header: %s", got.Message)
			}
		})
	}

	// and nothing was written
	page := decode[LogPage](t, do(t, s, http.MethodGet, base+"/logs", nil))
	if len(page.Items) != 0 {
		t.Errorf("%d log entries after refused writes", len(page.Items))
	}
}

// A dry run commits nothing, so there is nothing to duplicate and a lost
// response costs only asking again. Requiring a key there would be friction
// with no safety behind it.
func TestADryRunNeedsNoIdempotencyKey(t *testing.T) {
	s := newTestServer(t)
	base := newLedger(t, s, "demo")

	rec := do(t, s, http.MethodPost, base+"/transactions?dryRun=true", map[string]any{
		"postings": []map[string]any{posting("world", "users:alice", "USD/2", 100)},
	}, "Idempotency-Key", "")
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want a dry run to be accepted: %s", rec.Code, rec.Body.String())
	}
}

// The escape hatch, for a caller that deduplicates upstream. It exists so that
// running without keys is a decision somebody made out loud rather than one
// they arrived at by not setting a header.
func TestUnkeyedWritesCanBeAllowedDeliberately(t *testing.T) {
	s := newTestServer(t, AllowUnkeyedWrites())
	base := newLedger(t, s, "demo")

	rec := do(t, s, http.MethodPost, base+"/transactions", map[string]any{
		"postings": []map[string]any{posting("world", "users:alice", "USD/2", 100)},
	}, "Idempotency-Key", "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
}

// account metadata round trips, and an account without any omits the field
// rather than sending an empty object.
func TestAccountMetadataOmittedWhenAbsent(t *testing.T) {
	s := newTestServer(t)
	base := newLedger(t, s, "demo")
	fund(t, s, base, "users:alice", 100)

	a := decode[Account](t, do(t, s, http.MethodGet, base+"/accounts/users:alice", nil))
	if a.Metadata != nil {
		t.Errorf("metadata = %v, want omitted", a.Metadata)
	}
	if !strings.Contains(do(t, s, http.MethodGet, base+"/accounts/users:alice", nil).Body.String(), `"address"`) {
		t.Error("address missing from the response")
	}
}

// r.URL.Query() drops parameters it cannot parse, which would hand a client
// the first page again instead of an error. for a cursor that means silently
// reprocessing transactions it has already seen.
func TestMalformedQueryStringIsRejected(t *testing.T) {
	s := newTestServer(t)
	base := newLedger(t, s, "demo")
	fund(t, s, base, "users:alice", 100)

	paths := []string{
		base + "/logs?cursor=%%%",
		base + "/transactions?cursor=%%%",
		base + "/accounts/users:alice?expand=%%%",
		base + "/balances?prefix=%%%",
	}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			rec := do(t, s, http.MethodGet, path, nil)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			if got := decode[Error](t, rec).Code; got != VALIDATION {
				t.Errorf("code = %s, want VALIDATION", got)
			}
		})
	}
}
