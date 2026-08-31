package migrate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func testURL() string {
	if u := os.Getenv("GIRO_TEST_DATABASE_URL"); u != "" {
		return u
	}
	return "postgres://" + os.Getenv("USER") + "@localhost:5432/giro_test"
}

// each test gets its own schema so schema_migrations and any tables a
// migration creates are isolated. advisory locks are per database, so the
// concurrency test still exercises real contention.
func testConn(t *testing.T) (context.Context, *pgx.Conn, string) {
	t.Helper()
	ctx := context.Background()

	conn, err := pgx.Connect(ctx, testURL())
	if err != nil {
		t.Skipf("no test database: %v", err)
	}

	schema := fmt.Sprintf("t_%d", time.Now().UnixNano())
	if _, err := conn.Exec(ctx, "create schema "+schema); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "set search_path to "+schema); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		conn.Exec(ctx, "drop schema "+schema+" cascade")
		conn.Close(ctx)
	})
	return ctx, conn, schema
}

// writes migration files into a temp dir and returns an fs.FS over it.
func dir(t *testing.T, files map[string]string) (string, *os.Root) {
	t.Helper()
	d := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(d, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(d)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return d, root
}

func appliedVersions(t *testing.T, ctx context.Context, conn *pgx.Conn) []int64 {
	t.Helper()
	rows, err := conn.Query(ctx, "select version from schema_migrations order by version")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	return out
}

func TestRunAppliesInVersionOrder(t *testing.T) {
	ctx, conn, _ := testConn(t)
	_, root := dir(t, map[string]string{
		"20260101000002_second.sql": "create table second (id int);",
		"20260101000001_first.sql":  "create table first (id int);",
		"README.md":                 "not a migration",
	})

	if _, err := Run(ctx, conn, root.FS()); err != nil {
		t.Fatal(err)
	}

	got := appliedVersions(t, ctx, conn)
	want := []int64{20260101000001, 20260101000002}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("applied = %v, want %v", got, want)
	}

	for _, table := range []string{"first", "second"} {
		var exists bool
		conn.QueryRow(ctx, "select to_regclass($1) is not null", table).Scan(&exists)
		if !exists {
			t.Errorf("table %s was not created", table)
		}
	}
}

func TestRunIsIdempotent(t *testing.T) {
	ctx, conn, _ := testConn(t)
	// create table without if not exists, so a second apply would fail loudly
	_, root := dir(t, map[string]string{
		"20260101000001_first.sql": "create table first (id int);",
	})

	for i := range 3 {
		if _, err := Run(ctx, conn, root.FS()); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	if got := appliedVersions(t, ctx, conn); len(got) != 1 {
		t.Fatalf("applied %v, want exactly one row", got)
	}
}

func TestChecksumDrift(t *testing.T) {
	ctx, conn, _ := testConn(t)
	d, root := dir(t, map[string]string{
		"20260101000001_first.sql": "create table first (id int);",
	})

	if _, err := Run(ctx, conn, root.FS()); err != nil {
		t.Fatal(err)
	}

	// edit a migration that already ran
	path := filepath.Join(d, "20260101000001_first.sql")
	if err := os.WriteFile(path, []byte("create table first (id bigint);"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Run(ctx, conn, root.FS())
	if !errors.Is(err, ErrChecksumDrift) {
		t.Fatalf("err = %v, want ErrChecksumDrift", err)
	}
}

func TestOutOfOrder(t *testing.T) {
	ctx, conn, _ := testConn(t)
	d, root := dir(t, map[string]string{
		"20260101000005_later.sql": "create table later (id int);",
	})
	if _, err := Run(ctx, conn, root.FS()); err != nil {
		t.Fatal(err)
	}

	// a branch merged late, carrying an older timestamp
	if err := os.WriteFile(filepath.Join(d, "20260101000002_earlier.sql"),
		[]byte("create table earlier (id int);"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Run(ctx, conn, root.FS())
	if !errors.Is(err, ErrOutOfOrder) {
		t.Fatalf("err = %v, want ErrOutOfOrder", err)
	}
}

func TestMissingFile(t *testing.T) {
	ctx, conn, _ := testConn(t)
	d, root := dir(t, map[string]string{
		"20260101000001_first.sql": "create table first (id int);",
	})
	if _, err := Run(ctx, conn, root.FS()); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(filepath.Join(d, "20260101000001_first.sql")); err != nil {
		t.Fatal(err)
	}

	_, err := Run(ctx, conn, root.FS())
	if !errors.Is(err, ErrMissing) {
		t.Fatalf("err = %v, want ErrMissing", err)
	}
}

// create index concurrently cannot run inside a transaction, so this would
// fail with "cannot run inside a transaction block" without the directive.
func TestNoTransactionDirective(t *testing.T) {
	ctx, conn, _ := testConn(t)
	_, root := dir(t, map[string]string{
		"20260101000001_table.sql": "create table widgets (id int);",
		"20260101000002_index.sql": noTransactionDirective + "\ncreate index concurrently widgets_id on widgets (id);",
	})

	if _, err := Run(ctx, conn, root.FS()); err != nil {
		t.Fatalf("no-transaction migration failed: %v", err)
	}
	if got := appliedVersions(t, ctx, conn); len(got) != 2 {
		t.Fatalf("applied %v, want two", got)
	}
}

// dollar quoted bodies contain semicolons. splitting the file on ';' would
// break this, which is why the whole file goes to Exec in one call.
func TestDollarQuotedFunctionBody(t *testing.T) {
	ctx, conn, _ := testConn(t)
	_, root := dir(t, map[string]string{
		"20260101000001_fn.sql": `
create function bump(n int) returns int as $$
begin
	n := n + 1;
	return n;
end;
$$ language plpgsql;`,
	})

	if _, err := Run(ctx, conn, root.FS()); err != nil {
		t.Fatal(err)
	}
	var got int
	if err := conn.QueryRow(ctx, "select bump(41)").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != 42 {
		t.Errorf("bump(41) = %d, want 42", got)
	}
}

// a failed migration must leave no trace, including no schema_migrations row.
func TestFailedMigrationRollsBack(t *testing.T) {
	ctx, conn, _ := testConn(t)
	_, root := dir(t, map[string]string{
		"20260101000001_bad.sql": "create table good (id int); create table oops (id nonsense);",
	})

	if _, err := Run(ctx, conn, root.FS()); err == nil {
		t.Fatal("expected an error")
	}

	var exists bool
	conn.QueryRow(ctx, "select to_regclass('good') is not null").Scan(&exists)
	if exists {
		t.Error("table good survived a failed migration")
	}
	if got := appliedVersions(t, ctx, conn); len(got) != 0 {
		t.Errorf("schema_migrations has %v, want empty", got)
	}
}

// the advisory lock is the whole point: two processes booting at once must not
// both apply the same migration.
func TestConcurrentRunsSerialize(t *testing.T) {
	ctx, conn, schema := testConn(t)
	_, root := dir(t, map[string]string{
		"20260101000001_slow.sql": "select pg_sleep(1); create table slow (id int);",
	})

	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := pgx.Connect(ctx, testURL())
			if err != nil {
				errs[i] = err
				return
			}
			defer c.Close(ctx)
			if _, err := c.Exec(ctx, "set search_path to "+schema); err != nil {
				errs[i] = err
				return
			}
			_, errs[i] = Run(ctx, c, root.FS())
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("runner %d: %v", i, err)
		}
	}
	if got := appliedVersions(t, ctx, conn); len(got) != 1 {
		t.Fatalf("applied %v, want exactly one row", got)
	}
}

func TestParseFilename(t *testing.T) {
	tests := []struct {
		filename    string
		wantVersion int64
		wantName    string
		wantErr     bool
	}{
		{"20260831142530_init_schema.sql", 20260831142530, "init_schema", false},
		{"20260101000000_a.sql", 20260101000000, "a", false},
		{"20260831142530_init.sql", 20260831142530, "init", false},

		{"init_schema.sql", 0, "", true},
		{"2026_init.sql", 0, "", true},
		{"20260831142530_Init.sql", 0, "", true},
		{"20260831142530_init-schema.sql", 0, "", true},
		{"20260831142530_init schema.sql", 0, "", true},
		{"20260831142530_.sql", 0, "", true},
		{"20260831142530_init_schema.txt", 0, "", true},
	}

	for _, tc := range tests {
		t.Run(tc.filename, func(t *testing.T) {
			v, n, err := parseFilename(tc.filename)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr && (v != tc.wantVersion || n != tc.wantName) {
				t.Errorf("= (%d, %q), want (%d, %q)", v, n, tc.wantVersion, tc.wantName)
			}
		})
	}
}

func TestLoadRejectsDuplicateVersions(t *testing.T) {
	_, root := dir(t, map[string]string{
		"20260101000001_a.sql": "select 1;",
		"20260101000001_b.sql": "select 1;",
	})
	if _, err := Load(root.FS()); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("err = %v, want a duplicate version error", err)
	}
}

func TestSlugify(t *testing.T) {
	tests := map[string]string{
		"Add metadata tables": "add_metadata_tables",
		"add-metadata-tables": "add_metadata_tables",
		"  Init  Schema!  ":   "init_schema",
		"already_fine":        "already_fine",
		"MiXeD CaSe 123":      "mixed_case_123",
		"!!!":                 "",
	}
	for in, want := range tests {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

// the directive must be on its own line, not merely present in the file.
func TestNoTransactionDirectiveDetection(t *testing.T) {
	tests := []struct {
		sql  string
		want bool
	}{
		{noTransactionDirective, true},
		{"  " + noTransactionDirective + "  \ncreate table t (id int);", true},
		{"create table t (id int);\n" + noTransactionDirective, true},
		{"-- GIRO:NO-TRANSACTION", true},
		{"create table t (id int);", false},
		{"select '-- giro:no-transaction' as not_a_directive;", false},
		{"-- giro:no-transaction is what you would add here", false},
	}
	for _, tc := range tests {
		if got := hasDirective(tc.sql, noTransactionDirective); got != tc.want {
			t.Errorf("hasDirective(%q) = %v, want %v", tc.sql, got, tc.want)
		}
	}
}
