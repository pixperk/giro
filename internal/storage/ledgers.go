package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var ErrLedgerExists = errors.New("ledger already exists")

type LedgerInfo struct {
	Name    string    `json:"name"`
	AddedAt time.Time `json:"addedAt"`
}

// CreateLedger creates the row that every other write depends on.
//
// the counters on it allocate transaction and log ids, and the lock taken on
// it is what serialises the hash chain, so a ledger has to exist before
// anything can be committed to it.
func (s *Store) CreateLedger(ctx context.Context) (*LedgerInfo, error) {
	var l LedgerInfo
	err := s.pool.QueryRow(ctx,
		`insert into ledgers (name) values ($1) returning name, added_at`,
		s.ledger).Scan(&l.Name, &l.AddedAt)

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
		return nil, fmt.Errorf("%w: %q", ErrLedgerExists, s.ledger)
	}
	if err != nil {
		return nil, err
	}
	l.AddedAt = l.AddedAt.UTC()
	return &l, nil
}

func (s *Store) GetLedger(ctx context.Context) (*LedgerInfo, error) {
	var l LedgerInfo
	err := s.pool.QueryRow(ctx,
		`select name, added_at from ledgers where name = $1`, s.ledger).Scan(&l.Name, &l.AddedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: ledger %q", ErrNotFound, s.ledger)
	}
	if err != nil {
		return nil, err
	}
	l.AddedAt = l.AddedAt.UTC()
	return &l, nil
}
