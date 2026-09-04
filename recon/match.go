package recon

import (
	"context"
	"fmt"
	"math/big"

	"github.com/jackc/pgx/v5/pgtype"
)

// Matching staged statement lines to movements in the ledger.
//
// # The stance
//
// A line matches on an exact reference or it does not match at all. No fuzzy
// matching, no matching by amount and date, no guessing. The reasoning is
// worth stating because the temptation is real and the cost is asymmetric:
//
//	an unmatched line costs somebody five minutes.
//	a falsely matched one costs a restatement.
//
// So everything here is deterministic, and anything it cannot resolve is left
// alone and reported rather than resolved on a balance of probabilities.
//
// # The rules, in order
//
// Cheapest first, and each one only sees what the previous ones left.
//
//  1. exact: one line, one movement, one reference.
//  2. consolidated: one line paying several movements that share a reference,
//     and whose amounts sum to it exactly.
//
// Deliberately absent, and each for a reason.
//
// Many-to-many subset-sum, where some subset of lines matches some subset of
// movements: combinatorial, and a set that happens to add up is not evidence
// it is the right set.
//
// Fuzzy or amount-and-date matching: two payments of the same size on the same
// day are common, and telling them apart is exactly what a reference is for.
//
// # What it matches against
//
// Movements on boundary accounts, not transactions. A statement line is one
// account, one asset, one amount, one direction, which is what a move is. A
// transaction can be two of those at once -- a conversion moves stablecoin one
// way and dollars the other -- and has no single amount to compare a line to.

// Rule names, recorded on every match so a pairing can be explained later.
const (
	RuleExact        = "exact_ref"
	RuleConsolidated = "consolidated_ref"
)

// Summary is one matching run's outcome.
type Summary struct {
	// Matched is lines paired with a movement whose amount agrees.
	Matched int
	// Variance is lines paired with a movement whose amount does not. The
	// pairing is recorded with the difference: somebody thinks a different
	// amount moved, and that is worth a person's attention rather than a
	// silent adjustment.
	Variance int
	// Unmatched is what is left, by reason.
	Unmatched map[Break]int
}

// Break says why a line did not match, because "unmatched" is four different
// problems with four different people fixing them.
type Break string

const (
	// NoReference: the source gave no match key at all. A source
	// configuration problem, not a ledger one.
	NoReference Break = "no_reference"
	// NotFound: the reference is good and names nothing here. Either they
	// recorded something we did not, or we have not recorded it yet.
	NotFound Break = "reference_not_found"
	// Ambiguous: the reference resolves to several movements that do not sum
	// to the line. Indistinguishable from a coincidence, so it is left alone.
	Ambiguous Break = "reference_ambiguous"
	// Contested: the movement it resolves to was already matched by an
	// earlier line from this source.
	Contested Break = "movement_already_matched"
)

func (s *Summary) count(b Break, n int) {
	if s.Unmatched == nil {
		s.Unmatched = map[Break]int{}
	}
	s.Unmatched[b] += n
}

// Match pairs every unmatched staged line it can and reports what happened.
//
// Idempotent: a line already matched is not looked at again, so running twice
// changes nothing and running on a schedule is the intended use.
//
// It writes no postings and changes no balance. A reconciler able to correct
// the book would be a second way for money to move, and the entire value of
// this layer is that its opinion is independent.
func Match(ctx context.Context, db DB, ledgerName string, cfg Config) (Summary, error) {
	var sum Summary

	matched, variance, err := matchExact(ctx, db, ledgerName, cfg)
	if err != nil {
		return sum, err
	}
	sum.Matched, sum.Variance = matched, variance

	matched, err = matchConsolidated(ctx, db, ledgerName, cfg)
	if err != nil {
		return sum, err
	}
	sum.Matched += matched

	breaks, err := classify(ctx, db, ledgerName, cfg)
	if err != nil {
		return sum, err
	}
	for b, n := range breaks {
		sum.count(b, n)
	}
	return sum, nil
}

// candidates is the join every rule starts from: a staged line and the
// boundary movements its reference could name.
//
// The direction check lives here rather than in each rule. A boundary account
// is the outside world, so money arriving debits it -- the move on that
// account is the source -- and money leaving credits it. A line that does not
// say which way it went skips the check, which is the source being unhelpful
// rather than wrong.
const candidates = `
	select r.record_id, r.source, r.amount as line_amount, r.reference,
	       m.seq as move_seq, m.amount as move_amount
	  from recon_records r
	  join transactions t
	    on t.ledger = r.ledger
	   and (t.reference = r.reference or t.metadata->>$2 = r.reference)
	  join moves m
	    on m.ledger = t.ledger and m.tx_id = t.id
	   and m.asset = r.asset
	   and m.address like $3 || '%'
	 where r.ledger = $1
	   and r.matched_count = 0
	   and r.reference is not null
	   and (r.direction is null
	     or (r.direction = 'in'  and m.is_source)
	     or (r.direction = 'out' and not m.is_source))
	   and not exists (select 1 from recon_matches x
	                    where x.ledger = r.ledger and x.source = r.source
	                      and x.move_seq = m.seq)
`

// one line, one movement.
//
// DISTINCT ON with an explicit order resolves a movement contested by two
// lines to the earliest staged one, which is what makes repeated runs give the
// same answer instead of whatever the planner felt like.
func matchExact(ctx context.Context, db DB, ledgerName string, cfg Config) (matched, variance int, err error) {
	rows, err := db.Query(ctx, `
		with candidates as (`+candidates+`),
		sized as (
			select record_id, source, count(*) as n from candidates group by record_id, source
		),
		picks as (
			select distinct on (c.move_seq)
			       c.record_id, c.source, c.move_seq,
			       c.line_amount - c.move_amount as variance
			  from candidates c join sized s using (record_id, source)
			 where s.n = 1
			 order by c.move_seq, c.record_id
		),
		recorded as (
			insert into recon_matches (ledger, source, record_id, move_seq, variance, set_size, rule)
			select $1, source, record_id, move_seq, variance, 1, $4 from picks
			returning record_id, source, variance
		),
		marked as (
			update recon_records r set matched_count = 1, matched_at = now()
			  from recorded d
			 where r.ledger = $1 and r.source = d.source and r.record_id = d.record_id
		)
		select variance from recorded`,
		ledgerName, ExternalRefKey, cfg.boundaryPrefix(), RuleExact)
	if err != nil {
		return 0, 0, fmt.Errorf("match exact: %w", err)
	}
	defer rows.Close()

	tolerance := cfg.tolerance()
	for rows.Next() {
		var v pgNumeric
		if err := rows.Scan(&v); err != nil {
			return matched, variance, err
		}
		if v.big().CmpAbs(tolerance) > 0 {
			variance++
		} else {
			matched++
		}
	}
	return matched, variance, rows.Err()
}

// one line paying several movements that share a reference.
//
// The set must sum to the line exactly, and that equality is the whole
// discriminator. Several movements under one reference is either a
// consolidated payment or an ambiguous reference, and nothing in the reference
// itself tells them apart -- a real batch adds up to the line it paid, and two
// unrelated movements that happen to share a string do not.
//
// A set that does not add up therefore stays unmatched rather than being
// recorded as a partial match. Matching it would assert a pairing nothing
// justifies and, worse, drop it out of the queue of things a person still has
// to look at.
func matchConsolidated(ctx context.Context, db DB, ledgerName string, cfg Config) (matched int, err error) {
	rows, err := db.Query(ctx, `
		with candidates as (`+candidates+`),
		sized as (
			select record_id, source, count(*) as n, sum(move_amount) as total
			  from candidates group by record_id, source
		),
		sets as (
			select c.record_id, c.source, c.move_seq, s.n
			  from candidates c join sized s using (record_id, source)
			 where s.n > 1 and s.total = c.line_amount
		),
		recorded as (
			insert into recon_matches (ledger, source, record_id, move_seq, variance, set_size, rule)
			select $1, source, record_id, move_seq, 0, n, $4 from sets
			returning record_id, source
		),
		marked as (
			update recon_records r set matched_count = c.n, matched_at = now()
			  from (select record_id, source, max(n) as n from sets group by record_id, source) c
			 where r.ledger = $1 and r.source = c.source and r.record_id = c.record_id
			returning r.record_id
		)
		select count(*) from marked`,
		ledgerName, ExternalRefKey, cfg.boundaryPrefix(), RuleConsolidated)
	if err != nil {
		return 0, fmt.Errorf("match consolidated: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		if err := rows.Scan(&matched); err != nil {
			return 0, err
		}
	}
	return matched, rows.Err()
}

// why each remaining line did not match. four problems, four different people.
func classify(ctx context.Context, db DB, ledgerName string, cfg Config) (map[Break]int, error) {
	rows, err := db.Query(ctx, `
		with candidates as (`+candidates+`),
		sized as (
			select record_id, source, count(*) as n from candidates group by record_id, source
		)
		select case
		         when r.reference is null then $4
		         when s.n is null then (
		           case when exists (
		                  select 1 from recon_matches x
		                   join moves m on m.ledger = x.ledger and m.seq = x.move_seq
		                   join transactions t on t.ledger = m.ledger and t.id = m.tx_id
		                  where x.ledger = r.ledger and x.source = r.source
		                    and (t.reference = r.reference or t.metadata->>$2 = r.reference))
		                then $7 else $5 end)
		         else $6
		       end as reason, count(*)
		  from recon_records r
		  left join sized s using (record_id, source)
		 where r.ledger = $1 and r.matched_count = 0
		 group by reason`,
		ledgerName, ExternalRefKey, cfg.boundaryPrefix(),
		string(NoReference), string(NotFound), string(Ambiguous), string(Contested))
	if err != nil {
		return nil, fmt.Errorf("classify breaks: %w", err)
	}
	defer rows.Close()

	out := map[Break]int{}
	for rows.Next() {
		var reason string
		var n int
		if err := rows.Scan(&reason, &n); err != nil {
			return nil, err
		}
		out[Break(reason)] = n
	}
	return out, rows.Err()
}

// numeric scans without going near a float, the same way the engine does.
type pgNumeric struct{ v *big.Int }

func (n *pgNumeric) ScanNumeric(v pgtype.Numeric) error {
	if !v.Valid {
		n.v = new(big.Int)
		return nil
	}
	n.v = new(big.Int).Set(v.Int)
	for range v.Exp {
		n.v.Mul(n.v, big.NewInt(10))
	}
	return nil
}

func (n *pgNumeric) big() *big.Int {
	if n.v == nil {
		return new(big.Int)
	}
	return n.v
}
