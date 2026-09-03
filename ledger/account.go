package ledger

import "time"

// an account is never registered, it appears because a posting named it. this
// record exists for metadata and prefix queries: an address absent from it
// still has a balance, which is zero.
type Account struct {
	Address  Address  `json:"address"`
	Metadata Metadata `json:"metadata,omitempty"`

	// the earliest effective date this account has been involved in, which a
	// backdated transaction can move earlier. InsertionDate never moves.
	FirstUsage    time.Time `json:"firstUsage"`
	InsertionDate time.Time `json:"insertionDate"`
	UpdatedAt     time.Time `json:"updatedAt"`

	// asset -> volumes. only populated by reads that ask for it, since each is
	// a second query.
	//
	// Volumes is what the account holds now. EffectiveVolumes is what it held
	// as of a date, which differs whenever a transaction has been backdated.
	Volumes          map[Asset]Volumes `json:"volumes,omitempty"`
	EffectiveVolumes map[Asset]Volumes `json:"effectiveVolumes,omitempty"`
}
