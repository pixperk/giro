package main

import (
	"context"
	"fmt"
	"os"
	"slices"
	"text/tabwriter"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pixperk/giro/ledger"
	"github.com/pixperk/giro/storage"
)

// account policy is operator work with a shell, never a route.
//
// every subcommand here either turns a guard off or takes an account out of
// service. an endpoint that let a caller mark its own account unbounded would
// put permission to disable the overdraw guard on the same wire as the
// requests that guard exists to refuse, which is not an API but a hole in one.
//
// the privilege is not the distinction -- giro_app is granted update on these
// columns and always was, because the commit path needs the row. the channel
// is. this reaches the same database with the same role and no listener in
// front of it, which is what makes it a decision somebody took rather than one
// a request could take for them.
const accountUsage = `usage:
  giro account show <ledger> <address>
                                  how the account is bounded, per asset

  giro account allow-negative  <ledger> <address> <asset>
  giro account refuse-negative <ledger> <address> <asset>
                                  may it end below zero. a boundary account
                                  facing a counterparty needs this; almost
                                  nothing else does

  giro account allow-positive  <ledger> <address> <asset>
  giro account refuse-positive <ledger> <address> <asset>
                                  may it end above zero. a cost line only ever
                                  leans one way, and refusing this is what says so

  giro account close  <ledger> <address>
  giro account reopen <ledger> <address>
                                  take it out of service, or put it back. an
                                  account still holding a balance cannot close

reads DATABASE_URL from the environment.
`

func accountCommand(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageErr{accountUsage}
	}

	verb := args[0]
	rest := args[1:]

	// every verb takes a ledger and an address; the four bound settings take
	// an asset after that, because a bound is per asset -- an account can be a
	// cost line in one and an ordinary balance in another.
	wantAsset := false
	switch verb {
	case "allow-negative", "refuse-negative", "allow-positive", "refuse-positive":
		wantAsset = true
	case "show", "close", "reopen":
	default:
		return usageErr{fmt.Sprintf("unknown account command %q\n\n%s", verb, accountUsage)}
	}

	want := 2
	if wantAsset {
		want = 3
	}
	if len(rest) != want {
		return usageErr{accountUsage}
	}

	ledgerName, address := rest[0], ledger.Address(rest[1])
	if !address.Valid() {
		return fmt.Errorf("invalid account address %q", address)
	}
	var asset ledger.Asset
	if wantAsset {
		asset = ledger.Asset(rest[2])
		if !asset.Valid() {
			return fmt.Errorf("invalid asset %q, expected a code and a scale like USD/2", asset)
		}
	}

	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return fmt.Errorf("DATABASE_URL is not set")
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	names, err := storage.Ledgers(ctx, pool)
	if err != nil {
		return err
	}
	// a typo in the ledger name would otherwise set a policy on an account in
	// a ledger that does not exist, report success, and change nothing anyone
	// will ever read.
	if !slices.Contains(names, ledgerName) {
		return fmt.Errorf("no ledger %q. this database has: %v", ledgerName, names)
	}
	store := storage.New(pool, ledgerName)

	// setting a bound on an unregistered asset otherwise reaches the foreign
	// key and comes back as a constraint name. "USD" and "USD/2" are different
	// assets and dropping the scale is the likely slip, so say what this
	// ledger actually holds.
	if wantAsset {
		registered, err := store.Assets(ctx)
		if err != nil {
			return err
		}
		if !slices.Contains(registered, asset) {
			return fmt.Errorf("ledger %s does not handle %s. it handles: %v", ledgerName, asset, registered)
		}
	}

	switch verb {
	case "show":
		return showAccount(ctx, store, address)
	case "allow-negative":
		return setBound(ctx, store, address, asset, "negative", true)
	case "refuse-negative":
		return setBound(ctx, store, address, asset, "negative", false)
	case "allow-positive":
		return setBound(ctx, store, address, asset, "positive", true)
	case "refuse-positive":
		return setBound(ctx, store, address, asset, "positive", false)
	case "close":
		if err := store.CloseAccount(ctx, address); err != nil {
			return err
		}
		fmt.Printf("closed %s in %s\n", address, ledgerName)
		return nil
	case "reopen":
		if err := store.ReopenAccount(ctx, address); err != nil {
			return err
		}
		fmt.Printf("reopened %s in %s\n", address, ledgerName)
		return nil
	}
	return usageErr{accountUsage}
}

func setBound(ctx context.Context, s *storage.Store, address ledger.Address, asset ledger.Asset, side string, allow bool) error {
	var err error
	if side == "negative" {
		err = s.SetAllowNegative(ctx, address, asset, allow)
	} else {
		err = s.SetAllowPositive(ctx, address, asset, allow)
	}
	if err != nil {
		return err
	}

	verb := "refuses"
	if allow {
		verb = "permits"
	}
	fmt.Printf("%s %s a %s balance in %s\n", address, verb, side, asset)

	// narrowing a bound an account already sits outside is allowed -- it is
	// how an operator stops the bleeding -- so it will not fail here, and the
	// account stays outside its own rule until something moves. that is a
	// finding giro verify will report tonight, and saying so now is cheaper
	// than being surprised by it then.
	if allow {
		return nil
	}
	// The setting has landed. What follows only describes it, so a failure to
	// read the balance back leaves the command successful and quiet rather
	// than reporting that a change which did happen did not.
	policy, perr := s.Policy(ctx, address)
	if perr != nil {
		return nil //nolint:nilerr // described above: the write succeeded
	}
	for _, p := range policy {
		if p.Asset != asset {
			continue
		}
		outside := (side == "negative" && p.Balance.Sign() < 0) ||
			(side == "positive" && p.Balance.Sign() > 0)
		if outside {
			fmt.Printf("  note: it already holds %s %s, which is outside that bound.\n"+
				"        nothing was refused, and giro verify will report it until it moves.\n",
				p.Balance, asset)
		}
	}
	return nil
}

func showAccount(ctx context.Context, s *storage.Store, address ledger.Address) error {
	closed, err := s.IsClosed(ctx, address)
	if err != nil {
		return err
	}
	policy, err := s.Policy(ctx, address)
	if err != nil {
		return err
	}

	state := "open"
	if closed {
		state = "closed"
	}
	fmt.Printf("%s  %s\n", address, state)

	if len(policy) == 0 {
		fmt.Println("  nothing has moved through this account yet, so it has no policy rows.")
		fmt.Println("  it would be created bounded below and unbounded above, like any other.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "  ASSET\tBALANCE\tBOUNDS")
	for _, p := range policy {
		_, _ = fmt.Fprintf(w, "  %s\t%s\t%s\n", p.Asset, p.Balance, bounds(p))
	}
	return w.Flush()
}

// bounds says what the two flags mean together, because "allow_negative=true,
// allow_positive=true" is a pair of settings and "unbounded" is the fact.
func bounds(p storage.AssetPolicy) string {
	switch {
	case p.AllowNegative && p.AllowPositive:
		return "unbounded — no rule in either direction"
	case p.AllowNegative:
		return "at or below zero — a cost line"
	case p.AllowPositive:
		return "at or above zero — the ordinary account"
	default:
		return "zero only — it may hold nothing"
	}
}
