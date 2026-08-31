package ledger

import (
	"crypto/sha256"
	"time"
)

type LogType string

const (
	LogNewTransaction      LogType = "NEW_TRANSACTION"
	LogRevertedTransaction LogType = "REVERTED_TRANSACTION"
	LogSetMetadata         LogType = "SET_METADATA"
	LogDeleteMetadata      LogType = "DELETE_METADATA"
)

// a log entry records one mutation. the log is the source of truth: the
// transactions, moves and volumes tables are a projection of it, kept because
// replaying from zero on every read would be absurd.
type Log struct {
	ID   int64     `json:"id"`
	Type LogType   `json:"type"`
	Date time.Time `json:"date"`

	// the exact bytes stored in logs.data and covered by Hash. kept as raw
	// json rather than a decoded value because the hash is over bytes, and
	// re-encoding a decoded value is not guaranteed to reproduce them.
	Data []byte `json:"data"`

	Hash []byte `json:"hash"`

	// supplied by the caller. IdempotencyHash is over the request inputs, so a
	// replayed key carrying different inputs is an error rather than a silent
	// success for a payment that never happened.
	IdempotencyKey  string `json:"idempotencyKey,omitempty"`
	IdempotencyHash string `json:"idempotencyHash,omitempty"`
}

// what a metadata log entry records. the target is a transaction id or an
// account address, as a string either way, so one payload shape covers both.
type MetadataTargetType string

const (
	TargetTransaction MetadataTargetType = "TRANSACTION"
	TargetAccount     MetadataTargetType = "ACCOUNT"
)

type SetMetadataPayload struct {
	TargetType MetadataTargetType `json:"targetType"`
	TargetID   string             `json:"targetId"`
	Metadata   Metadata           `json:"metadata"`
}

type DeleteMetadataPayload struct {
	TargetType MetadataTargetType `json:"targetType"`
	TargetID   string             `json:"targetId"`
	Key        string             `json:"key"`
}

// ChainHash covers the previous entry's hash as well as this entry's bytes, so
// editing any historical entry invalidates every hash after it. hashing each
// entry alone would let someone rewrite one and recompute just its own digest.
//
// previous is nil for the first entry in a ledger. every other hash is exactly
// 32 bytes, so there is no ambiguity in the concatenation.
func ChainHash(previous, data []byte) []byte {
	h := sha256.New()
	h.Write(previous)
	h.Write(data)
	return h.Sum(nil)
}
