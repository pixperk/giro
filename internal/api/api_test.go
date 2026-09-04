package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	giro "github.com/pixperk/giro"
	"github.com/pixperk/giro/migrate"
	"github.com/pixperk/giro/storage"
)

// tests drive the real server against real postgres rather than a mocked
// store. the handlers are thin, so a mock would mostly assert that the code
// calls the functions it visibly calls, while the parts that actually break
// live in the seam between them: status codes, json shapes and number
// precision.

var schemaCounter atomic.Int64

func testURL() string {
	if u := os.Getenv("GIRO_TEST_DATABASE_URL"); u != "" {
		return u
	}
	return "postgres://" + os.Getenv("USER") + "@localhost:5432/giro_test"
}

func newTestServer(t *testing.T, opts ...func(*Server)) *Server {
	t.Helper()
	ctx := context.Background()

	schema := fmt.Sprintf("api_%d_%d", os.Getpid(), schemaCounter.Add(1))

	admin, err := pgxpool.New(ctx, testURL())
	if err != nil {
		skipNoDatabase(t, err)
	}
	if _, err := admin.Exec(ctx, "create schema "+schema); err != nil {
		skipNoDatabase(t, err)
	}
	admin.Close()

	cfg, err := pgxpool.ParseConfig(testURL())
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.MaxConns = 10

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sub, _ := fs.Sub(giro.MigrationsFS, giro.MigrationsDir)
	if _, err := migrate.Run(ctx, conn.Conn(), sub); err != nil {
		t.Fatal(err)
	}
	conn.Release()

	t.Cleanup(func() {
		pool.Close()
		if cleanup, err := pgxpool.New(ctx, testURL()); err == nil {
			_, _ = cleanup.Exec(ctx, "drop schema "+schema+" cascade")
			cleanup.Close()
		}
	})

	return NewServer(func(name string) *storage.Store { return storage.New(pool, name) }, opts...)
}

// do issues a request and returns the recorder. body may be nil, a string, or
// anything json marshalable.
// nextKey gives every write in the suite a distinct idempotency key, so tests
// stay independent of each other.
var nextKey atomic.Int64

// the two endpoints that move money, and the only ones that require a key
func isWrite(method, path string) bool {
	if method != http.MethodPost {
		return false
	}
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	return strings.HasSuffix(path, "/transactions") || strings.HasSuffix(path, "/transactions/bulk")
}

func do(t *testing.T, s *Server, method, path string, body any, headers ...string) *httptest.ResponseRecorder {
	t.Helper()

	var reader *strings.Reader
	switch b := body.(type) {
	case nil:
		reader = strings.NewReader("")
	case string:
		reader = strings.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		reader = strings.NewReader(string(raw))
	}

	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")

	// A write carries an Idempotency-Key unless the test is about not having
	// one. The tests model the client a caller should write rather than the
	// smallest request the server will accept -- and the server refuses an
	// unkeyed write for a reason worth keeping in front of whoever reads
	// these.
	if isWrite(method, path) {
		req.Header.Set("Idempotency-Key", fmt.Sprintf("test-%d", nextKey.Add(1)))
	}
	for i := 0; i+1 < len(headers); i += 2 {
		if headers[i+1] == "" {
			req.Header.Del(headers[i]) // an explicit empty value clears it
			continue
		}
		req.Header.Set(headers[i], headers[i+1])
	}

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding %s: %v\nbody: %s", rec.Result().Status, err, rec.Body.String())
	}
	return out
}

// creates a ledger and returns its base path.
// the assets the suite posts in, registered over the api rather than inserted
// behind it, so this exercises the endpoint a real caller has to use before it
// can post anything at all.
var testAssets = []string{"USD/2", "EUR/2", "USDT/6", "BTC/8", "TOKEN/18"}

func newLedger(t *testing.T, s *Server, name string) string {
	t.Helper()
	base := "/v1/ledgers/" + name
	if rec := do(t, s, http.MethodPost, base, nil); rec.Code != http.StatusCreated {
		t.Fatalf("creating ledger: %d %s", rec.Code, rec.Body.String())
	}
	for _, asset := range testAssets {
		if rec := do(t, s, http.MethodPost, base+"/assets",
			map[string]any{"asset": asset}); rec.Code != http.StatusCreated {
			t.Fatalf("registering %s: %d %s", asset, rec.Code, rec.Body.String())
		}
	}
	return base
}

func fund(t *testing.T, s *Server, base, account string, amount int64) Transaction {
	t.Helper()
	rec := do(t, s, http.MethodPost, base+"/transactions", map[string]any{
		"postings": []map[string]any{
			{"source": "world", "destination": account, "asset": "USD/2", "amount": amount},
		},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("funding %s: %d %s", account, rec.Code, rec.Body.String())
	}
	return decode[Transaction](t, rec)
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
