package storage

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/pixperk/giro/ledger"
)

// Closing an account, which is a statement about a relationship rather than
// about a balance.
//
// An account that is never closed carries on accepting postings for ever, and
// a payment made by mistake a year after a client left is as welcome as one
// made on the first day. Closure is what makes "this is over" a fact the
// ledger holds rather than something the systems around it are expected to
// remember.

// ErrAccountNotEmpty is returned when closing an account that still holds
// something.
//
// The rule is what stops closure stranding money. If an account could be
// closed with a balance, that balance would sit somewhere nothing accepts
// postings, and getting it out would need the very thing closure forbids. So
// the balance has to be dealt with first, which is also how a bank does it.
var ErrAccountNotEmpty = errors.New("account still holds a balance")

// CloseAccount refuses further movement in either direction.
//
// The account must hold nothing in any asset. Reopening is allowed and is the
// supported way to deal with money that arrives afterwards: a wire that
// bounces back after the client left is not an error, it is a reason to
// reopen, pay it out, and close again.
func (s *Store) CloseAccount(ctx context.Context, address ledger.Address) error {
	if !address.Valid() {
		return fmt.Errorf("invalid account address %q", address)
	}
	if address == ledger.WorldAccount {
		return fmt.Errorf("%w: world is the boundary of the ledger and closing it would "+
			"refuse every deposit and every payout", ErrAccountClosureRefused)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	// locked while the balances are read, so a commit that has already taken
	// these rows finishes first and its balance is the one seen here. what it
	// cannot stop is a commit that starts afterwards, which is what
	// VerifyClosedAccounts is for.
	rows, err := tx.Query(ctx, `
		select asset, (input - output)::text
		  from accounts_volumes
		 where ledger = $1 and address = $2 and input <> output
		 order by asset
		   for update`,
		s.ledger, address)
	if err != nil {
		return fmt.Errorf("read balances of %s: %w", address, err)
	}
	var held []string
	for rows.Next() {
		var asset, balance string
		if err := rows.Scan(&asset, &balance); err != nil {
			rows.Close()
			return err
		}
		held = append(held, balance+" "+asset)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(held) > 0 {
		return fmt.Errorf("%w: %s holds %v", ErrAccountNotEmpty, address, held)
	}

	// an account nothing has ever touched has no row, so closing it is a
	// statement about an address rather than an edit of a record. inserting
	// one is how that statement is recorded at all.
	if _, err := tx.Exec(ctx, `
		insert into accounts (ledger, address, address_array, first_usage, insertion_date, updated_at, closed)
		values ($1, $2, $3, now(), now(), now(), true)
		on conflict (ledger, address) do update set closed = true, updated_at = now()`,
		s.ledger, address, address.Segments()); err != nil {
		return fmt.Errorf("close %s: %w", address, err)
	}
	return tx.Commit(ctx)
}

// ErrAccountClosureRefused is returned for an account that must not be closed.
var ErrAccountClosureRefused = errors.New("account cannot be closed")

// ReopenAccount lets an account take postings again.
//
// Not an undo. Closure and reopening are both recorded, and an account that
// has been reopened is a different thing from one that was never closed:
// something arrived after the relationship ended and somebody decided what to
// do about it.
func (s *Store) ReopenAccount(ctx context.Context, address ledger.Address) error {
	if !address.Valid() {
		return fmt.Errorf("invalid account address %q", address)
	}
	tag, err := s.pool.Exec(ctx, `
		update accounts set closed = false, updated_at = now()
		 where ledger = $1 and address = $2 and closed`,
		s.ledger, address)
	if err != nil {
		return fmt.Errorf("reopen %s: %w", address, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%s is not closed", address)
	}
	return nil
}

// IsClosed reports whether an account refuses further movement. An address
// nothing has ever touched is open.
func (s *Store) IsClosed(ctx context.Context, address ledger.Address) (bool, error) {
	var closed bool
	err := s.pool.QueryRow(ctx,
		"select coalesce((select closed from accounts where ledger = $1 and address = $2), false)",
		s.ledger, address).Scan(&closed)
	if err != nil {
		return false, fmt.Errorf("read closed for %s: %w", address, err)
	}
	return closed, nil
}

// ClosedAccountHoldsBalance names a closed account that holds something.
type ClosedAccountHoldsBalance struct {
	Account ledger.Address
	Asset   ledger.Asset
	Balance *big.Int
}

func (e *ClosedAccountHoldsBalance) Error() string {
	return fmt.Sprintf("%s is closed and holds %s %s", e.Account, e.Balance, e.Asset)
}

// VerifyClosedAccounts finds closed accounts that hold a balance, and reports
// how many closed accounts it looked at.
//
// Closing requires a zero balance, so this should always find nothing. It can
// still happen: closure locks the balance rows it can see, which stops a
// commit already in flight, and does not stop one that starts a moment later.
//
// Refusing that would mean every commit taking a lock on the account row, and
// a lock on a row the write path also updates is the shape that deadlocks --
// the same reason there is no foreign key to ledgers. So the race is left open
// deliberately and looked for here, which is the same trade as
// VerifyBalancePermissions.
//
// A finding is not necessarily a fault. It is money somewhere nothing accepts
// postings, and it needs someone to reopen the account and deal with it.
func (s *Store) VerifyClosedAccounts(ctx context.Context) (checked int, err error) {
	rows, err := s.pool.Query(ctx, `
		select a.address, v.asset, (v.input - v.output)::text
		  from accounts a
		  join accounts_volumes v on v.ledger = a.ledger and v.address = a.address
		 where a.ledger = $1 and a.closed and v.input <> v.output
		 order by a.address, v.asset`,
		s.ledger)
	if err != nil {
		return 0, fmt.Errorf("verify closed accounts: %w", err)
	}
	defer rows.Close()

	var found []error
	for rows.Next() {
		var e ClosedAccountHoldsBalance
		var balance string
		if err := rows.Scan(&e.Account, &e.Asset, &balance); err != nil {
			return 0, err
		}
		e.Balance, _ = new(big.Int).SetString(balance, 10)
		found = append(found, &e)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	// closed accounts examined, not findings: a run against a ledger where
	// nothing has been closed has looked at nothing and proves nothing.
	if err := s.pool.QueryRow(ctx,
		"select count(*) from accounts where ledger = $1 and closed", s.ledger).Scan(&checked); err != nil {
		return 0, err
	}
	return checked, errors.Join(found...)
}
