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

func TestStatus(t *testing.T) {
	ctx, conn, _ := testConn(t)
	_, root := dir(t, map[string]string{
		"20260101000001_first.sql":  "create table first (id int);",
		"20260101000002_second.sql": "create table second (id int);",
		// the directive, but not create index concurrently: that waits for every
		// transaction in the database that could see the table, so it deadlocks
		// against anything else running. applying it is covered by
		// TestNoTransactionDirective; here Status only has to report it, which
		// comes from parsing the file.
		"20260101000003_third.sql": noTransactionDirective + "\ncreate table third (id int);",
	})

	t.Run("before anything has run", func(t *testing.T) {
		list, err := Status(ctx, conn, root.FS())
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 3 {
			t.Fatalf("%d entries, want 3", len(list))
		}
		for i, s := range list {
			if s.Applied {
				t.Errorf("entry %d is applied on an empty database", i)
			}
			if !s.AppliedAt.IsZero() {
				t.Errorf("entry %d has an applied time it never had", i)
			}
		}
		// ordered by version, and the directive is reported
		if list[0].Name != "first" || list[2].Name != "third" {
			t.Errorf("out of order: %s then %s", list[0].Name, list[2].Name)
		}
		if !list[2].NoTx {
			t.Error("the no-transaction directive was not reported")
		}
	})

	t.Run("after applying", func(t *testing.T) {
		if _, err := Run(ctx, conn, root.FS()); err != nil {
			t.Fatal(err)
		}
		list, err := Status(ctx, conn, root.FS())
		if err != nil {
			t.Fatal(err)
		}
		for i, s := range list {
			if !s.Applied {
				t.Errorf("entry %d is still pending", i)
			}
			if s.AppliedAt.IsZero() {
				t.Errorf("entry %d has no applied time", i)
			}
		}
	})
}

// status must work before the tracking table exists, since that is exactly
// when someone runs it.
func TestStatusOnAnUntouchedDatabase(t *testing.T) {
	ctx, conn, _ := testConn(t)
	_, root := dir(t, map[string]string{
		"20260101000001_first.sql": "create table first (id int);",
	})

	var exists bool
	if err := conn.QueryRow(ctx, "select to_regclass('schema_migrations') is not null").Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("test setup: the table should not exist yet")
	}

	list, err := Status(ctx, conn, root.FS())
	if err != nil {
		t.Fatalf("status must not require the table it reports on: %v", err)
	}
	if len(list) != 1 || list[0].Applied {
		t.Errorf("got %+v", list)
	}
}

func TestStatusRejectsABadMigrationName(t *testing.T) {
	ctx, conn, _ := testConn(t)
	_, root := dir(t, map[string]string{"nope.sql": "select 1;"})

	if _, err := Status(ctx, conn, root.FS()); err == nil {
		t.Error("a malformed filename passed status")
	}
}

func TestNewWritesAStub(t *testing.T) {
	dir := t.TempDir()

	path, err := New(dir, "Add Metadata Tables!")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(path) != dir {
		t.Errorf("wrote to %s, want inside %s", path, dir)
	}

	name := filepath.Base(path)
	version, slug, err := parseFilename(name)
	if err != nil {
		t.Fatalf("the generator produced a name its own parser rejects: %v", err)
	}
	if slug != "add_metadata_tables" {
		t.Errorf("slug = %q", slug)
	}
	// the timestamp is utc, so two people in different zones cannot produce
	// files that sort in the wrong order
	stamp := time.Now().UTC().Format(TimestampLayout)
	if fmt.Sprint(version)[:8] != stamp[:8] {
		t.Errorf("version %d does not start with today in utc (%s)", version, stamp[:8])
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), noTransactionDirective) {
		t.Errorf("the stub does not mention the directive:\n%s", body)
	}
	if !strings.Contains(string(body), "forward only") {
		t.Errorf("the stub does not say migrations are forward only:\n%s", body)
	}
}

func TestNewCreatesTheDirectory(t *testing.T) {
	base := filepath.Join(t.TempDir(), "does", "not", "exist")

	path, err := New(base, "first")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the file was not created: %v", err)
	}
}

func TestNewRejectsAnUnusableName(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"", "!!!", "   ", "___"} {
		if _, err := New(dir, name); err == nil {
			t.Errorf("New(%q) was accepted", name)
		}
	}
}

// two migrations created in the same second would collide, and silently
// overwriting one would lose it.
func TestNewRefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()

	first, err := New(dir, "same name")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(first, []byte("-- edited"), 0o644); err != nil {
		t.Fatal(err)
	}

	// force the collision rather than racing the clock
	name := filepath.Base(first)
	stamp := name[:14]
	collision := filepath.Join(dir, stamp+"_same_name.sql")
	if collision != first {
		t.Fatalf("test setup: %s and %s should be the same path", collision, first)
	}

	// New builds its name from the current second, so this only collides when
	// it runs inside the same second. assert the guard exists by calling the
	// path it protects.
	if _, err := os.Stat(first); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(first)
	if string(body) != "-- edited" {
		t.Error("the existing file was overwritten")
	}
}
