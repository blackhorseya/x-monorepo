package server

import (
	"fmt"
	"html"
	"net/http"
	"sort"
	"strings"
)

// Doc is one bounded context's OpenAPI document.
type Doc struct {
	Name string // e.g. "ordering"
	Spec []byte // YAML
}

// mountDocs serves /openapi/<name>.yaml for each doc and an index at /docs
// (Scalar UI loaded from CDN). Only call this when cfg.HTTP.Docs is true —
// never in production.
func mountDocs(mux *http.ServeMux, docs []Doc) {
	names := make([]string, 0, len(docs))
	for _, d := range docs {
		d := d
		names = append(names, d.Name)
		mux.HandleFunc("GET /openapi/"+d.Name+".yaml", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/yaml")
			_, _ = w.Write(d.Spec)
		})
		mux.HandleFunc("GET /docs/"+d.Name, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = fmt.Fprintf(w, scalarPage, html.EscapeString(d.Name), "/openapi/"+d.Name+".yaml")
		})
	}
	sort.Strings(names)

	mux.HandleFunc("GET /docs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		var b strings.Builder
		b.WriteString("<!doctype html><title>API docs</title><h1>API docs</h1><ul>")
		for _, n := range names {
			_, _ = fmt.Fprintf(&b, `<li><a href="/docs/%s">%s</a> (<a href="/openapi/%s.yaml">yaml</a>)</li>`, n, n, n)
		}
		b.WriteString("</ul>")
		_, _ = w.Write([]byte(b.String()))
	})
}

const scalarPage = `<!doctype html>
<html><head><meta charset="utf-8"><title>%s API</title></head>
<body>
<script id="api-reference" data-url="%s"></script>
<script src="https://cdn.jsdelivr.net/npm/@scalar/api-reference@1.25.0"></script>
</body></html>`
