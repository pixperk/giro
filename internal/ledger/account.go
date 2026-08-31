package ledger

import "time"

// an account is never registered, it appears because a posting named it. this
// record exists for metadata and prefix queries: an address absent from it
// still has a balance, which is zero.
type Account struct {
	Address  string   `json:"address"`
	Metadata Metadata `json:"metadata,omitempty"`

	// the earliest effective date this account has been involved in, which a
	// backdated transaction can move earlier. InsertionDate never moves.
	FirstUsage    time.Time `json:"firstUsage"`
	InsertionDate time.Time `json:"insertionDate"`
	UpdatedAt     time.Time `json:"updatedAt"`

	// asset -> volumes. only populated by reads that ask for it, since it is a
	// second query.
	Volumes map[string]Volumes `json:"volumes,omitempty"`
}
