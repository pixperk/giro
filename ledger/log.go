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

	// LogRecovery marks resuming after a restore, and declares the range of
	// ids that were issued in a database this one no longer contains.
	//
	// It is the only entry an operator causes rather than a caller, and it
	// exists so that the gap it leaves is stated rather than merely present:
	// verification accepts a gap only when the entry after it declares that
	// gap, so an undeclared one is still a broken chain.
	LogRecovery LogType = "RECOVERY"
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

// what a reversal records: which transaction was undone, and the transaction
// that undid it. both, because a reader of the log should not have to join
// against anything to understand the entry.
type RevertedTransactionPayload struct {
	RevertedTransactionID int64        `json:"revertedTransactionId"`
	Transaction           *Transaction `json:"transaction"`
}

// keys the ledger writes into metadata itself. the giro/ prefix is reserved,
// so a caller's own keys can never collide with them.
const (
	// marks a reversal with the id of the transaction it undoes, so the pair
	// can be found from either side without a dedicated column.
	RevertsKey = "giro/reverts"

	// marks every transaction in a batch with that batch's input hash, which
	// is what lets a replayed idempotency key find all of them. the unique
	// index allows the key itself on only one log entry.
	BatchKey = "giro/batch"
)

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

// RecoveryPayload records what a restore lost, so the gap in the log is
// self-explaining a year later when nobody remembers the incident.
type RecoveryPayload struct {
	// ResumedFrom is the last log id the restored database actually contained.
	ResumedFrom int64 `json:"resumedFrom"`

	// SkippedThrough is the highest log id known to have been issued before
	// the restore. Ids from ResumedFrom+1 to SkippedThrough inclusive belonged
	// to entries this database does not have, and are never reissued.
	SkippedThrough int64 `json:"skippedThrough"`

	// Note is whatever the operator wants their colleagues to read: a ticket,
	// an incident number, a sentence.
	Note string `json:"note,omitempty"`
}
