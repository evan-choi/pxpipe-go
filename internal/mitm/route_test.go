package mitm

import (
	"net/url"
	"testing"
)

func TestRouteMatching(t *testing.T) {
	upstream, _ := url.Parse("http://127.0.0.1:1234/base")
	routes := []Route{{Host: "example.com", PathPrefix: "/v1/messages", Upstream: upstream}}

	if !hostCouldMatch(routes, "EXAMPLE.com:443") {
		t.Fatal("host without a configured port should match every port")
	}
	if matchingRoute(routes, "example.com:443", "/v1/other") != nil {
		t.Fatal("non-matching path was routed")
	}
	route := matchingRoute(routes, "example.com:443", "/v1/messages/count_tokens")
	if route == nil {
		t.Fatal("matching path was not routed")
	}
	requestURL, _ := url.Parse("https://example.com/v1/messages?stream=true")
	got := routeURL(route, requestURL).String()
	if got != "http://127.0.0.1:1234/base/v1/messages?stream=true" {
		t.Fatalf("rewritten URL = %q", got)
	}
}

func TestPortSpecificRoute(t *testing.T) {
	upstream, _ := url.Parse("http://127.0.0.1:1234")
	routes := []Route{{Host: "example.com:8443", PathPrefix: "/", Upstream: upstream}}
	if hostCouldMatch(routes, "example.com:443") {
		t.Fatal("port-specific route matched another port")
	}
	if !hostCouldMatch(routes, "example.com:8443") {
		t.Fatal("port-specific route did not match")
	}
}

func TestValidateRoutes(t *testing.T) {
	httpUpstream, _ := url.Parse("http://localhost")
	invalidUpstream, _ := url.Parse("ftp://localhost")
	tests := []Route{
		{PathPrefix: "/", Upstream: httpUpstream},
		{Host: "https://example.com", PathPrefix: "/", Upstream: httpUpstream},
		{Host: "example.com", PathPrefix: "v1", Upstream: httpUpstream},
		{Host: "example.com", PathPrefix: "/", Upstream: invalidUpstream},
	}
	for i, route := range tests {
		if err := validateRoutes([]Route{route}); err == nil {
			t.Errorf("case %d: expected validation error", i)
		}
	}
}
