// Package api serves the ledger over http.
//
// routing is stdlib ServeMux, which handles method and path patterns natively
// since go 1.22. the types in gen.go come from api/openapi.yaml, so the
// contract is written first and the shapes follow from it.
package api

import (
	"net/http"

	"github.com/pixperk/giro/internal/storage"
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

func (s *Server) routes() {
	s.mux.HandleFunc("GET /openapi.yaml", s.handleSpec)
	s.mux.HandleFunc("GET /docs", s.handleDocs)
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}
