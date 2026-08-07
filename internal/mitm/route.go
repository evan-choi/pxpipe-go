package mitm

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Route redirects matching requests to an upstream. Host "*" matches every
// authority; otherwise a host without a port matches that host on every port.
// Exactly one of PathPrefix or PathSuffix must be set.
type Route struct {
	Host       string
	PathPrefix string
	PathSuffix string
	Upstream   *url.URL
}

func validateRoutes(routes []Route) error {
	for i, route := range routes {
		if strings.TrimSpace(route.Host) == "" {
			return fmt.Errorf("route %d: host is required", i)
		}
		if strings.Contains(route.Host, "://") || strings.Contains(route.Host, "/") {
			return fmt.Errorf("route %d: host must be an authority", i)
		}
		if (route.PathPrefix == "") == (route.PathSuffix == "") {
			return fmt.Errorf("route %d: exactly one path prefix or suffix is required", i)
		}
		if route.PathPrefix != "" && !strings.HasPrefix(route.PathPrefix, "/") {
			return fmt.Errorf("route %d: path prefix must start with /", i)
		}
		if route.PathSuffix != "" && !strings.HasPrefix(route.PathSuffix, "/") {
			return fmt.Errorf("route %d: path suffix must start with /", i)
		}
		if route.Upstream == nil || route.Upstream.Host == "" ||
			(route.Upstream.Scheme != "http" && route.Upstream.Scheme != "https") {
			return fmt.Errorf("route %d: upstream must be an HTTP(S) URL", i)
		}
		if route.Upstream.RawQuery != "" || route.Upstream.Fragment != "" {
			return fmt.Errorf("route %d: upstream must not contain a query or fragment", i)
		}
	}
	return nil
}

func matchingRoute(routes []Route, authority, path string) *Route {
	for i := range routes {
		if routeMatchesHost(routes[i].Host, authority) && routeMatchesPath(routes[i], path) {
			return &routes[i]
		}
	}
	return nil
}

func hostCouldMatch(routes []Route, authority string) bool {
	for _, route := range routes {
		if routeMatchesHost(route.Host, authority) {
			return true
		}
	}
	return false
}

func routeMatchesHost(pattern, authority string) bool {
	if pattern == "*" {
		return true
	}
	patternHost, patternPort := splitAuthority(pattern)
	host, port := splitAuthority(authority)
	if !strings.EqualFold(patternHost, host) {
		return false
	}
	return patternPort == "" || patternPort == port
}

func routeMatchesPath(route Route, path string) bool {
	if route.PathPrefix != "" {
		return strings.HasPrefix(path, route.PathPrefix)
	}
	return strings.HasSuffix(path, route.PathSuffix)
}

func splitAuthority(authority string) (string, string) {
	authority = strings.TrimSpace(authority)
	if host, port, err := net.SplitHostPort(authority); err == nil {
		return strings.Trim(host, "[]"), port
	}
	return strings.Trim(authority, "[]"), ""
}

func routeURL(route *Route, requestURL *url.URL) *url.URL {
	target := *route.Upstream
	target.Path = joinPath(target.Path, requestURL.Path)
	target.RawPath = ""
	target.RawQuery = requestURL.RawQuery
	return &target
}

func joinPath(base, path string) string {
	switch {
	case base == "":
		return path
	case path == "":
		return base
	case strings.HasSuffix(base, "/") && strings.HasPrefix(path, "/"):
		return base + path[1:]
	case !strings.HasSuffix(base, "/") && !strings.HasPrefix(path, "/"):
		return base + "/" + path
	default:
		return base + path
	}
}
