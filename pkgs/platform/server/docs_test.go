package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// The served YAML carries an absolute server (Scalar's request sender needs
// one); the embedded spec keeps its relative /api.
func TestDocs_AbsoluteServers(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	spec := []byte("openapi: 3.1.0\nservers:\n  - url: /api\npaths: {}\n")
	engine := gin.New()
	mountDocs(engine, []Doc{{Name: "ordering", Spec: spec}})

	get := func(host, proto string) string {
		req := httptest.NewRequest("GET", OpenAPIPath+"/ordering.yaml", nil)
		req.Host = host
		if proto != "" {
			req.Header.Set("X-Forwarded-Proto", proto)
		}
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("GET = %d", rec.Code)
		}
		return rec.Body.String()
	}
	if body := get("localhost:8080", ""); !strings.Contains(body, "  - url: http://localhost:8080/api\n") {
		t.Fatalf("plain: %q", body)
	}
	if body := get("api.example.com", "https"); !strings.Contains(body, "  - url: https://api.example.com/api\n") {
		t.Fatalf("behind tls proxy: %q", body)
	}
	if body := get("h", "ftp"); !strings.Contains(body, "  - url: http://h/api\n") {
		t.Fatalf("bogus forwarded proto must be ignored: %q", body)
	}
	if string(spec) != "openapi: 3.1.0\nservers:\n  - url: /api\npaths: {}\n" {
		t.Fatal("embedded spec was mutated")
	}
}
