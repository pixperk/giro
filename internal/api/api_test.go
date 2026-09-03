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

func newTestServer(t *testing.T) *Server {
	t.Helper()
	ctx := context.Background()

	schema := fmt.Sprintf("api_%d_%d", os.Getpid(), schemaCounter.Add(1))

	admin, err := pgxpool.New(ctx, testURL())
	if err != nil {
		t.Skipf("no test database: %v", err)
	}
	if _, err := admin.Exec(ctx, "create schema "+schema); err != nil {
		t.Skipf("no test database: %v", err)
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

	return NewServer(func(name string) *storage.Store { return storage.New(pool, name) })
}

// do issues a request and returns the recorder. body may be nil, a string, or
// anything json marshalable.
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
	for i := 0; i+1 < len(headers); i += 2 {
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
func newLedger(t *testing.T, s *Server, name string) string {
	t.Helper()
	base := "/v1/ledgers/" + name
	if rec := do(t, s, http.MethodPost, base, nil); rec.Code != http.StatusCreated {
		t.Fatalf("creating ledger: %d %s", rec.Code, rec.Body.String())
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
