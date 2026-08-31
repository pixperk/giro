package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/pixperk/giro/internal/ledger"
)

// every query in this file carries `ledger = $1` from s.ledger, which is
// seeded at construction and is never a parameter. that column is the tenant
// boundary, and it fails silently when forgotten: the query returns more rows
// rather than an error. store_isolation_test.go exists to catch that.

func (s *Store) GetTransaction(ctx context.Context, id int64) (*ledger.Transaction, error) {
	var t ledger.Transaction
	var postings, metadata, pcv []byte

	err := s.pool.QueryRow(ctx, `
		select id, timestamp, inserted_at, reverted_at, coalesce(reference, ''),
		       postings, metadata, pc_volumes
		  from transactions
		 where ledger = $1 and id = $2`,
		s.ledger, id,
	).Scan(&t.ID, &t.Timestamp, &t.InsertedAt, &t.RevertedAt, &t.Reference,
		&postings, &metadata, &pcv)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: transaction %d", ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}

	if err := hydrateTransaction(&t, postings, metadata, pcv); err != nil {
		return nil, err
	}
	return &t, nil
}

func hydrateTransaction(t *ledger.Transaction, postings, metadata, pcv []byte) error {
	if err := json.Unmarshal(postings, &t.Postings); err != nil {
		return fmt.Errorf("decode postings: %w", err)
	}
	if len(metadata) > 0 {
		if err := json.Unmarshal(metadata, &t.Metadata); err != nil {
			return fmt.Errorf("decode metadata: %w", err)
		}
	}
	if len(pcv) > 0 {
		if err := json.Unmarshal(pcv, &t.PostCommitVolumes); err != nil {
			return fmt.Errorf("decode post commit volumes: %w", err)
		}
	}
	// every timestamp on the way out, or the same instant serialises
	// differently depending on the server's zone
	t.Timestamp = t.Timestamp.UTC()
	t.InsertedAt = t.InsertedAt.UTC()
	if t.RevertedAt != nil {
		utc := t.RevertedAt.UTC()
		t.RevertedAt = &utc
	}
	return nil
}

// GetBalances returns every asset balance held by one account.
//
// an account with no row has no balances rather than an error, because an
// address that was never touched and one that does not exist are the same
// thing: zero either way.
func (s *Store) GetBalances(ctx context.Context, address string) (map[string]*big.Int, error) {
	rows, err := s.pool.Query(ctx, `
		select asset, input, output from accounts_volumes
		 where ledger = $1 and address = $2
		 order by asset`,
		s.ledger, address)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]*big.Int{}
	for rows.Next() {
		var asset string
		var in, o pgtype.Numeric
		if err := rows.Scan(&asset, &in, &o); err != nil {
			return nil, err
		}
		input, err := bigInt(in)
		if err != nil {
			return nil, err
		}
		output, err := bigInt(o)
		if err != nil {
			return nil, err
		}
		out[asset] = new(big.Int).Sub(input, output)
	}
	return out, rows.Err()
}

// AggregateBalances sums a subtree, for example every account under "users:".
//
// prefix matching goes through the address text with a varchar_pattern_ops
// index rather than the gin index on the segments, because a trailing wildcard
// is a range scan and measurably cheaper. gin containment is also wrong here:
// it is position independent, so "users" would match "fees:users:refunds".
func (s *Store) AggregateBalances(ctx context.Context, prefix string) (map[string]*big.Int, error) {
	rows, err := s.pool.Query(ctx, `
		select asset, sum(input), sum(output) from accounts_volumes
		 where ledger = $1 and ($2 = '' or address like $2 || '%')
		 group by asset
		 order by asset`,
		s.ledger, prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]*big.Int{}
	for rows.Next() {
		var asset string
		var in, o pgtype.Numeric
		if err := rows.Scan(&asset, &in, &o); err != nil {
			return nil, err
		}
		input, err := bigInt(in)
		if err != nil {
			return nil, err
		}
		output, err := bigInt(o)
		if err != nil {
			return nil, err
		}
		out[asset] = new(big.Int).Sub(input, output)
	}
	return out, rows.Err()
}

type accountOptions struct {
	volumes          bool
	effectiveVolumes bool
	at               time.Time
}

type AccountOption func(*accountOptions)

// WithVolumes attaches the account's per asset volumes, at the cost of a
// second query.
//
// off by default: a lean response is the right default and the caller decides
// when the extra read is worth it. most reads of an account want its metadata,
// not its money.
func WithVolumes() AccountOption {
	return func(o *accountOptions) { o.volumes = true }
}

// WithEffectiveVolumes attaches what the account held as of a date, in
// effective date order, which differs from the current view whenever a
// transaction has been backdated. a zero time means now.
func WithEffectiveVolumes(at time.Time) AccountOption {
	return func(o *accountOptions) {
		o.effectiveVolumes = true
		o.at = at
	}
}

func (s *Store) GetAccount(ctx context.Context, address string, opts ...AccountOption) (*ledger.Account, error) {
	var o accountOptions
	for _, apply := range opts {
		apply(&o)
	}

	var a ledger.Account
	var metadata []byte

	err := s.pool.QueryRow(ctx, `
		select address, metadata, first_usage, insertion_date, updated_at
		  from accounts where ledger = $1 and address = $2`,
		s.ledger, address,
	).Scan(&a.Address, &metadata, &a.FirstUsage, &a.InsertionDate, &a.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: account %q", ErrNotFound, address)
	}
	if err != nil {
		return nil, err
	}

	if len(metadata) > 0 {
		if err := json.Unmarshal(metadata, &a.Metadata); err != nil {
			return nil, err
		}
	}
	a.FirstUsage = a.FirstUsage.UTC()
	a.InsertionDate = a.InsertionDate.UTC()
	a.UpdatedAt = a.UpdatedAt.UTC()

	if o.volumes {
		if a.Volumes, err = s.GetVolumes(ctx, address); err != nil {
			return nil, err
		}
	}
	if o.effectiveVolumes {
		at := o.at
		if at.IsZero() {
			at = time.Now()
		}
		if a.EffectiveVolumes, err = s.GetEffectiveVolumesAt(ctx, address, at); err != nil {
			return nil, err
		}
	}
	return &a, nil
}

// GetVolumes returns the raw counters rather than the derived balance. gross
// flow is information a balance destroys: an account that has settled millions
// and now holds nothing looks identical to one never used.
func (s *Store) GetVolumes(ctx context.Context, address string) (map[string]ledger.Volumes, error) {
	rows, err := s.pool.Query(ctx, `
		select asset, input, output from accounts_volumes
		 where ledger = $1 and address = $2
		 order by asset`,
		s.ledger, address)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]ledger.Volumes{}
	for rows.Next() {
		var asset string
		var in, o pgtype.Numeric
		if err := rows.Scan(&asset, &in, &o); err != nil {
			return nil, err
		}
		input, err := bigInt(in)
		if err != nil {
			return nil, err
		}
		output, err := bigInt(o)
		if err != nil {
			return nil, err
		}
		out[asset] = ledger.Volumes{Input: input, Output: output}
	}
	return out, rows.Err()
}

// --- lists -----------------------------------------------------------------

type TransactionFilter struct {
	// matches a transaction where this address is a source or a destination.
	Account string `json:"account,omitempty"`
	// matches every account under a prefix, for example "users:".
	AccountPrefix string `json:"accountPrefix,omitempty"`
	Reference     string `json:"reference,omitempty"`
}

type ListTransactionsQuery struct {
	Filter TransactionFilter
	Limit  int
	// when set, everything else is ignored: the cursor carries the filter it
	// was created with.
	Cursor string
}

func (s *Store) ListTransactions(ctx context.Context, q ListTransactionsQuery) (Page[ledger.Transaction], error) {
	var page Page[ledger.Transaction]

	c := cursor[TransactionFilter]{Filter: q.Filter, Limit: clampLimit(q.Limit)}
	if q.Cursor != "" {
		decoded, err := decodeCursor[TransactionFilter](q.Cursor)
		if err != nil {
			return page, err
		}
		c = decoded
	}

	// one more than asked for, so the presence of a next page is known without
	// a second count query.
	rows, err := s.pool.Query(ctx, `
		select id, timestamp, inserted_at, reverted_at, coalesce(reference, ''),
		       postings, metadata, pc_volumes
		  from transactions
		 where ledger = $1
		   and ($2 = 0 or id > $2)
		   and ($3 = '' or $3 = any(sources) or $3 = any(destinations))
		   and ($4 = '' or exists (
		         select 1 from unnest(sources || destinations) as a
		          where a like $4 || '%'))
		   and ($5 = '' or reference = $5)
		 order by id
		 limit $6`,
		s.ledger, c.After, c.Filter.Account, c.Filter.AccountPrefix, c.Filter.Reference, c.Limit+1)
	if err != nil {
		return page, err
	}
	defer rows.Close()

	for rows.Next() {
		var t ledger.Transaction
		var postings, metadata, pcv []byte
		if err := rows.Scan(&t.ID, &t.Timestamp, &t.InsertedAt, &t.RevertedAt, &t.Reference,
			&postings, &metadata, &pcv); err != nil {
			return page, err
		}
		if err := hydrateTransaction(&t, postings, metadata, pcv); err != nil {
			return page, err
		}
		page.Items = append(page.Items, t)
	}
	if err := rows.Err(); err != nil {
		return page, err
	}

	return finish(page, c, func(t ledger.Transaction) int64 { return t.ID })
}

type LogFilter struct{}

type ListLogsQuery struct {
	Limit  int
	Cursor string
}

// ListLogs walks the log in order. this is the seam a replica or a replay
// consumes, and the reason ids are gapless: a missing entry is detectable.
func (s *Store) ListLogs(ctx context.Context, q ListLogsQuery) (Page[ledger.Log], error) {
	var page Page[ledger.Log]

	c := cursor[LogFilter]{Limit: clampLimit(q.Limit)}
	if q.Cursor != "" {
		decoded, err := decodeCursor[LogFilter](q.Cursor)
		if err != nil {
			return page, err
		}
		c = decoded
	}

	rows, err := s.pool.Query(ctx, `
		select id, type, data, date, hash,
		       coalesce(idempotency_key, ''), coalesce(idempotency_hash, '')
		  from logs
		 where ledger = $1 and ($2 = 0 or id > $2)
		 order by id
		 limit $3`,
		s.ledger, c.After, c.Limit+1)
	if err != nil {
		return page, err
	}
	defer rows.Close()

	for rows.Next() {
		var l ledger.Log
		var typ string
		if err := rows.Scan(&l.ID, &typ, &l.Data, &l.Date, &l.Hash,
			&l.IdempotencyKey, &l.IdempotencyHash); err != nil {
			return page, err
		}
		l.Type = ledger.LogType(typ)
		l.Date = l.Date.UTC()
		page.Items = append(page.Items, l)
	}
	if err := rows.Err(); err != nil {
		return page, err
	}

	return finish(page, c, func(l ledger.Log) int64 { return l.ID })
}

// trims the extra row fetched to detect a next page, and builds the cursor
// that continues from the last item actually returned.
func finish[T any, F any](page Page[T], c cursor[F], id func(T) int64) (Page[T], error) {
	if len(page.Items) <= c.Limit {
		return page, nil
	}

	page.Items = page.Items[:c.Limit]
	next, err := encodeCursor(cursor[F]{
		Filter: c.Filter,
		After:  id(page.Items[len(page.Items)-1]),
		Limit:  c.Limit,
	})
	if err != nil {
		return page, err
	}
	page.Next = next
	return page, nil
}
