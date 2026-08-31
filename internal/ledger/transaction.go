package ledger

import "time"

// Metadata is opaque to the ledger. it is never interpreted, only stored.
type Metadata map[string]string

// the volumes of every account a transaction touched, immediately after it
// committed. account -> asset -> volumes.
type PostCommitVolumes map[string]map[string]Volumes

func (v PostCommitVolumes) Set(account, asset string, vol Volumes) {
	if v[account] == nil {
		v[account] = map[string]Volumes{}
	}
	v[account][asset] = vol
}

type Transaction struct {
	ID       int64    `json:"id"`
	Postings Postings `json:"postings"`

	// when it happened economically, and when this database learned of it.
	// they disagree whenever news arrives late.
	Timestamp  time.Time `json:"timestamp"`
	InsertedAt time.Time `json:"insertedAt"`

	// set when a reversal exists. the reversal itself is a separate
	// transaction with its own id, this is only a mark.
	RevertedAt *time.Time `json:"revertedAt,omitempty"`

	Reference string   `json:"reference,omitempty"`
	Metadata  Metadata `json:"metadata,omitempty"`

	// frozen at commit and never rewritten, even when a backdated transaction
	// lands later and changes what was true on this date.
	PostCommitVolumes PostCommitVolumes `json:"postCommitVolumes,omitempty"`
}
