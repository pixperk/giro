package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/pixperk/giro/internal/ledger"
	"github.com/pixperk/giro/internal/storage"
)

// error mapping lives here rather than in each handler, so a new storage error
// gets a sane status without touching every endpoint, and so the codes stay
// consistent across the api.
//
// the code is what clients branch on. the message is for humans and may change.
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	status, code, message := classify(err)

	if status == http.StatusInternalServerError {
		// the real error goes to the log, not to the client. an internal
		// message can carry table names, queries and account addresses.
		slog.ErrorContext(r.Context(), "request failed",
			"method", r.Method, "path", r.URL.Path, "error", err)
		message = "internal error"
	}

	writeJSON(w, status, Error{Code: code, Message: message, Details: detailsFor(err)})
}

func classify(err error) (status int, code ErrorCode, message string) {
	var insufficient *storage.InsufficientFundsError
	var posting *storage.PostingError

	switch {
	case errors.As(err, &insufficient):
		// the postings are well formed, they just cannot be applied
		return http.StatusUnprocessableEntity, INSUFFICIENTFUNDS, err.Error()
	case errors.As(err, &posting):
		return http.StatusBadRequest, VALIDATION, err.Error()

	case errors.Is(err, storage.ErrNotFound), errors.Is(err, storage.ErrLedgerNotFound):
		return http.StatusNotFound, NOTFOUND, err.Error()

	case errors.Is(err, storage.ErrIdempotencyMismatch):
		return http.StatusConflict, IDEMPOTENCYMISMATCH, err.Error()
	case errors.Is(err, storage.ErrDuplicateReference), errors.Is(err, storage.ErrLedgerExists),
		errors.Is(err, storage.ErrAlreadyReverted):
		return http.StatusConflict, CONFLICT, err.Error()

	case errors.Is(err, storage.ErrNoPostings), errors.Is(err, storage.ErrInvalidCursor),
		errors.Is(err, storage.ErrEmptyMetadata),
		errors.Is(err, ledger.ErrEmptyMetadataKey),
		errors.Is(err, ledger.ErrTooManyMetadataKeys),
		errors.Is(err, ledger.ErrMetadataKeyTooLong),
		errors.Is(err, ledger.ErrMetadataValueTooLong),
		errors.Is(err, ledger.ErrInvalidSourceAddress),
		errors.Is(err, storage.ErrBatchTooLarge):
		return http.StatusBadRequest, VALIDATION, err.Error()

	default:
		return http.StatusInternalServerError, INTERNAL, err.Error()
	}
}

// insufficient funds is the one error a caller can act on programmatically, so
// it carries the numbers rather than only a sentence.
func detailsFor(err error) *map[string]any {
	var e *storage.InsufficientFundsError
	if !errors.As(err, &e) {
		return nil
	}
	return &map[string]any{
		"account":   e.Account,
		"asset":     e.Asset,
		"available": e.Available.String(),
		"requested": e.Requested.String(),
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("failed to write response", "error", err)
	}
}

// decodes a request body into a typed struct.
//
// never into any: encoding/json routes numbers through float64, which silently
// destroys an amount above 2^53. a typed *big.Int field parses the digits
// directly.
func decodeJSON(w http.ResponseWriter, r *http.Request, into any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{
			Code:    VALIDATION,
			Message: "malformed request body: " + err.Error(),
		})
		return false
	}
	return true
}
