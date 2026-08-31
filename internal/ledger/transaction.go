package ledger

import (
	"errors"
	"fmt"
	"time"
)

// Metadata is opaque to the ledger. it is never interpreted, only stored.
//
// the caps are a size guard on a table that only grows, the same reasoning as
// the digit cap on an amount. they are deliberately generous: metadata is for
// identifiers and short labels, and anything larger belongs in the system that
// owns the document rather than in a ledger row.
type Metadata map[string]string

const (
	MaxMetadataKeys        = 32
	MaxMetadataKeyLength   = 128
	MaxMetadataValueLength = 1024
)

var (
	ErrEmptyMetadataKey     = errors.New("metadata key is empty")
	ErrTooManyMetadataKeys  = errors.New("too many metadata keys")
	ErrMetadataKeyTooLong   = errors.New("metadata key too long")
	ErrMetadataValueTooLong = errors.New("metadata value too long")
)

func (m Metadata) Validate() error {
	if len(m) > MaxMetadataKeys {
		return fmt.Errorf("%w: %d, limit is %d", ErrTooManyMetadataKeys, len(m), MaxMetadataKeys)
	}
	for k, v := range m {
		switch {
		case k == "":
			return ErrEmptyMetadataKey
		case len(k) > MaxMetadataKeyLength:
			return fmt.Errorf("%w: %q is %d bytes, limit is %d",
				ErrMetadataKeyTooLong, truncate(k), len(k), MaxMetadataKeyLength)
		case len(v) > MaxMetadataValueLength:
			return fmt.Errorf("%w: key %q holds %d bytes, limit is %d",
				ErrMetadataValueTooLong, truncate(k), len(v), MaxMetadataValueLength)
		}
	}
	return nil
}

// an oversized key would otherwise be echoed back in full inside the error it
// caused.
func truncate(s string) string {
	if len(s) <= 32 {
		return s
	}
	return s[:32] + "..."
}

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
