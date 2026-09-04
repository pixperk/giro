package storage

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Running the checks, and recording that they ran.
//
// Each check is useful on its own and none of them is useful unrun. The
// operational failure this closes is the one that hides every other problem:
//
//	a detector that stopped running looks exactly like a book with nothing
//	wrong.
//
// So a run records what it examined as well as what it found, and alerting
// takes two conditions rather than one: findings above zero, and the absence
// of a recent run.

// CheckResult is one check's outcome.
type CheckResult struct {
	Name    string        `json:"name"`
	Checked int           `json:"checked"`
	OK      bool          `json:"ok"`
	Detail  string        `json:"detail,omitempty"`
	Took    time.Duration `json:"took"`
}

// how much of a finding is worth keeping. a ledger with ten thousand problems
// does not need ten thousand lines stored to say something is wrong, and the
// first few name enough of it to start looking.
const maxDetail = 4000

// VerifyOptions selects which checks run.
type VerifyOptions struct {
	// prefix and age for the stale balance check. a zero StaleAfter skips it,
	// because "money that has not moved in no time at all" is every balance
	// and answers nothing.
	StalePrefix string
	StaleAfter  time.Duration

	// record the outcome. off by default so a caller can check without
	// writing, which a read replica has to be able to do.
	Record bool
}

// Verify runs every check and reports each outcome, in a fixed order so two
// runs are comparable.
//
// A check that finds problems and a check that could not run both come back
// with OK false, because both mean the same thing operationally: this check is
// not currently telling you the book is sound. Detail says which.
//
// It does not stop at the first failure. A run that reports one problem and
// hides four is worse than useless during an incident.
func (s *Store) Verify(ctx context.Context, opts VerifyOptions) ([]CheckResult, error) {
	checks := []struct {
		name string
		run  func(context.Context) (int, error)
	}{
		{"conservation", s.VerifyConservation},
		{"log", s.VerifyLog},
		{"projection", s.VerifyProjection},
		{"effective_volumes", s.VerifyEffectiveVolumes},
		{"balance_permissions", s.VerifyBalancePermissions},
	}

	results := make([]CheckResult, 0, len(checks)+1)
	for _, c := range checks {
		start := time.Now()
		checked, err := c.run(ctx)
		results = append(results, result(c.name, checked, err, time.Since(start)))
	}

	if opts.StaleAfter > 0 {
		start := time.Now()
		stale, err := s.StaleBalances(ctx, opts.StalePrefix, opts.StaleAfter)
		r := CheckResult{Name: "stale_balances", OK: true, Took: time.Since(start)}
		switch {
		case err != nil:
			r.OK, r.Detail = false, err.Error()
		case len(stale) > 0:
			r.OK, r.Checked = false, len(stale)
			for _, b := range stale {
				r.Detail = truncateDetail(r.Detail + b.String() + "\n")
			}
		}
		results = append(results, r)
	}

	if opts.Record {
		if err := s.recordVerification(ctx, results); err != nil {
			return results, err
		}
	}
	return results, nil
}

func result(name string, checked int, err error, took time.Duration) CheckResult {
	r := CheckResult{Name: name, Checked: checked, OK: err == nil, Took: took}
	if err != nil {
		r.Detail = truncateDetail(err.Error())
	}
	return r
}

func truncateDetail(s string) string {
	if len(s) <= maxDetail {
		return s
	}
	return s[:maxDetail] + "\n... truncated"
}

func (s *Store) recordVerification(ctx context.Context, results []CheckResult) error {
	for _, r := range results {
		if _, err := s.pool.Exec(ctx, `
			insert into verification_runs (ledger, check_name, checked, ok, detail, took_ms)
			values ($1, $2, $3, $4, nullif($5, ''), $6)`,
			s.ledger, r.Name, r.Checked, r.OK, r.Detail, r.Took.Milliseconds()); err != nil {
			return fmt.Errorf("record %s: %w", r.Name, err)
		}
	}
	return nil
}

// LastVerified reports when each check last ran on this ledger.
//
// This is the half of alerting people leave out. A check missing from this map
// has never run, and a check whose timestamp is old has stopped running, and
// neither says anything about whether the book is sound.
func (s *Store) LastVerified(ctx context.Context) (map[string]time.Time, error) {
	rows, err := s.pool.Query(ctx, `
		select distinct on (check_name) check_name, ran_at
		  from verification_runs
		 where ledger = $1
		 order by check_name, ran_at desc`, s.ledger)
	if err != nil {
		return nil, fmt.Errorf("last verified: %w", err)
	}
	defer rows.Close()

	out := map[string]time.Time{}
	for rows.Next() {
		var name string
		var at time.Time
		if err := rows.Scan(&name, &at); err != nil {
			return nil, err
		}
		out[name] = at.UTC()
	}
	return out, rows.Err()
}

// VerifyConservation checks the master invariant: for any asset, every balance
// summed together is exactly zero.
//
// Every posting adds the same amount to one account's input and another's
// output, so the sums cancel by construction. A non-zero total means value was
// created or destroyed, and nothing else in the system would say so.
//
// The database enforces this at commit (§5.6), so a finding here means either
// that enforcement was bypassed by something holding more privilege than the
// application, or that it has a hole. Either way it is the first thing to know.
func (s *Store) VerifyConservation(ctx context.Context) (checked int, err error) {
	rows, err := s.pool.Query(ctx, `
		select asset, (sum(input) - sum(output))::text, count(*)
		  from accounts_volumes
		 where ledger = $1
		 group by asset
		 order by asset`, s.ledger)
	if err != nil {
		return 0, fmt.Errorf("verify conservation: %w", err)
	}
	defer rows.Close()

	var drifted []string
	for rows.Next() {
		var asset, drift string
		var accounts int
		if err := rows.Scan(&asset, &drift, &accounts); err != nil {
			return 0, err
		}
		checked += accounts
		if drift != "0" {
			amount, _ := new(big.Int).SetString(drift, 10)
			drifted = append(drifted, fmt.Sprintf("%s drifted by %s", asset, amount))
		}
	}
	if err := rows.Err(); err != nil {
		return checked, err
	}
	if len(drifted) > 0 {
		return checked, fmt.Errorf("conservation broken: %v", drifted)
	}
	return checked, nil
}

// Ledgers lists every ledger in the database.
//
// A package function rather than a method, because a Store is scoped to one
// ledger and that scoping is the tenant boundary: giving it a method that
// looks across all of them would be the one call in the package that ignores
// the thing every other call is careful about. An operator sweeping every
// ledger is a different job from serving one.
func Ledgers(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	rows, err := pool.Query(ctx, "select name from ledgers order by name")
	if err != nil {
		return nil, fmt.Errorf("list ledgers: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}
