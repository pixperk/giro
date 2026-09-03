package api

import (
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/pixperk/giro/ledger"
	"github.com/pixperk/giro/storage"
)

func (s *Server) createLedger(w http.ResponseWriter, r *http.Request) {
	store := s.store(r.PathValue("ledger"))

	l, err := store.CreateLedger(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toAPILedger(l))
}

func (s *Server) getLedger(w http.ResponseWriter, r *http.Request) {
	l, err := s.store(r.PathValue("ledger")).GetLedger(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toAPILedger(l))
}

func (s *Server) createTransaction(w http.ResponseWriter, r *http.Request) {
	params, ok := query(w, r)
	if !ok {
		return
	}
	dryRun, ok := boolParam(w, params, "dryRun")
	if !ok {
		return
	}

	var body CreateTransactionRequest
	if !decodeJSON(w, r, &body) {
		return
	}

	opts := storage.CommitOptions{
		// the header rather than the body, so it is not part of the hashed
		// inputs: the same postings under a new key must hash the same.
		IdempotencyKey: r.Header.Get("Idempotency-Key"),
		DryRun:         dryRun,
	}
	if body.Timestamp != nil {
		opts.Timestamp = *body.Timestamp
	}
	if body.Reference != nil {
		opts.Reference = *body.Reference
	}
	if body.Metadata != nil {
		opts.Metadata = map[string]string(*body.Metadata)
	}

	store := s.store(r.PathValue("ledger"))
	tx, err := store.CommitTransaction(r.Context(), fromAPIPostings(body.Postings), opts)
	if err != nil {
		writeError(w, r, err)
		return
	}
	// 200 rather than 201: a dry run created nothing
	status := http.StatusCreated
	if dryRun {
		status = http.StatusOK
	}
	writeJSON(w, status, toAPITransaction(tx))
}

func (s *Server) listTransactions(w http.ResponseWriter, r *http.Request) {
	params, ok := query(w, r)
	if !ok {
		return
	}
	limit, ok := intParam(w, params, "limit")
	if !ok {
		return
	}

	q := storage.ListTransactionsQuery{
		Limit:  limit,
		Cursor: params.Get("cursor"),
		Filter: storage.TransactionFilter{
			Account:       ledger.Address(params.Get("account")),
			AccountPrefix: params.Get("accountPrefix"),
			Reference:     params.Get("reference"),
		},
	}

	page, err := s.store(r.PathValue("ledger")).ListTransactions(r.Context(), q)
	if err != nil {
		writeError(w, r, err)
		return
	}

	out := TransactionPage{Items: make([]Transaction, 0, len(page.Items))}
	for i := range page.Items {
		out.Items = append(out.Items, toAPITransaction(&page.Items[i]))
	}
	if page.Next != "" {
		out.Next = &page.Next
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getTransaction(w http.ResponseWriter, r *http.Request) {
	id, ok := transactionID(w, r)
	if !ok {
		return
	}

	tx, err := s.store(r.PathValue("ledger")).GetTransaction(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toAPITransaction(tx))
}

func (s *Server) getAccount(w http.ResponseWriter, r *http.Request) {
	params, ok := query(w, r)
	if !ok {
		return
	}

	at, ok := effectiveDate(w, params)
	if !ok {
		return
	}

	var opts []storage.AccountOption

	// unrecognised expansions are an error rather than silently ignored, so a
	// typo does not look like an empty result.
	for _, want := range strings.Split(params.Get("expand"), ",") {
		switch strings.TrimSpace(want) {
		case "":
		case "volumes":
			opts = append(opts, storage.WithVolumes())
		case "effectiveVolumes":
			opts = append(opts, storage.WithEffectiveVolumes(at))
		default:
			writeJSON(w, http.StatusBadRequest, Error{
				Code:    VALIDATION,
				Message: "unknown expand value " + strconv.Quote(want) + ", want volumes or effectiveVolumes",
			})
			return
		}
	}

	a, err := s.store(r.PathValue("ledger")).GetAccount(r.Context(), ledger.Address(r.PathValue("address")), opts...)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toAPIAccount(a))
}

func (s *Server) getBalances(w http.ResponseWriter, r *http.Request) {
	params, ok := query(w, r)
	if !ok {
		return
	}
	at, ok := effectiveDate(w, params)
	if !ok {
		return
	}

	store := s.store(r.PathValue("ledger"))
	address := ledger.Address(r.PathValue("address"))

	// asking for a date reads the effective view, which differs from the
	// current one whenever something has been backdated.
	var balances map[ledger.Asset]*big.Int
	var err error
	if at.IsZero() {
		balances, err = store.GetBalances(r.Context(), address)
	} else {
		balances, err = store.GetBalancesAt(r.Context(), address, at)
	}
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toAPIBalances(balances))
}

// zero means now, which the caller reads as the current view rather than a
// historical one. a present but unparseable date is an error.
func effectiveDate(w http.ResponseWriter, params url.Values) (time.Time, bool) {
	raw := params.Get("at")
	if raw == "" {
		return time.Time{}, true
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, Error{
			Code:    VALIDATION,
			Message: "at must be an RFC 3339 timestamp, for example 2026-03-01T12:00:00Z",
		})
		return time.Time{}, false
	}
	return at, true
}

func (s *Server) aggregateBalances(w http.ResponseWriter, r *http.Request) {
	params, ok := query(w, r)
	if !ok {
		return
	}

	balances, err := s.store(r.PathValue("ledger")).
		AggregateBalances(r.Context(), params.Get("prefix"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toAPIBalances(balances))
}

func (s *Server) listLogs(w http.ResponseWriter, r *http.Request) {
	params, ok := query(w, r)
	if !ok {
		return
	}
	limit, ok := intParam(w, params, "limit")
	if !ok {
		return
	}

	page, err := s.store(r.PathValue("ledger")).ListLogs(r.Context(), storage.ListLogsQuery{
		Limit:  limit,
		Cursor: params.Get("cursor"),
	})
	if err != nil {
		writeError(w, r, err)
		return
	}

	out := LogPage{Items: make([]Log, 0, len(page.Items))}
	for _, l := range page.Items {
		out.Items = append(out.Items, toAPILog(l))
	}
	if page.Next != "" {
		out.Next = &page.Next
	}
	writeJSON(w, http.StatusOK, out)
}

// r.URL.Query() discards anything it cannot parse, so a corrupted query string
// arrives at the handler as absent parameters. for a cursor that is dangerous:
// the client silently gets the first page again and reprocesses transactions it
// has already seen. parse once, and reject rather than guess.
func query(w http.ResponseWriter, r *http.Request) (url.Values, bool) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, Error{
			Code:    VALIDATION,
			Message: "malformed query string: " + err.Error(),
		})
		return nil, false
	}
	return values, true
}

// absent means false. only the values a caller could reasonably mean are
// accepted, so a typo is an error rather than a silent false.
func boolParam(w http.ResponseWriter, q url.Values, name string) (bool, bool) {
	switch q.Get(name) {
	case "":
		return false, true
	case "true", "1":
		return true, true
	case "false", "0":
		return false, true
	default:
		writeJSON(w, http.StatusBadRequest, Error{
			Code:    VALIDATION,
			Message: name + " must be true or false",
		})
		return false, false
	}
}

// absent means zero, which the storage layer reads as its default. a present
// but unparseable value is an error rather than a silent fallback.
func intParam(w http.ResponseWriter, q url.Values, name string) (int, bool) {
	raw := q.Get(name)
	if raw == "" {
		return 0, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, Error{
			Code:    VALIDATION,
			Message: name + " must be an integer",
		})
		return 0, false
	}
	return n, true
}

// --- metadata ---------------------------------------------------------------

func (s *Server) setTransactionMetadata(w http.ResponseWriter, r *http.Request) {
	id, ok := transactionID(w, r)
	if !ok {
		return
	}
	var m Metadata
	if !decodeJSON(w, r, &m) {
		return
	}

	tx, err := s.store(r.PathValue("ledger")).
		SetTransactionMetadata(r.Context(), id, map[string]string(m))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toAPITransaction(tx))
}

func (s *Server) deleteTransactionMetadata(w http.ResponseWriter, r *http.Request) {
	id, ok := transactionID(w, r)
	if !ok {
		return
	}

	tx, err := s.store(r.PathValue("ledger")).
		DeleteTransactionMetadataKey(r.Context(), id, r.PathValue("key"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toAPITransaction(tx))
}

func (s *Server) setAccountMetadata(w http.ResponseWriter, r *http.Request) {
	var m Metadata
	if !decodeJSON(w, r, &m) {
		return
	}

	a, err := s.store(r.PathValue("ledger")).
		SetAccountMetadata(r.Context(), ledger.Address(r.PathValue("address")), map[string]string(m))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toAPIAccount(a))
}

func (s *Server) deleteAccountMetadata(w http.ResponseWriter, r *http.Request) {
	a, err := s.store(r.PathValue("ledger")).
		DeleteAccountMetadataKey(r.Context(), ledger.Address(r.PathValue("address")), r.PathValue("key"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toAPIAccount(a))
}

func transactionID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Code: VALIDATION, Message: "id must be an integer"})
		return 0, false
	}
	return id, true
}

func (s *Server) revertTransaction(w http.ResponseWriter, r *http.Request) {
	id, ok := transactionID(w, r)
	if !ok {
		return
	}

	// the body is optional, so an empty one means both flags off
	var body RevertRequest
	if r.ContentLength > 0 && !decodeJSON(w, r, &body) {
		return
	}

	opts := storage.RevertOptions{}
	if body.AtEffectiveDate != nil {
		opts.AtEffectiveDate = *body.AtEffectiveDate
	}

	result, err := s.store(r.PathValue("ledger")).RevertTransaction(r.Context(), id, opts)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, Reversal{
		Original: toAPITransaction(result.Original),
		Reversal: toAPITransaction(result.Reversal),
	})
}

func (s *Server) listMoves(w http.ResponseWriter, r *http.Request) {
	params, ok := query(w, r)
	if !ok {
		return
	}
	limit, ok := intParam(w, params, "limit")
	if !ok {
		return
	}
	from, ok := optionalDate(w, params, "from")
	if !ok {
		return
	}
	to, ok := optionalDate(w, params, "to")
	if !ok {
		return
	}

	page, err := s.store(r.PathValue("ledger")).ListMoves(r.Context(), storage.ListMovesQuery{
		Limit:  limit,
		Cursor: params.Get("cursor"),
		Filter: storage.MoveFilter{
			Address: ledger.Address(r.PathValue("address")),
			Asset:   ledger.Asset(params.Get("asset")),
			From:    from,
			To:      to,
		},
	})
	if err != nil {
		writeError(w, r, err)
		return
	}

	out := MovePage{Items: make([]Move, 0, len(page.Items))}
	for _, m := range page.Items {
		out.Items = append(out.Items, toAPIMove(m))
	}
	if page.Next != "" {
		out.Next = &page.Next
	}
	writeJSON(w, http.StatusOK, out)
}

func optionalDate(w http.ResponseWriter, params url.Values, name string) (*time.Time, bool) {
	raw := params.Get(name)
	if raw == "" {
		return nil, true
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, Error{
			Code:    VALIDATION,
			Message: name + " must be an RFC 3339 timestamp",
		})
		return nil, false
	}
	return &at, true
}

func (s *Server) commitBatch(w http.ResponseWriter, r *http.Request) {
	params, ok := query(w, r)
	if !ok {
		return
	}
	dryRun, ok := boolParam(w, params, "dryRun")
	if !ok {
		return
	}

	var body BatchRequest
	if !decodeJSON(w, r, &body) {
		return
	}

	items := make([]storage.BatchItem, len(body.Transactions))
	for i, t := range body.Transactions {
		items[i] = storage.BatchItem{Postings: fromAPIPostings(t.Postings)}
		if t.Timestamp != nil {
			items[i].Timestamp = *t.Timestamp
		}
		if t.Reference != nil {
			items[i].Reference = *t.Reference
		}
		if t.Metadata != nil {
			items[i].Metadata = map[string]string(*t.Metadata)
		}
	}

	out, err := s.store(r.PathValue("ledger")).CommitBatch(r.Context(), items, storage.CommitOptions{
		IdempotencyKey: r.Header.Get("Idempotency-Key"),
		DryRun:         dryRun,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}

	resp := BatchResponse{Transactions: make([]Transaction, 0, len(out))}
	for _, tx := range out {
		resp.Transactions = append(resp.Transactions, toAPITransaction(tx))
	}

	status := http.StatusCreated
	if dryRun {
		status = http.StatusOK
	}
	writeJSON(w, status, resp)
}
