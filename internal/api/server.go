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
	mux *http.ServeMux
	// resolves a ledger name to a store scoped to it. the name is never a
	// parameter to a query, only an input to constructing the store.
	store func(ledger string) *storage.Store
}

func NewServer(store func(ledger string) *storage.Store) *Server {
	s := &Server{mux: http.NewServeMux(), store: store}
	s.routes()
	return s
}

// the patterns match the paths in api/openapi.yaml exactly. go 1.22 ServeMux
// uses the same {name} wildcard syntax openapi does, so the two are directly
// comparable, which is what routes_test.go checks.
func (s *Server) routes() {
	s.mux.HandleFunc("POST /v1/ledgers/{ledger}", s.createLedger)
	s.mux.HandleFunc("GET /v1/ledgers/{ledger}", s.getLedger)

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
