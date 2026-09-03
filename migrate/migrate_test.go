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

	path, err := Create(dir, "Add Metadata Tables!")
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

	path, err := Create(base, "first")
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
		if _, err := Create(dir, name); err == nil {
			t.Errorf("Create(%q) was accepted", name)
		}
	}
}

// Run returns Load's error rather than proceeding with a partial set. a
// migration the runner cannot name is one it cannot order, and applying the
// rest would leave a gap nothing records.
func TestRunRejectsABadMigrationName(t *testing.T) {
	ctx, conn, _ := testConn(t)
	_, root := dir(t, map[string]string{
		"20260101000001_first.sql": "create table first (id int);",
		"nope.sql":                 "create table nope (id int);",
	})

	if _, err := Run(ctx, conn, root.FS()); err == nil {
		t.Fatal("a malformed filename was accepted")
	}

	var exists bool
	if err := conn.QueryRow(ctx, "select to_regclass('first') is not null").Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Error("a migration ran despite the set failing to load")
	}
}

// every step before the first migration can fail on a dead connection, and
// each must surface rather than being swallowed.
func TestRunOnAClosedConnection(t *testing.T) {
	ctx, conn, _ := testConn(t)
	_, root := dir(t, map[string]string{
		"20260101000001_first.sql": "create table first (id int);",
	})

	if err := conn.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(ctx, conn, root.FS()); err == nil {
		t.Fatal("Run succeeded on a closed connection")
	}
	if _, err := Status(ctx, conn, root.FS()); err == nil {
		t.Fatal("Status succeeded on a closed connection")
	}
}

// something else already owns the name. create table if not exists finds it and
// leaves it alone, so the failure surfaces on the read that follows.
func TestRunWithAConflictingTable(t *testing.T) {
	ctx, conn, _ := testConn(t)
	_, root := dir(t, map[string]string{
		"20260101000001_first.sql": "create table first (id int);",
	})

	if _, err := conn.Exec(ctx, `create table schema_migrations (something_else int)`); err != nil {
		t.Fatal(err)
	}

	_, err := Run(ctx, conn, root.FS())
	if err == nil {
		t.Fatal("Run proceeded against a table it does not own")
	}
	if !strings.Contains(err.Error(), "schema_migrations") {
		t.Errorf("err = %v, want it to name the table", err)
	}
}

// a no-transaction migration has nothing to roll back, so the record must
// simply be absent rather than undone.
func TestNoTransactionMigrationFailureIsReported(t *testing.T) {
	ctx, conn, _ := testConn(t)
	_, root := dir(t, map[string]string{
		"20260101000001_bad.sql": noTransactionDirective + "\ncreate table oops (id nonsense);",
	})

	if _, err := Run(ctx, conn, root.FS()); err == nil {
		t.Fatal("a broken no-transaction migration was accepted")
	}
	var count int
	if err := conn.QueryRow(ctx, "select count(*) from schema_migrations").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("%d migrations recorded, want 0", count)
	}
}

// the count Run returns is what the cli prints.
func TestRunReportsTheCount(t *testing.T) {
	ctx, conn, _ := testConn(t)
	d, root := dir(t, map[string]string{
		"20260101000001_first.sql":  "create table first (id int);",
		"20260101000002_second.sql": "create table second (id int);",
	})

	n, err := Run(ctx, conn, root.FS())
	if err != nil || n != 2 {
		t.Fatalf("applied %d (err %v), want 2", n, err)
	}
	if n, err = Run(ctx, conn, root.FS()); err != nil || n != 0 {
		t.Errorf("second run applied %d (err %v), want 0", n, err)
	}

	if err := os.WriteFile(filepath.Join(d, "20260101000003_third.sql"),
		[]byte("create table third (id int);"), 0o644); err != nil {
		t.Fatal(err)
	}
	if n, err = Run(ctx, conn, root.FS()); err != nil || n != 1 {
		t.Errorf("third run applied %d (err %v), want 1", n, err)
	}
}

// pg_advisory_unlock returns false when the session does not hold the lock, and
// raises no error. that is how a lock ends up held until the connection happens
// to close, with the next deploy blocking on something nobody knows about.
func TestReleaseAdvisoryLock(t *testing.T) {
	ctx, conn, _ := testConn(t)

	t.Run("when it was never held", func(t *testing.T) {
		err := releaseAdvisoryLock(ctx, conn)
		if err == nil {
			t.Fatal("releasing a lock we never took reported success")
		}
		if !strings.Contains(err.Error(), "not held") {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("when it is held", func(t *testing.T) {
		if _, err := conn.Exec(ctx,
			"select pg_advisory_lock($1, $2)", lockKeyApp, lockKeyMigrations); err != nil {
			t.Fatal(err)
		}
		if err := releaseAdvisoryLock(ctx, conn); err != nil {
			t.Errorf("err = %v, want nil", err)
		}
		var held int
		if err := conn.QueryRow(ctx,
			"select count(*) from pg_locks where locktype = 'advisory' and objid = $1",
			lockKeyMigrations).Scan(&held); err != nil {
			t.Fatal(err)
		}
		if held != 0 {
			t.Errorf("%d advisory locks still held", held)
		}
	})

	t.Run("on a dead connection", func(t *testing.T) {
		dead, err := pgx.Connect(ctx, testURL())
		if err != nil {
			t.Skip(err)
		}
		if err := dead.Close(ctx); err != nil {
			t.Fatal(err)
		}
		if err := releaseAdvisoryLock(ctx, dead); err == nil {
			t.Error("releasing on a closed connection reported success")
		}
	})
}

// two migrations created within the same second would collide, and silently
// overwriting one would lose it. the clock is injected so this does not depend
// on how fast the test runs.
func TestNewRefusesToOverwriteAtTheSameSecond(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	first, err := newAt(dir, "add tables", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(first, []byte("-- real work"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := newAt(dir, "add tables", now); err == nil {
		t.Fatal("a second migration in the same second overwrote the first")
	}
	body, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "-- real work" {
		t.Errorf("the existing file was overwritten: %q", body)
	}

	// a different name in the same second is fine, and so is the same name a
	// second later
	if _, err := newAt(dir, "other tables", now); err != nil {
		t.Errorf("a different name collided: %v", err)
	}
	if _, err := newAt(dir, "add tables", now.Add(time.Second)); err != nil {
		t.Errorf("the next second collided: %v", err)
	}
}

// whatever the local zone, the filename is utc, so files from two developers
// sort in the order they were written.
func TestNewNamesInUTC(t *testing.T) {
	dir := t.TempDir()
	kolkata := time.FixedZone("IST", 5*3600+1800)
	local := time.Date(2026, 3, 1, 2, 30, 0, 0, kolkata) // 2026-02-28 21:00 UTC

	path, err := newAt(dir, "first", local)
	if err != nil {
		t.Fatal(err)
	}
	if got := filepath.Base(path)[:14]; got != "20260228210000" {
		t.Errorf("named %s, want the utc instant 20260228210000", got)
	}
}

// create index concurrently waits for every transaction that was already
// running when it started, and a runner blocked on the migration lock is one
// of them: it holds a virtual transaction id while it waits. so the holder
// waits for the waiter and the waiter waits for the holder, and postgres
// resolves the cycle by killing the waiter.
//
// that is a boot failure in the one situation the lock exists for, two
// instances starting at once, and it only appears when a migration uses the
// no-transaction directive. acquiring by polling rather than blocking removes
// the cycle: between attempts the waiter holds nothing at all.
func TestConcurrentIndexDoesNotDeadlockAWaitingRunner(t *testing.T) {
	ctx, conn, schema := testConn(t)
	_, root := dir(t, map[string]string{
		"20260101000001_table.sql": "create table widgets (id int);",
		// its own file: multiple statements in one exec run inside an implicit
		// transaction block, which create index concurrently refuses.
		"20260101000002_slow.sql": "select pg_sleep(1);",
		"20260101000003_index.sql": noTransactionDirective +
			"\ncreate index concurrently widgets_id on widgets (id);",
	})

	held := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		c, err := pgx.Connect(ctx, testURL())
		if err != nil {
			done <- err
			return
		}
		defer c.Close(ctx)
		if _, err := c.Exec(ctx, "set search_path to "+schema); err != nil {
			done <- err
			return
		}
		close(held)
		_, err = Run(ctx, c, root.FS())
		done <- err
	}()

	// let the first runner take the lock and reach pg_sleep, so this one is
	// already waiting when create index concurrently begins.
	<-held
	time.Sleep(300 * time.Millisecond)

	if _, err := Run(ctx, conn, root.FS()); err != nil {
		t.Errorf("waiting runner: %v", err)
	}
	if err := <-done; err != nil {
		t.Errorf("holding runner: %v", err)
	}
}

// a migration that never finishes must fail the boot rather than hang it, so
// the wait has an end and says what it was waiting for.
func TestLockWaitTimesOut(t *testing.T) {
	ctx, conn, _ := testConn(t)
	_, root := dir(t, map[string]string{
		"20260101000001_first.sql": "create table first (id int);",
	})

	holder, err := pgx.Connect(ctx, testURL())
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close(ctx)
	if _, err := holder.Exec(ctx,
		"select pg_advisory_lock($1, $2)", lockKeyApp, lockKeyMigrations); err != nil {
		t.Fatal(err)
	}
	defer holder.Exec(ctx, "select pg_advisory_unlock($1, $2)", lockKeyApp, lockKeyMigrations)

	defer func(d time.Duration) { lockWait = d }(lockWait)
	lockWait = 250 * time.Millisecond

	start := time.Now()
	_, err = Run(ctx, conn, root.FS())
	if err == nil {
		t.Fatal("acquired a lock another session holds")
	}
	if !strings.Contains(err.Error(), "another migration") {
		t.Errorf("err = %v, want it to name what it waited for", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("waited %s, want it bounded by lockWait", elapsed)
	}
}

// a binary that boots against a schema it does not match will fail on the
// first write instead, with a raw sql error, inside a money path. the four
// states it has to tell apart are worth separating because only two of them
// are faults.
func TestRequireUpToDate(t *testing.T) {
	files := map[string]string{
		"20260101000001_first.sql":  "create table first (id int);",
		"20260101000002_second.sql": "create table second (id int);",
	}

	t.Run("nothing has ever run", func(t *testing.T) {
		ctx, conn, _ := testConn(t)
		_, root := dir(t, files)

		err := RequireUpToDate(ctx, conn, root.FS())
		if !errors.Is(err, ErrPending) {
			t.Fatalf("err = %v, want ErrPending", err)
		}
		if !strings.Contains(err.Error(), "giro migrate up") {
			t.Errorf("err = %q, want it to say what to do", err)
		}
	})

	t.Run("up to date", func(t *testing.T) {
		ctx, conn, _ := testConn(t)
		_, root := dir(t, files)
		if _, err := Run(ctx, conn, root.FS()); err != nil {
			t.Fatal(err)
		}

		if err := RequireUpToDate(ctx, conn, root.FS()); err != nil {
			t.Fatalf("a matching schema was refused: %v", err)
		}
	})

	t.Run("schema behind the binary", func(t *testing.T) {
		ctx, conn, _ := testConn(t)
		// applied with only the first migration on disk
		_, first := dir(t, map[string]string{
			"20260101000001_first.sql": files["20260101000001_first.sql"],
		})
		if _, err := Run(ctx, conn, first.FS()); err != nil {
			t.Fatal(err)
		}

		// the binary carries both
		_, both := dir(t, files)
		err := RequireUpToDate(ctx, conn, both.FS())
		if !errors.Is(err, ErrPending) {
			t.Fatalf("err = %v, want ErrPending", err)
		}
		if !strings.Contains(err.Error(), "20260101000002_second.sql") {
			t.Errorf("err = %q, want it to name the pending migration", err)
		}
	})

	// the state a rolling deploy passes through: migrations run first, so an
	// instance still on the old build sees versions it does not carry. that is
	// the deploy working, and refusing to start would mean an old instance
	// could not be restarted during one.
	t.Run("schema ahead of the binary", func(t *testing.T) {
		ctx, conn, _ := testConn(t)
		_, both := dir(t, files)
		if _, err := Run(ctx, conn, both.FS()); err != nil {
			t.Fatal(err)
		}

		_, first := dir(t, map[string]string{
			"20260101000001_first.sql": files["20260101000001_first.sql"],
		})
		err := RequireUpToDate(ctx, conn, first.FS())
		if !errors.Is(err, ErrSchemaAhead) {
			t.Fatalf("err = %v, want ErrSchemaAhead", err)
		}
		// distinguishable from a fault, which is the whole reason it is its
		// own error rather than ErrMissing
		if errors.Is(err, ErrPending) || errors.Is(err, ErrChecksumDrift) {
			t.Error("being ahead reads as a fatal condition")
		}
	})

	t.Run("drift wins over anything else", func(t *testing.T) {
		ctx, conn, _ := testConn(t)
		d, root := dir(t, files)
		if _, err := Run(ctx, conn, root.FS()); err != nil {
			t.Fatal(err)
		}

		// edit an applied migration, and add an unapplied one, so both
		// conditions hold at once
		if err := os.WriteFile(filepath.Join(d, "20260101000001_first.sql"),
			[]byte("create table first (id bigint);"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "20260101000003_third.sql"),
			[]byte("create table third (id int);"), 0o644); err != nil {
			t.Fatal(err)
		}

		// drift is reported, not the pending count: once the schema and the
		// code disagree about what ran, how many are outstanding tells you
		// nothing useful.
		if err := RequireUpToDate(ctx, conn, root.FS()); !errors.Is(err, ErrChecksumDrift) {
			t.Fatalf("err = %v, want ErrChecksumDrift", err)
		}
	})

	// it must be safe to call from a process that serves traffic, which should
	// not be able to change the schema at all.
	t.Run("applies nothing", func(t *testing.T) {
		ctx, conn, _ := testConn(t)
		_, root := dir(t, files)

		for range 2 {
			if err := RequireUpToDate(ctx, conn, root.FS()); !errors.Is(err, ErrPending) {
				t.Fatalf("err = %v, want ErrPending", err)
			}
		}
		var exists bool
		conn.QueryRow(ctx, "select to_regclass('first') is not null").Scan(&exists)
		if exists {
			t.Error("the check created a table")
		}
	})
}
