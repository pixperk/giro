package storage

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// the largest page a caller can ask for. an unbounded limit is a denial of
// service vector on a table that only grows.
const (
	DefaultPageSize = 25
	MaxPageSize     = 100
)

type Page[T any] struct {
	Items []T `json:"items"`
	// opaque, empty when there is nothing after this page.
	Next string `json:"next,omitempty"`
}

// keyset pagination, never OFFSET.
//
// on an append only table, OFFSET skips and repeats rows as new ones land
// between requests. in most systems that is cosmetic. in a ledger it reads as
// missing money, and the client cannot tell it apart from the real thing.
//
// the cursor carries the filters as well as the position, so a caller cannot
// change the query halfway through a walk and get an incoherent sequence.
type cursor[F any] struct {
	Filter F     `json:"f"`
	After  int64 `json:"a"`
	Limit  int   `json:"l"`
}

func encodeCursor[F any](c cursor[F]) (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func decodeCursor[F any](s string) (cursor[F], error) {
	var c cursor[F]
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return c, fmt.Errorf("%w: not valid base64", ErrInvalidCursor)
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%w: not a valid cursor", ErrInvalidCursor)
	}
	if c.Limit <= 0 || c.Limit > MaxPageSize {
		return c, fmt.Errorf("%w: bad limit", ErrInvalidCursor)
	}
	return c, nil
}

func clampLimit(n int) int {
	switch {
	case n <= 0:
		return DefaultPageSize
	case n > MaxPageSize:
		return MaxPageSize
	default:
		return n
	}
}
