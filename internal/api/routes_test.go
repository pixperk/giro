package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	giro "github.com/pixperk/giro"
)

// Generating models only means the compiler no longer checks that a handler
// exists for every operation in the contract. This does instead.
//
// Without it, adding a path to openapi.yaml and forgetting the route would
// ship a documented endpoint that 404s, and the docs page would advertise it.

func TestEveryDocumentedPathIsRouted(t *testing.T) {
	s := newTestServer(t)

	operations := specOperations(t)
	if len(operations) < 14 {
		t.Fatalf("only found %d operations in the spec, the scanner is probably broken", len(operations))
	}

	for _, op := range operations {
		t.Run(op.method+" "+op.path, func(t *testing.T) {
			req := httptest.NewRequest(op.method, fillWildcards(op.path), nil)

			// asking the mux directly rather than looking at a status code:
			// a handler is free to return 404 for a missing row, and that is
			// a different thing from having no route at all.
			_, pattern := s.mux.Handler(req)
			if pattern == "" {
				t.Errorf("%s %s is in the contract but has no route", op.method, op.path)
			}
		})
	}
}

// the reverse direction: a route that nothing documents.
func TestEveryRouteIsDocumented(t *testing.T) {
	documented := map[string]bool{}
	for _, op := range specOperations(t) {
		documented[op.method+" "+op.path] = true
	}

	// served by the binary rather than described by the contract
	for _, undocumented := range []string{"GET /openapi.yaml", "GET /docs", "GET /healthz"} {
		documented[undocumented] = true
	}

	for _, route := range registeredRoutes() {
		if !documented[route] {
			t.Errorf("%s is routed but absent from the contract", route)
		}
	}
}

type operation struct{ method, path string }

// scans the paths out of the embedded contract.
//
// a yaml parser would be tidier, but go has none in the standard library and
// this is one file whose shape we control. the length assertion above is what
// stops a silent miss if the layout ever changes.
func specOperations(t *testing.T) []operation {
	t.Helper()

	var ops []operation
	var currentPath string
	inPaths := false

	for _, line := range strings.Split(string(giro.OpenAPISpec), "\n") {
		switch {
		case line == "paths:":
			inPaths = true
		case inPaths && len(line) > 0 && line[0] != ' ':
			inPaths = false // a new top level key ends the section
		case inPaths && strings.HasPrefix(line, "  /") && strings.HasSuffix(strings.TrimRight(line, " "), ":"):
			currentPath = strings.TrimSuffix(strings.TrimSpace(line), ":")
		case inPaths && currentPath != "":
			trimmed := strings.TrimSpace(line)
			for _, method := range []string{"get:", "post:", "put:", "patch:", "delete:"} {
				if trimmed == method && strings.HasPrefix(line, "    ") && !strings.HasPrefix(line, "      ") {
					ops = append(ops, operation{
						method: strings.ToUpper(strings.TrimSuffix(method, ":")),
						path:   currentPath,
					})
				}
			}
		}
	}
	return ops
}

// the mux does not enumerate its patterns, so this mirrors routes(). the two
// tests above are what keep it honest in both directions.
func registeredRoutes() []string {
	return []string{
		"POST /v1/ledgers/{ledger}",
		"GET /v1/ledgers/{ledger}",
		"POST /v1/ledgers/{ledger}/transactions",
		"GET /v1/ledgers/{ledger}/transactions",
		"GET /v1/ledgers/{ledger}/transactions/{id}",
		"POST /v1/ledgers/{ledger}/transactions/{id}/revert",
		"POST /v1/ledgers/{ledger}/transactions/{id}/metadata",
		"DELETE /v1/ledgers/{ledger}/transactions/{id}/metadata/{key}",
		"GET /v1/ledgers/{ledger}/accounts/{address}",
		"POST /v1/ledgers/{ledger}/accounts/{address}/metadata",
		"DELETE /v1/ledgers/{ledger}/accounts/{address}/metadata/{key}",
		"GET /v1/ledgers/{ledger}/accounts/{address}/balances",
		"GET /v1/ledgers/{ledger}/balances",
		"GET /v1/ledgers/{ledger}/logs",
		"GET /openapi.yaml",
		"GET /docs",
		"GET /healthz",
	}
}

// every entry in registeredRoutes must actually be registered, otherwise the
// list above could drift into fiction and still pass the test that reads it.
func TestRegisteredRoutesListIsAccurate(t *testing.T) {
	s := newTestServer(t)

	for _, route := range registeredRoutes() {
		method, path, _ := strings.Cut(route, " ")
		req := httptest.NewRequest(method, fillWildcards(path), nil)
		if _, pattern := s.mux.Handler(req); pattern == "" {
			t.Errorf("%s is listed as registered but the mux does not have it", route)
		}
	}
}

func fillWildcards(path string) string {
	r := strings.NewReplacer(
		"{ledger}", "demo",
		"{address}", "users:alice",
		"{id}", "1",
		"{key}", "orderId",
	)
	return r.Replace(path)
}

func TestDocsAndSpecAreServed(t *testing.T) {
	s := newTestServer(t)

	spec := do(t, s, http.MethodGet, "/openapi.yaml", nil)
	if spec.Code != http.StatusOK {
		t.Fatalf("spec: %d", spec.Code)
	}
	if got := spec.Header().Get("Content-Type"); got != "application/yaml" {
		t.Errorf("content type %q", got)
	}
	if spec.Body.String() != string(giro.OpenAPISpec) {
		t.Error("the served spec is not byte identical to the embedded one")
	}

	docs := do(t, s, http.MethodGet, "/docs", nil)
	if docs.Code != http.StatusOK {
		t.Fatalf("docs: %d", docs.Code)
	}
	// the page is useless if it points at the wrong document
	if !strings.Contains(docs.Body.String(), "/openapi.yaml") {
		t.Error("the docs page does not reference the spec")
	}

	health := do(t, s, http.MethodGet, "/healthz", nil)
	if health.Code != http.StatusOK {
		t.Errorf("healthz: %d", health.Code)
	}
}
