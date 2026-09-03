package storage

import (
	"errors"
	"fmt"
	"math/big"
)

var (
	ErrNoPostings         = errors.New("transaction has no postings")
	ErrLedgerNotFound     = errors.New("ledger not found")
	ErrDuplicateReference = errors.New("reference already used")
	ErrNotFound           = errors.New("not found")
	ErrInvalidCursor      = errors.New("invalid cursor")
)

// the balance check failed. carries enough to tell the caller which account
// ran out and by how much, because "insufficient funds" alone is useless when
// a transaction touches a dozen accounts.
type InsufficientFundsError struct {
	Account   string
	Asset     string
	Available *big.Int
	Requested *big.Int
}

func (e *InsufficientFundsError) Error() string {
	return fmt.Sprintf("insufficient funds: %s holds %s %s, needs %s",
		e.Account, e.Available, e.Asset, e.Requested)
}

// an invalid posting, with the index of the one that failed.
type PostingError struct {
	Index int
	Err   error
}

func (e *PostingError) Error() string {
	return fmt.Sprintf("postings[%d]: %v", e.Index, e.Err)
}

func (e *PostingError) Unwrap() error { return e.Err }
