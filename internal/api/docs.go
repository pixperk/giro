package api

import (
	"net/http"

	"github.com/pixperk/giro"
)

// a single page that renders the spec and can call the api. loaded from a cdn
// rather than vendored, because the bundle is megabytes and this page is for
// developers rather than for the service to function.
//
// if the service ever runs somewhere without egress, vendor the script and
// serve it from here instead.
const docsHTML = `<!doctype html>
<html>
  <head>
    <title>giro API</title>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <style>body { margin: 0 }</style>
  </head>
  <body>
    <div id="app"></div>
    <script src="https://cdn.jsdelivr.net/npm/@scalar/api-reference"></script>
    <script>
      Scalar.createApiReference('#app', {
        url: '/openapi.yaml',
        theme: 'default',
      })
    </script>
  </body>
</html>
`

func (s *Server) handleSpec(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(giro.OpenAPISpec)
}

func (s *Server) handleDocs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(docsHTML))
}
