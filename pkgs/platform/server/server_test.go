package server

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestUntracedByGin(t *testing.T) {
	filter := untracedByGin([]string{"/ordering.v1.OrderService/"})
	cases := map[string]bool{
		"/ordering.v1.OrderService/CreateOrder": false, // otelconnect's job
		"/api/healthz":                          false,
		"/api/metrics":                          false,
		"/healthz":                              true, // not a route any more: 404, but traced like any unknown path
		"/api/docs":                             true,
		"/api/openapi/ordering.yaml":            true,
		"/nope":                                 true,
	}
	for path, want := range cases {
		c := &gin.Context{Request: httptest.NewRequest("GET", path, nil)}
		if got := filter(c); got != want {
			t.Errorf("%s: traced = %v, want %v", path, got, want)
		}
	}
}
