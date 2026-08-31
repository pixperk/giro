package ledger

import (
	"regexp"
	"strconv"
	"strings"
)

// an asset carries its own scale: "USD/2" means two decimal places, so 100 is
// $1.00. "USD" and "USD/2" are different assets and never mix.
var assetPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*(/\d{1,6})?$`)

func ValidateAsset(s string) bool {
	return assetPattern.MatchString(s)
}

// "USD/2" -> 2, "POINTS" -> 0. ok is false if s is not a valid asset.
// display only. nothing in the engine may branch on scale.
func AssetScale(s string) (scale int, ok bool) {
	if !ValidateAsset(s) {
		return 0, false
	}
	_, digits, found := strings.Cut(s, "/")
	if !found {
		return 0, true
	}
	scale, _ = strconv.Atoi(digits) // pattern already guarantees 1 to 6 digits
	return scale, true
}
