package server

import (
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

// mountDocs serves /openapi/<name>.yaml for each doc and an index at /docs
// (Scalar UI loaded from CDN). Only call this when cfg.HTTP.Docs is true —
// never in production.
func mountDocs(engine *gin.Engine, docs []Doc) {
	names := make([]string, 0, len(docs))
	for _, d := range docs {
		names = append(names, d.Name)
		engine.GET("/openapi/"+d.Name+".yaml", func(c *gin.Context) {
			c.Data(http.StatusOK, "application/yaml", d.Spec)
		})
		engine.GET("/docs/"+d.Name, func(c *gin.Context) {
			page := fmt.Sprintf(scalarPage, html.EscapeString(d.Name), "/openapi/"+d.Name+".yaml")
			c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(page))
		})
	}
	sort.Strings(names)

	engine.GET("/docs", func(c *gin.Context) {
		var b strings.Builder
		b.WriteString("<!doctype html><title>API docs</title><h1>API docs</h1><ul>")
		for _, n := range names {
			_, _ = fmt.Fprintf(&b, `<li><a href="/docs/%s">%s</a> (<a href="/openapi/%s.yaml">yaml</a>)</li>`, n, n, n)
		}
		b.WriteString("</ul>")
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(b.String()))
	})
}

const scalarPage = `<!doctype html>
<html><head><meta charset="utf-8"><title>%s API</title></head>
<body>
<script id="api-reference" data-url="%s"></script>
<script src="https://cdn.jsdelivr.net/npm/@scalar/api-reference@1.25.0"></script>
</body></html>`
