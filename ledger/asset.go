package ledger

import (
	"regexp"
	"strconv"
	"strings"
)

// Asset is what kind of thing is moving, carrying its own scale: "USD/2" means
// two decimal places, so 100 is $1.00. "USD" and "USD/2" are different assets
// and never mix.
//
// A named type for the same reason as Address: an asset and an address are
// both strings, and without this the compiler cannot tell a signature taking
// one from a signature taking the other.
type Asset string

var assetPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*(/\d{1,6})?$`)

func (a Asset) Valid() bool {
	return assetPattern.MatchString(string(a))
}

// Scale reports the number of decimal places: "USD/2" is 2, "POINTS" is 0.
// ok is false if the asset is not well formed.
//
// Display only. Nothing in the engine may branch on scale.
func (a Asset) Scale() (scale int, ok bool) {
	if !a.Valid() {
		return 0, false
	}
	_, digits, found := strings.Cut(string(a), "/")
	if !found {
		return 0, true
	}
	scale, _ = strconv.Atoi(digits) // pattern already guarantees 1 to 6 digits
	return scale, true
}

func (a Asset) String() string { return string(a) }
