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
	"github.com/pixperk/giro/ledger"
	"github.com/pixperk/giro/storage"
	"math/big"
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

// The command that runs the checks. It is the piece an operator schedules, so
// the thing that matters most is what it does to the exit code: a scheduler
// notices a non-zero exit and ignores anything printed.
func TestVerifyCommand(t *testing.T) {
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

	if _, err := pool.Exec(ctx, "insert into ledgers (name) values ('main')"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		"insert into assets (ledger, asset) values ('main', 'USD/2')"); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.New(pool, "main").CommitTransaction(ctx, ledger.Postings{
		{Source: "world", Destination: "users:alice", Asset: "USD/2", Amount: big.NewInt(10000)},
	}, storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}

	t.Run("a sound ledger passes and says what it examined", func(t *testing.T) {
		out, err := captureOutput(t, func() error { return dispatch(ctx, []string{"verify"}) })
		if err != nil {
			t.Fatalf("a sound ledger failed: %v\n%s", err, out)
		}
		for _, check := range []string{"conservation", "log", "projection", "effective_volumes", "balance_permissions"} {
			if !strings.Contains(out, check) {
				t.Errorf("%s did not run:\n%s", check, out)
			}
		}
		if !strings.Contains(out, "checked") {
			t.Error("the output does not say what was examined, so a run against nothing looks like a pass")
		}
	})

	t.Run("running records that it ran", func(t *testing.T) {
		out, err := captureOutput(t, func() error { return dispatch(ctx, []string{"verify", "--last"}) })
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "never run") {
			t.Errorf("the previous run was not recorded:\n%s", out)
		}
		if !strings.Contains(out, "conservation") {
			t.Errorf("no last run for conservation:\n%s", out)
		}
	})

	t.Run("a finding is an error, so a scheduler notices", func(t *testing.T) {
		if _, err := pool.Exec(ctx, "alter table accounts_volumes disable trigger user"); err != nil {
			t.Fatal(err)
		}
		defer pool.Exec(ctx, "alter table accounts_volumes enable trigger user")
		if _, err := pool.Exec(ctx,
			"update accounts_volumes set input = input + 500 where ledger='main' and address='world'"); err != nil {
			t.Fatal(err)
		}

		out, err := captureOutput(t, func() error { return dispatch(ctx, []string{"verify"}) })
		if err == nil {
			t.Fatalf("a broken ledger exited zero:\n%s", out)
		}
		// the findings are printed even though the command fails, because the
		// error is the signal and the output is the diagnosis
		if !strings.Contains(out, "FAIL") || !strings.Contains(out, "drifted by") {
			t.Errorf("the finding was not reported:\n%s", out)
		}
	})

	t.Run("an unknown flag is a usage error, not a silent default", func(t *testing.T) {
		err := dispatch(ctx, []string{"verify", "--nonsense"})
		var u usageErr
		if !errors.As(err, &u) {
			t.Errorf("err = %v, want a usage error", err)
		}
	})
}

// Account policy is the one thing giro deliberately gives no endpoint, so the
// command is the only way to reach it, and a bug here has no second path
// around it.
func TestAccountCommand(t *testing.T) {
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

	if _, err := pool.Exec(ctx, "insert into ledgers (name) values ('main')"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "insert into assets (ledger, asset) values ('main', 'USD/2')"); err != nil {
		t.Fatal(err)
	}
	store := storage.New(pool, "main")

	run := func(t *testing.T, args ...string) (string, error) {
		t.Helper()
		return captureOutput(t, func() error { return dispatch(ctx, append([]string{"account"}, args...)) })
	}

	// The gap this command exists to close: over HTTP there is no way to make
	// a boundary account, so an inbound flow from a counterparty cannot be
	// expressed at all. This is the whole point, so it is the first test.
	t.Run("a boundary account can receive from outside the ledger", func(t *testing.T) {
		lp := ledger.Address("external:lp:kraken:USD")

		_, err := store.CommitTransaction(ctx, ledger.Postings{
			{Source: lp, Destination: "ops:usd", Asset: "USD/2", Amount: big.NewInt(9996)},
		}, storage.CommitOptions{})
		if err == nil {
			t.Fatal("a boundary account went negative before anyone permitted it")
		}

		if out, err := run(t, "allow-negative", "main", string(lp), "USD/2"); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}

		if _, err := store.CommitTransaction(ctx, ledger.Postings{
			{Source: lp, Destination: "ops:usd", Asset: "USD/2", Amount: big.NewInt(9996)},
		}, storage.CommitOptions{}); err != nil {
			t.Fatalf("still refused after the policy was set: %v", err)
		}
	})

	t.Run("show reports the bound beside the balance it governs", func(t *testing.T) {
		out, err := run(t, "show", "main", "external:lp:kraken:USD")
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		for _, want := range []string{"USD/2", "-9996", "unbounded"} {
			if !strings.Contains(out, want) {
				t.Errorf("output does not mention %q:\n%s", want, out)
			}
		}
	})

	t.Run("an untouched account says so rather than inventing a policy", func(t *testing.T) {
		out, err := run(t, "show", "main", "users:nobody")
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if !strings.Contains(out, "nothing has moved through") {
			t.Errorf("an account with no rows was described as if it had them:\n%s", out)
		}
	})

	// Narrowing a bound an account already sits outside is permitted -- it is
	// how an operator stops the bleeding -- so nothing refuses it and the
	// account stays outside its own rule. Being told at the time is the
	// difference between a decision and a surprise at tomorrow's verify.
	t.Run("narrowing a bound the account already breaks says so", func(t *testing.T) {
		out, err := run(t, "refuse-negative", "main", "external:lp:kraken:USD", "USD/2")
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if !strings.Contains(out, "outside that bound") {
			t.Errorf("the operator was not told the account already breaks the rule:\n%s", out)
		}
		if !strings.Contains(out, "giro verify") {
			t.Errorf("nothing said where this will show up:\n%s", out)
		}

		// and it is a real finding, not only a warning printed here
		vout, verr := captureOutput(t, func() error { return dispatch(ctx, []string{"verify", "--record=false"}) })
		if verr == nil {
			t.Fatalf("verify passed an account outside its own bound:\n%s", vout)
		}
		if !strings.Contains(vout, "balance_permissions") {
			t.Errorf("the wrong check reported it:\n%s", vout)
		}
	})

	t.Run("a cost line is bounded above rather than unbounded", func(t *testing.T) {
		if out, err := run(t, "allow-negative", "main", "cost:peg", "USD/2"); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if out, err := run(t, "refuse-positive", "main", "cost:peg", "USD/2"); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		out, err := run(t, "show", "main", "cost:peg")
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if !strings.Contains(out, "cost line") {
			t.Errorf("the pair of flags was not read as the fact it states:\n%s", out)
		}
	})

	// Each of these changes nothing and must say why, because a policy command
	// that silently succeeds against a typo is worse than one that fails.
	t.Run("a mistake is refused rather than quietly doing nothing", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			args []string
			want string
		}{
			{"a ledger that does not exist", []string{"show", "nosuch", "users:alice"}, "no ledger"},
			{"an asset this ledger does not handle", []string{"allow-negative", "main", "users:alice", "USD"}, "does not handle"},
			{"world may not be bounded below", []string{"refuse-negative", "main", "world", "USD/2"}, "must be allowed a negative"},
			{"an unknown verb", []string{"frobnicate", "main", "users:alice"}, "unknown account command"},
			{"too few arguments", []string{"allow-negative", "main", "users:alice"}, "usage"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				out, err := run(t, tc.args...)
				if err == nil {
					t.Fatalf("accepted:\n%s", out)
				}
				if !strings.Contains(err.Error(), tc.want) {
					t.Errorf("err = %v, want it to mention %q", err, tc.want)
				}
			})
		}
	})
}

// Go's flag package stops parsing at the first positional argument, so a flag
// written after a ledger name becomes a ledger name. Left alone, "giro verify
// main --record=false" checks a ledger that does not exist, finds nothing
// wrong with it, exits zero, and records the run -- which is indistinguishable
// from a clean pass and is the exact failure this command exists to surface.
func TestVerifyRefusesAFlagAfterALedgerName(t *testing.T) {
	ctx := context.Background()
	isolatedDatabase(t)
	if _, err := captureOutput(t, func() error { return dispatch(ctx, []string{"migrate", "up"}) }); err != nil {
		t.Fatal(err)
	}

	out, err := captureOutput(t, func() error {
		return dispatch(ctx, []string{"verify", "main", "--record=false"})
	})
	if err == nil {
		t.Fatalf("a flag after a ledger name was verified as a ledger:\n%s", out)
	}
	var u usageErr
	if !errors.As(err, &u) {
		t.Fatalf("err = %v (%T), want usageErr so it exits 2", err, err)
	}
	if !strings.Contains(u.text, "flags go first") {
		t.Errorf("the message does not say how to fix it:\n%s", u.text)
	}
}

// The recovery path is the one an operator runs while something has already
// gone wrong, so its failure modes matter more than most.
func TestRecoverCommand(t *testing.T) {
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

	if _, err := pool.Exec(ctx, "insert into ledgers (name) values ('main')"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "insert into assets (ledger, asset) values ('main', 'USD/2')"); err != nil {
		t.Fatal(err)
	}
	store := storage.New(pool, "main")
	for range 3 {
		if _, err := store.CommitTransaction(ctx, ledger.Postings{
			{Source: "world", Destination: "users:alice", Asset: "USD/2", Amount: big.NewInt(100)},
		}, storage.CommitOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	var recorded string
	t.Run("tip prints a position that can be pasted back in", func(t *testing.T) {
		out, err := captureOutput(t, func() error { return dispatch(ctx, []string{"recover", "tip"}) })
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		recorded = strings.TrimSpace(out)
		if !strings.HasPrefix(recorded, "main:3:") {
			t.Fatalf("tip = %q, want main:3:<hash>", recorded)
		}
		// the whole point is that it round trips through a deployment record
		if _, err := storage.ParseTip(recorded); err != nil {
			t.Errorf("the printed tip does not parse: %v", err)
		}
	})

	t.Run("check passes against the current position", func(t *testing.T) {
		out, err := captureOutput(t, func() error {
			return dispatch(ctx, []string{"recover", "check", recorded})
		})
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if !strings.Contains(out, "ok") {
			t.Errorf("output does not confirm the ledger: %s", out)
		}
	})

	t.Run("check fails, non-zero, against a position ahead of us", func(t *testing.T) {
		ahead := "main:99:" + strings.SplitN(recorded, ":", 3)[2]
		out, err := captureOutput(t, func() error {
			return dispatch(ctx, []string{"recover", "check", ahead})
		})
		if err == nil {
			t.Fatalf("a ledger behind its watermark exited zero:\n%s", out)
		}
		// a scheduler and a person both need to be told, and the message has
		// to say what not to do next
		if !strings.Contains(out, "FAIL") {
			t.Errorf("the finding was not printed: %s", out)
		}
		if !strings.Contains(err.Error(), "do not write") {
			t.Errorf("err = %v, want it to say writes must stop", err)
		}
	})

	t.Run("resume declares the gap and the chain still verifies", func(t *testing.T) {
		out, err := captureOutput(t, func() error {
			return dispatch(ctx, []string{"recover", "resume", "main:9:" + strings.SplitN(recorded, ":", 3)[2],
				"--note=incident 41"})
		})
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if !strings.Contains(out, "never reissued") {
			t.Errorf("output does not say the skipped ids are retired: %s", out)
		}

		// the gap is real and the chain still verifies, which is the property
		// the RECOVERY entry exists to provide
		if _, err := storage.New(pool, "main").VerifyLog(ctx); err != nil {
			t.Errorf("the chain broke across a declared gap: %v", err)
		}
		var kind, data string
		if err := pool.QueryRow(ctx,
			"select type, data::text from logs where ledger='main' and type='RECOVERY'").Scan(&kind, &data); err != nil {
			t.Fatalf("no recovery entry was appended: %v", err)
		}
		if !strings.Contains(data, "incident 41") {
			t.Errorf("the note was not recorded: %s", data)
		}
	})

	t.Run("a mistake is refused rather than guessed at", func(t *testing.T) {
		for _, tc := range []struct {
			name, want string
			args       []string
		}{
			{"a malformed tip", "malformed", []string{"recover", "check", "nonsense"}},
			{"an unknown ledger", "no ledger", []string{"recover", "resume", "nosuch:4:aaaa"}},
			{"an unknown subcommand", "unknown recover command", []string{"recover", "frobnicate"}},
			{"no arguments", "usage", []string{"recover"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				_, err := captureOutput(t, func() error { return dispatch(ctx, tc.args) })
				if err == nil {
					t.Fatal("accepted")
				}
				if !strings.Contains(err.Error(), tc.want) {
					t.Errorf("err = %v, want it to mention %q", err, tc.want)
				}
			})
		}
	})
}
