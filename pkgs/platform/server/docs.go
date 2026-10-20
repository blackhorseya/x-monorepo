package server

import (
	"bytes"
	"fmt"
	"html"
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
)

// Doc is one bounded context's OpenAPI document.
type Doc struct {
	Name string // e.g. "ordering"
	Spec []byte // YAML
}

// Docs routes live under this prefix so an ingress can expose or block them
// as one group: /api/docs, /api/docs/<bc>, /api/openapi/<bc>.yaml.
const (
	DocsPath    = "/api/docs"
	OpenAPIPath = "/api/openapi"
)

// mountDocs serves OpenAPIPath/<name>.yaml for each doc and an index at
// DocsPath (Scalar UI loaded from CDN). Only call this when cfg.HTTP.Docs is
// true — never in production.
//
// The generated specs say `servers: - url: /api` (RPCPrefix): correct for
// any host, but Scalar's request sender does `new URL(server + path)` and a
// relative server fails it ("The URL seems to be invalid"). The YAML is
// therefore served with the server rewritten to the absolute origin of the
// request (scheme from X-Forwarded-Proto / TLS, host from the Host header),
// which also makes the file importable into Postman / Bruno as-is.
func mountDocs(engine *gin.Engine, docs []Doc) {
	names := make([]string, 0, len(docs))
	for _, d := range docs {
		names = append(names, d.Name)
		engine.GET(OpenAPIPath+"/"+d.Name+".yaml", func(c *gin.Context) {
			c.Data(http.StatusOK, "application/yaml", absoluteServers(d.Spec, origin(c.Request)))
		})
		engine.GET(DocsPath+"/"+d.Name, func(c *gin.Context) {
			page := fmt.Sprintf(scalarPage, html.EscapeString(d.Name), OpenAPIPath+"/"+d.Name+".yaml")
			c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(page))
		})
	}
	sort.Strings(names)

	engine.GET(DocsPath, func(c *gin.Context) {
		var b strings.Builder
		b.WriteString("<!doctype html><title>API docs</title><h1>API docs</h1><ul>")
		for _, n := range names {
			_, _ = fmt.Fprintf(&b, `<li><a href="%s/%s">%s</a> (<a href="%s/%s.yaml">yaml</a>)</li>`, DocsPath, n, n, OpenAPIPath, n)
		}
		b.WriteString("</ul>")
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(b.String()))
	})
}

// relativeServer is the servers entry protoc-gen-connect-openapi emits from
// every openapi.base.yaml.
var relativeServer = []byte("- url: " + RPCPrefix + "\n")

// absoluteServers rewrites the first relative server entry of spec to origin + RPCPrefix.
func absoluteServers(spec []byte, origin string) []byte {
	return bytes.Replace(spec, relativeServer, []byte("- url: "+origin+RPCPrefix+"\n"), 1)
}

// origin is the scheme://host the client used, honouring a proxy's X-Forwarded-Proto.
func origin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if fwd := r.Header.Get("X-Forwarded-Proto"); fwd == "http" || fwd == "https" {
		scheme = fwd
	}

	return scheme + "://" + r.Host
}

const scalarPage = `<!doctype html>
<html><head><meta charset="utf-8"><title>%s API</title></head>
<body>
<script id="api-reference" data-url="%s"></script>
<script src="https://cdn.jsdelivr.net/npm/@scalar/api-reference@1.25.0"></script>
</body></html>`
