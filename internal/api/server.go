// Package api serves the ledger over http.
//
// routing is stdlib ServeMux, which handles method and path patterns natively
// since go 1.22. the types in gen.go come from api/openapi.yaml, so the
// contract is written first and the shapes follow from it.
package api

import (
	"net/http"

	"github.com/pixperk/giro/storage"
)

type Server struct {
	// see AllowUnkeyedWrites. false means a write without an Idempotency-Key
	// is refused.
	allowUnkeyed bool

	mux *http.ServeMux
	// resolves a ledger name to a store scoped to it. the name is never a
	// parameter to a query, only an input to constructing the store.
	store func(ledger string) *storage.Store
}

// AllowUnkeyedWrites lets a write arrive without an Idempotency-Key.
//
// Off by default, and that default is the point. A connection severed after
// the server has committed but before the client hears about it leaves the
// caller unable to tell whether the payment landed -- a property of networks,
// not a bug. The only remedy is a key the caller can retry under, and the
// fault-injection tests exist to prove it works. A ledger that accepts an
// unkeyed write is a ledger that will eventually pay somebody twice.
//
// It exists at all because a caller may be doing its own deduplication
// upstream, and that is a decision somebody should make out loud rather than
// arrive at by not setting a header.
func AllowUnkeyedWrites() func(*Server) { return func(s *Server) { s.allowUnkeyed = true } }

func NewServer(store func(ledger string) *storage.Store, opts ...func(*Server)) *Server {
	s := &Server{mux: http.NewServeMux(), store: store}
	for _, o := range opts {
		o(s)
	}
	s.routes()
	return s
}

// requireKey refuses a write that carries no Idempotency-Key, and says why.
//
// A 400 rather than a warning: the request has not happened yet, so refusing
// it costs the caller a retry with a header. Accepting it costs them a
// duplicate payment on the day their network misbehaves, and they will not
// find out until reconciliation.
func (s *Server) requireKey(w http.ResponseWriter, r *http.Request) bool {
	if s.allowUnkeyed || r.Header.Get("Idempotency-Key") != "" {
		return true
	}
	// A dry run runs the whole commit path and rolls it back, so there is
	// nothing to duplicate and a lost response costs nothing but asking again.
	// Demanding a key there would be friction with no safety behind it.
	if r.URL.Query().Get("dryRun") == "true" {
		return true
	}
	writeJSON(w, http.StatusBadRequest, Error{
		Code: IDEMPOTENCYKEYREQUIRED,
		Message: "this endpoint moves money and requires an Idempotency-Key header. " +
			"a connection lost after the commit but before the response leaves you unable " +
			"to tell whether it landed, and retrying under the same key is the only way to " +
			"find out without paying twice. start the server with --allow-unkeyed-writes " +
			"if you deduplicate upstream.",
	})
	return false
}

// the patterns match the paths in api/openapi.yaml exactly. go 1.22 ServeMux
// uses the same {name} wildcard syntax openapi does, so the two are directly
// comparable, which is what routes_test.go checks.
func (s *Server) routes() {
	s.mux.HandleFunc("POST /v1/ledgers/{ledger}", s.createLedger)
	s.mux.HandleFunc("GET /v1/ledgers/{ledger}", s.getLedger)
	s.mux.HandleFunc("POST /v1/ledgers/{ledger}/assets", s.registerAsset)
	s.mux.HandleFunc("GET /v1/ledgers/{ledger}/assets", s.listAssets)

	s.mux.HandleFunc("POST /v1/ledgers/{ledger}/transactions", s.createTransaction)
	s.mux.HandleFunc("GET /v1/ledgers/{ledger}/transactions", s.listTransactions)
	s.mux.HandleFunc("POST /v1/ledgers/{ledger}/transactions/bulk", s.commitBatch)
	s.mux.HandleFunc("GET /v1/ledgers/{ledger}/transactions/{id}", s.getTransaction)

	s.mux.HandleFunc("POST /v1/ledgers/{ledger}/transactions/{id}/revert", s.revertTransaction)
	s.mux.HandleFunc("POST /v1/ledgers/{ledger}/transactions/{id}/metadata", s.setTransactionMetadata)
	s.mux.HandleFunc("DELETE /v1/ledgers/{ledger}/transactions/{id}/metadata/{key}", s.deleteTransactionMetadata)

	s.mux.HandleFunc("GET /v1/ledgers/{ledger}/accounts/{address}", s.getAccount)
	s.mux.HandleFunc("POST /v1/ledgers/{ledger}/accounts/{address}/metadata", s.setAccountMetadata)
	s.mux.HandleFunc("DELETE /v1/ledgers/{ledger}/accounts/{address}/metadata/{key}", s.deleteAccountMetadata)
	s.mux.HandleFunc("GET /v1/ledgers/{ledger}/accounts/{address}/balances", s.getBalances)
	s.mux.HandleFunc("GET /v1/ledgers/{ledger}/accounts/{address}/moves", s.listMoves)
	s.mux.HandleFunc("GET /v1/ledgers/{ledger}/balances", s.aggregateBalances)

	s.mux.HandleFunc("GET /v1/ledgers/{ledger}/logs", s.listLogs)

	s.mux.HandleFunc("GET /openapi.yaml", s.handleSpec)
	s.mux.HandleFunc("GET /docs", s.handleDocs)
	s.mux.HandleFunc("GET /", s.handleHome)
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}
