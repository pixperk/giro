package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// the cli is thin, but it is the layer where a real bug already hid: the up
// command once counted pending migrations and returned early, skipping the
// checks for drift, missing files and out of order versions that live inside
// Run. so these tests are mostly about the wiring, not the logic underneath.

func testURL(t *testing.T) string {
	t.Helper()
	if u := os.Getenv("GIRO_TEST_DATABASE_URL"); u != "" {
		return u
	}
	return "postgres://" + os.Getenv("USER") + "@localhost:5432/giro_test"
}

var schemaCounter atomic.Int64

// points DATABASE_URL at a schema of its own, so commands that touch the
// database do not see each other's tables.
func isolatedDatabase(t *testing.T) string {
	t.Helper()
	ctx := context.Background()

	conn, err := pgx.Connect(ctx, testURL(t))
	if err != nil {
		t.Skipf("no test database: %v", err)
	}
	defer conn.Close(ctx)

	schema := fmt.Sprintf("cli_%d_%d", os.Getpid(), schemaCounter.Add(1))
	if _, err := conn.Exec(ctx, "create schema "+schema); err != nil {
		t.Skipf("no test database: %v", err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), testURL(t))
		if err == nil {
			_, _ = c.Exec(context.Background(), "drop schema "+schema+" cascade")
			c.Close(context.Background())
		}
	})

	url := testURL(t) + "?search_path=" + schema
	t.Setenv("DATABASE_URL", url)
	return url
}

// commands print to stdout, so tests read it back rather than the code being
// restructured around an io.Writer it would otherwise not need.
func captureOutput(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	original := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	done := make(chan string, 1)
	go func() {
		out, _ := io.ReadAll(r)
		done <- string(out)
	}()

	runErr := fn()

	w.Close()
	os.Stdout = original
	return <-done, runErr
}

func TestDispatch(t *testing.T) {
	tests := []struct {
		why       string
		args      []string
		wantUsage bool
		wantErr   bool
	}{
		{why: "no arguments", args: nil, wantUsage: true, wantErr: true},
		{why: "an unknown command", args: []string{"frobnicate"}, wantUsage: true, wantErr: true},
		{why: "help", args: []string{"help"}},
		{why: "-h", args: []string{"-h"}},
		{why: "--help", args: []string{"--help"}},
		{why: "migrate with no subcommand", args: []string{"migrate"}, wantUsage: true, wantErr: true},
		{why: "an unknown migrate subcommand", args: []string{"migrate", "down"}, wantUsage: true, wantErr: true},
		{why: "new with no name", args: []string{"migrate", "new"}, wantUsage: true, wantErr: true},
		{why: "serve with too many arguments", args: []string{"serve", ":1", ":2"}, wantUsage: true, wantErr: true},
		{why: "serve --help", args: []string{"serve", "--help"}, wantUsage: true, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.why, func(t *testing.T) {
			out, err := captureOutput(t, func() error {
				return dispatch(context.Background(), tc.args)
			})

			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr %v (stdout %q)", err, tc.wantErr, out)
			}

			var u usageErr
			if got := errors.As(err, &u); got != tc.wantUsage {
				t.Errorf("usage error = %v, want %v (err %v)", got, tc.wantUsage, err)
			}
			// a usage error must actually say something useful
			if tc.wantUsage && u.text == "" {
				t.Error("the usage error carries no text")
			}
			if !tc.wantErr && !strings.Contains(out, "giro") {
				t.Errorf("help printed %q", out)
			}
		})
	}
}

// wrong invocation and a failed job are different answers, and scripts branch
// on the difference.
func TestUsageErrorsAreDistinguishable(t *testing.T) {
	_, err := captureOutput(t, func() error {
		return dispatch(context.Background(), []string{"nope"})
	})
	var u usageErr
	if !errors.As(err, &u) {
		t.Fatalf("err = %v, want usageErr so main can exit 2", err)
	}

	t.Setenv("DATABASE_URL", "")
	_, err = captureOutput(t, func() error {
		return dispatch(context.Background(), []string{"migrate", "status"})
	})
	if err == nil {
		t.Fatal("expected an error with no DATABASE_URL")
	}
	if errors.As(err, &u) {
		t.Error("a missing DATABASE_URL is a failure, not a usage error: it should exit 1")
	}
	if !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Errorf("err = %v, want it to name the variable", err)
	}
}

func TestMigrateNewWritesAFile(t *testing.T) {
	dir := t.TempDir()
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	defer func() { _ = os.Chdir(original) }()

	out, err := captureOutput(t, func() error {
		return dispatch(context.Background(), []string{"migrate", "new", "Add Some Tables!"})
	})
	if err != nil {
		t.Fatal(err)
	}

	matches, _ := filepath.Glob(filepath.Join(dir, "migrations", "*_add_some_tables.sql"))
	if len(matches) != 1 {
		t.Fatalf("wrote %v, want one slugified file", matches)
	}
	if !strings.Contains(out, "created") {
		t.Errorf("printed %q", out)
	}

	body, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	// the stub tells the next person how to opt out of the transaction
	if !strings.Contains(string(body), "giro:no-transaction") {
		t.Errorf("the stub does not mention the directive:\n%s", body)
	}
}

func TestMigrateUpAndStatus(t *testing.T) {
	isolatedDatabase(t)
	ctx := context.Background()

	out, err := captureOutput(t, func() error { return dispatch(ctx, []string{"migrate", "up"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "applied") {
		t.Errorf("first up printed %q", out)
	}

	out, err = captureOutput(t, func() error { return dispatch(ctx, []string{"migrate", "status"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "applied") || strings.Contains(out, "pending") {
		t.Errorf("status printed %q, want everything applied", out)
	}

	// the second run must be a no-op, and must still have gone through the
	// consistency checks rather than returning early on a pending count
	out, err = captureOutput(t, func() error { return dispatch(ctx, []string{"migrate", "up"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "nothing to do") {
		t.Errorf("second up printed %q", out)
	}
}

func TestServeStartsAndShutsDown(t *testing.T) {
	isolatedDatabase(t)
	ctx := context.Background()
	if err := dispatch(ctx, []string{"migrate", "up"}); err != nil {
		t.Fatal(err)
	}

	addr := freePort(t)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	errs := make(chan error, 1)
	go func() {
		_, err := captureOutput(t, func() error {
			return dispatch(runCtx, []string{"serve", addr})
		})
		errs <- err
	}()

	// the server is up when it answers
	client := &http.Client{Timeout: time.Second}
	var resp *http.Response
	var err error
	for range 50 {
		resp, err = client.Get("http://localhost" + addr + "/healthz")
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("server never answered: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// and cancelling the context stops it cleanly rather than returning an error
	cancel()
	select {
	case err := <-errs:
		if err != nil {
			t.Errorf("shutdown returned %v, want nil", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the server did not stop when its context was cancelled")
	}
}

func TestServeRejectsABadDatabaseURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "not://a valid url")
	err := dispatch(context.Background(), []string{"serve", freePort(t)})
	if err == nil {
		t.Fatal("expected an error")
	}
	var u usageErr
	if errors.As(err, &u) {
		t.Error("a bad url is a failure, not a usage error")
	}
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return fmt.Sprintf(":%d", l.Addr().(*net.TCPAddr).Port)
}

// The bug this file exists for.
//
// `up` once called Status, counted pending migrations, and returned early when
// there were none. That skipped Run entirely, and with it the checks for
// checksum drift, missing files and out of order versions. Every unit test
// passed, because they all called Run directly.
//
// So: apply a migration, corrupt its recorded checksum exactly as editing the
// file would, and require that `up` still fails. A short circuit would report
// "nothing to do" and be quietly wrong.
func TestMigrateUpVerifiesEvenWhenNothingIsPending(t *testing.T) {
	url := isolatedDatabase(t)
	ctx := context.Background()

	if _, err := captureOutput(t, func() error { return dispatch(ctx, []string{"migrate", "up"}) }); err != nil {
		t.Fatal(err)
	}

	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx,
		`update schema_migrations set checksum = 'not-the-real-checksum'`); err != nil {
		t.Fatal(err)
	}

	out, err := captureOutput(t, func() error { return dispatch(ctx, []string{"migrate", "up"}) })
	if err == nil {
		t.Fatalf("up reported %q instead of failing on drift", strings.TrimSpace(out))
	}
	if !strings.Contains(err.Error(), "drift") {
		t.Errorf("err = %v, want it to name the drift", err)
	}
}

// status must show the world as it is, not as it was hoped to be.
func TestMigrateStatusShowsPendingBeforeApplying(t *testing.T) {
	isolatedDatabase(t)
	ctx := context.Background()

	out, err := captureOutput(t, func() error { return dispatch(ctx, []string{"migrate", "status"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "pending") {
		t.Errorf("status on an empty database printed %q, want everything pending", out)
	}
}

// same shape as captureOutput, for the boot warnings, which go to stderr so
// they are not mistaken for the output of a command.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	original := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w

	done := make(chan string, 1)
	go func() {
		out, _ := io.ReadAll(r)
		done <- string(out)
	}()

	fn()

	w.Close()
	os.Stderr = original
	return <-done
}

// The database enforces the invariants with triggers, and a table's owner can
// switch its own triggers off. So serving as the owner makes every guard
// advisory, and the one place anyone will notice is at boot.
//
// This is the check that says so. It is a warning rather than a refusal
// because a local database and a first run legitimately connect as the owner,
// and refusing would make the safe configuration the awkward one.
func TestServeWarnsWhenItCanDisableItsOwnGuards(t *testing.T) {
	ctx := context.Background()
	url := isolatedDatabase(t)

	if _, err := captureOutput(t, func() error { return dispatch(ctx, []string{"migrate", "up"}) }); err != nil {
		t.Fatal(err)
	}

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	// as the owner, which is what DATABASE_URL points at in the test harness
	warning := captureStderr(t, func() { warnIfPrivileged(ctx, pool) })
	if !strings.Contains(warning, "guards can be disabled") {
		t.Errorf("owner got no warning, output was %q", warning)
	}
	if !strings.Contains(warning, "giro_app") {
		t.Errorf("the warning does not say what to do instead: %q", warning)
	}

	// and as a role that cannot, which is what a deployment should look like
	restricted, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	restricted.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "set role giro_app")
		return err
	}
	quiet, err := pgxpool.NewWithConfig(ctx, restricted)
	if err != nil {
		t.Fatal(err)
	}
	defer quiet.Close()

	if warning := captureStderr(t, func() { warnIfPrivileged(ctx, quiet) }); warning != "" {
		t.Errorf("the restricted role was warned anyway: %q", warning)
	}
}
