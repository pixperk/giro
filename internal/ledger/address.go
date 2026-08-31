package ledger

import (
	"regexp"
	"strings"
)

// an address is one or more segments joined by ':', like "users:alice:wallet".
// accounts are never registered, they exist as soon as a posting names one.
var addressSegment = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// reports whether every segment is non-empty and uses only [a-zA-Z0-9_-].
// rejects "", "a::b", and anything with whitespace, so "world " is not "world".
func ValidateAddress(addr string) bool {
	for _, seg := range strings.Split(addr, ":") {
		if !addressSegment.MatchString(seg) {
			return false
		}
	}
	return true
}

// splits an address into its segments. this is what gets stored as a jsonb
// array and gin indexed, so prefix queries like "users:*" hit an index instead
// of scanning. assumes addr has already passed ValidateAddress.
func Segments(addr string) []string {
	return strings.Split(addr, ":")
}
