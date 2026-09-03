package ledger

import "testing"

func TestValidateAsset(t *testing.T) {
	tests := []struct {
		asset string
		want  bool
		why   string
	}{
		{"A", true, "single letter"},
		{"USD", true, "no scale"},
		{"USD/2", true, "with scale"},
		{"BTC/8", true, "eight decimals"},
		{"EUR/00", true, "leading zero is allowed by the pattern"},
		{"USD/123456", true, "six digit scale is the maximum"},
		{"EUR_COL", true, "underscore"},
		{"EUR_COL/12", true, "underscore with scale"},
		{"USD123", true, "digits after the first letter"},

		{"", false, "empty"},
		{"1", false, "must start with a letter"},
		{"1USD", false, "must start with a letter"},
		{"usd", false, "lowercase"},
		{"usd/2", false, "lowercase with scale"},
		{"USD/", false, "slash with no scale"},
		{"/2", false, "scale with no asset"},
		{"A//2", false, "double slash"},
		{"USD/2/2", false, "two scales"},
		{"USD/1234567", false, "seven digit scale is too long"},
		{"USD/x", false, "non numeric scale"},
		{"@s", false, "punctuation"},
		{"US D", false, "space"},
	}

	for _, tc := range tests {
		t.Run(tc.why, func(t *testing.T) {
			if got := ValidateAsset(tc.asset); got != tc.want {
				t.Errorf("ValidateAsset(%q) = %v, want %v", tc.asset, got, tc.want)
			}
		})
	}
}

func TestAssetScale(t *testing.T) {
	tests := []struct {
		asset     string
		wantScale int
		wantOK    bool
	}{
		{"USD/2", 2, true},
		{"BTC/8", 8, true},
		{"POINTS", 0, true},
		{"EUR/00", 0, true},
		{"USD/123456", 123456, true},

		// ok must imply the asset is valid, not merely parseable
		{"USD/", 0, false},
		{"usd/2", 0, false},
		{"1USD", 0, false},
		{"", 0, false},
		{"USD/x", 0, false},
	}

	for _, tc := range tests {
		t.Run(tc.asset, func(t *testing.T) {
			scale, ok := AssetScale(tc.asset)
			if scale != tc.wantScale || ok != tc.wantOK {
				t.Errorf("AssetScale(%q) = (%d, %v), want (%d, %v)",
					tc.asset, scale, ok, tc.wantScale, tc.wantOK)
			}
		})
	}
}
