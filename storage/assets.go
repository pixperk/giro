package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/pixperk/giro/ledger"
)

// Which assets a ledger is allowed to handle.
//
// Accounts are not registered: one exists because a posting named it, and an
// account nobody has touched answers every question the same way as one that
// does not exist. Assets are the opposite case, and the difference is worth
// stating because the two decisions look inconsistent otherwise.
//
// An asset is a string carrying its own scale, so "USD/2" and "USD/6" are both
// well formed, are different assets, and never mix. Without a registry a typo
// in one caller is not an error, it is a second currency that accumulates its
// own balances, and conservation still passes because each pile balances on
// its own. There are a handful of assets in a system and thousands of
// accounts, so requiring the handful to be declared costs almost nothing and
// closes a failure mode that is otherwise silent and permanent.

// ErrUnknownAsset is returned when a posting names an asset this ledger has
// not registered.
var ErrUnknownAsset = errors.New("asset is not registered")

// UnknownAssetError names the asset and, when the ledger has one, the spelling
// it does know.
type UnknownAssetError struct {
	Asset ledger.Asset
	// the registered asset sharing this one's code, if there is one. a typo in
	// a scale is the likely cause, so saying "you have USD/2" is more use than
	// listing every asset on the ledger.
	Registered ledger.Asset
}

func (e *UnknownAssetError) Error() string {
	if e.Registered != "" {
		return fmt.Sprintf("%s is not registered, but %s is: same currency, different scale",
			e.Asset, e.Registered)
	}
	return fmt.Sprintf("%s is not registered on this ledger", e.Asset)
}

func (e *UnknownAssetError) Unwrap() error { return ErrUnknownAsset }

// RegisterAsset declares an asset this ledger may handle.
//
// Permanent, and one scale per code: registering USD/2 makes USD/6 and bare
// USD refusable. Re-registering the same asset is a no-op rather than an
// error, so a startup routine that declares its chart of accounts can run on
// every boot.
func (s *Store) RegisterAsset(ctx context.Context, asset ledger.Asset) error {
	if !asset.Valid() {
		return fmt.Errorf("invalid asset %q", asset)
	}

	_, err := s.pool.Exec(ctx, `
		insert into assets (ledger, asset) values ($1, $2)
		on conflict (ledger, asset) do nothing`,
		s.ledger, asset)
	if err == nil {
		return nil
	}

	// the one-scale-per-code index, which the on conflict clause above does
	// not cover: it names the primary key, and this is a different constraint.
	if conflicting, e := s.assetWithCodeOf(ctx, asset); e == nil && conflicting != "" {
		return fmt.Errorf("cannot register %s: %s is already registered, "+
			"and a ledger holds one scale per currency", asset, conflicting)
	}
	return fmt.Errorf("register asset %s: %w", asset, err)
}

// Assets lists what this ledger may handle, in registration order.
func (s *Store) Assets(ctx context.Context) ([]ledger.Asset, error) {
	rows, err := s.pool.Query(ctx,
		"select asset from assets where ledger = $1 order by registered_at, asset", s.ledger)
	if err != nil {
		return nil, fmt.Errorf("list assets: %w", err)
	}
	defer rows.Close()

	var out []ledger.Asset
	for rows.Next() {
		var a ledger.Asset
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// the registered asset sharing this one's code, if any.
func (s *Store) assetWithCodeOf(ctx context.Context, asset ledger.Asset) (ledger.Asset, error) {
	var found ledger.Asset
	err := s.pool.QueryRow(ctx, `
		select asset from assets
		 where ledger = $1 and split_part(asset, '/', 1) = $2`,
		s.ledger, asset.Code()).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return found, err
}

// checkAssets rejects a transaction naming an asset this ledger has not
// registered.
//
// The foreign keys enforce this for every writer, and this exists for the
// error rather than the enforcement: a constraint violation says
// "accounts_volumes_asset_registered", and a caller who mistyped a scale needs
// to be told that USD/2 exists and USD/6 does not. It runs before any lock is
// taken, so a rejected transaction costs nothing.
func (s *Store) checkAssets(ctx context.Context, p ledger.Postings) error {
	seen := make(map[ledger.Asset]bool, len(p))
	assets := make([]string, 0, len(p))
	for _, posting := range p {
		if !seen[posting.Asset] {
			seen[posting.Asset] = true
			assets = append(assets, string(posting.Asset))
		}
	}

	// the ledger's existence rides along, in the same round trip. an
	// unregistered asset on a ledger that does not exist is not the useful
	// half of that sentence, and checking separately would cost a query on
	// every commit to improve one error message.
	var exists bool
	var missing *ledger.Asset
	err := s.pool.QueryRow(ctx, `
		select exists (select 1 from ledgers where name = $1),
		       (select a.asset
		          from unnest($2::text[]) as a(asset)
		          left join assets r on r.ledger = $1 and r.asset = a.asset
		         where r.asset is null
		         limit 1)`,
		s.ledger, assets).Scan(&exists, &missing)
	if err != nil {
		return fmt.Errorf("check assets: %w", err)
	}
	if !exists {
		return fmt.Errorf("%w: %q", ErrLedgerNotFound, s.ledger)
	}
	if missing == nil {
		return nil
	}

	conflicting, _ := s.assetWithCodeOf(ctx, *missing)
	return &UnknownAssetError{Asset: *missing, Registered: conflicting}
}
