package ledger

import (
	"regexp"
	"strings"
)

// Address names an account: one or more segments joined by ':', like
// "users:alice:wallet" or "external:lp:kraken:USD".
//
// A named type rather than a string, so that an address and an asset are not
// interchangeable in a signature. Both are strings underneath and neither
// costs anything at runtime; the point is that a value carried through several
// layers of application code cannot arrive in the wrong parameter.
//
// It does not catch a transposed source and destination, since both are
// addresses. Nothing in the type system can.
//
// Accounts are never registered. One exists as soon as a posting names it.
type Address string

var addressSegment = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// Valid reports whether every segment is non-empty and uses only
// [a-zA-Z0-9_-]. It rejects "", "a::b", and anything with whitespace, so
// "world " is not "world".
func (a Address) Valid() bool {
	for seg := range strings.SplitSeq(string(a), ":") {
		if !addressSegment.MatchString(seg) {
			return false
		}
	}
	return true
}

// Segments splits an address on ':'. This is what gets stored as an array and
// indexed, so prefix queries like "users:*" hit an index instead of scanning.
// Assumes the address has already passed Valid.
func (a Address) Segments() []string {
	return strings.Split(string(a), ":")
}

func (a Address) String() string { return string(a) }
