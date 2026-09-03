package storage

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/jackc/pgx/v5"

	"github.com/pixperk/giro/ledger"
)

// which accounts may end below zero, and why that is a setting rather than a
// naming rule.
//
// most accounts must not go negative: an account spending money it does not
// have is the failure the ledger exists to prevent. a few must. world is the
// obvious one, since the first deposit has to come from somewhere. the others
// are cost lines. a peg absorption account is not a pot of money that can run
// out, it is a running total of what carrying the client's price risk has cost
// so far, and it only ever goes one way.
//
// the guard cannot tell those apart on its own, and a naming convention would
// only mean a typo could join the exempt set. so it is a flag, default closed,
// set in a statement somebody reviewed.

// ErrWorldMustAllowNegative is returned by SetAllowNegative when asked to
// refuse world a negative balance.
//
// world is where value enters. taking the permission away means the next
// deposit into an empty ledger is rejected for overdrawing the account that is
// defined by being overdrawn, and every existing balance becomes unreachable
// from the outside. it is never a thing anyone means to do.
var ErrWorldMustAllowNegative = errors.New("world must be allowed a negative balance")

// SetAllowNegative permits or refuses a negative balance for one account in
// one asset.
//
// per asset rather than per account, because an account can be a cost line in
// one and an ordinary balance in another. an operating account that absorbs
// USD peg movements should still not be able to overdraw its USDT.
//
// the row is created if the account has never been touched, so a book can be
// set up before it is used rather than only after something has moved through
// it.
func (s *Store) SetAllowNegative(ctx context.Context, address ledger.Address, asset ledger.Asset, allow bool) error {
	if !address.Valid() {
		return fmt.Errorf("invalid account address %q", address)
	}
	if !asset.Valid() {
		return fmt.Errorf("invalid asset %q", asset)
	}
	if address == ledger.WorldAccount && !allow {
		return ErrWorldMustAllowNegative
	}

	_, err := s.pool.Exec(ctx, `
		insert into accounts_volumes (ledger, address, asset, allow_negative)
		values ($1, $2, $3, $4)
		on conflict (ledger, address, asset)
		do update set allow_negative = excluded.allow_negative`,
		s.ledger, address, asset, allow)
	if err != nil {
		return fmt.Errorf("set allow_negative for %s/%s: %w", address, asset, err)
	}
	return nil
}

// UnpermittedNegative names one account and asset holding a negative balance
// it is not permitted to hold.
type UnpermittedNegative struct {
	Account ledger.Address
	Asset   ledger.Asset
	Balance *big.Int
}

func (e *UnpermittedNegative) Error() string {
	return fmt.Sprintf("%s %s: balance %s, and the account is not permitted to go negative",
		e.Account, e.Asset, e.Balance)
}

// VerifyBalancePermissions finds accounts sitting below zero without the
// permission to, and reports how many rows it checked.
//
// The commit path makes this state unreachable, so it should always find
// nothing. It can still arise two ways, and they are worth telling apart.
//
// The mundane one: a permission was revoked while the account was already
// negative. That leaves the balance where it is on purpose, because the
// alternative is the ledger inventing a correcting transaction nobody
// authorised. Expected, but somebody should know.
//
// The one that matters: the guard has a hole, or something wrote to
// accounts_volumes without going through it. Every other check would pass. The
// hash chain is intact because nothing was tampered with, conservation holds
// because the other side of the posting is right there, and the projection
// agrees because the log records exactly what was written. Only this notices.
//
// Not on the write path. Run it on a schedule, and alert on the absence of a
// run as well as on findings: a detector that stopped running looks exactly
// like a book with nothing wrong.
func (s *Store) VerifyBalancePermissions(ctx context.Context) (checked int, err error) {
	rows, err := s.pool.Query(ctx, `
		select address, asset, (input - output)::text
		  from accounts_volumes
		 where ledger = $1 and not allow_negative and input < output
		 order by address, asset`,
		s.ledger)
	if err != nil {
		return 0, fmt.Errorf("verify balance permissions: %w", err)
	}
	defer rows.Close()

	var found []error
	for rows.Next() {
		var e UnpermittedNegative
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

	// the count is of rows examined, not rows returned: the query filters in
	// the database, so a separate count is the only way to say "this actually
	// looked at something" rather than "this found nothing".
	if err := s.pool.QueryRow(ctx,
		"select count(*) from accounts_volumes where ledger = $1", s.ledger).Scan(&checked); err != nil {
		return 0, err
	}
	return checked, errors.Join(found...)
}

// AllowsNegative reports whether the account may end below zero in this asset.
//
// an account that has never been touched reports whatever it would be created
// with, so the answer does not depend on whether anything has moved yet.
func (s *Store) AllowsNegative(ctx context.Context, address ledger.Address, asset ledger.Asset) (bool, error) {
	var allow bool
	err := s.pool.QueryRow(ctx, `
		select allow_negative from accounts_volumes
		 where ledger = $1 and address = $2 and asset = $3`,
		s.ledger, address, asset).Scan(&allow)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return address == ledger.WorldAccount, nil
	case err != nil:
		return false, fmt.Errorf("read allow_negative for %s/%s: %w", address, asset, err)
	}
	return allow, nil
}
