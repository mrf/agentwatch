// Package hostcheck validates the Host and Origin headers of requests to the
// agentwatch transports. It is shared by transport/httpapi and
// transport/wsapi.
//
// Host validation is the DNS-rebinding defense: a page on evil.example whose
// DNS later resolves to 127.0.0.1 reaches a loopback listener with
// Host: evil.example, so only loopback names and explicitly configured hosts
// are accepted.
package hostcheck

import (
	"net"
	"net/url"
	"strings"
)

// loopbackHosts are always accepted, on any port.
var loopbackHosts = []string{"localhost", "127.0.0.1", "::1"}

// HostAllowed reports whether a request Host header (host or host:port)
// names this server. Loopback names are accepted on any port. Each entry in
// allowed is either a hostname, which matches on any port, or a host:port,
// which matches exactly. Matching is case-insensitive; IPv6 entries may be
// written with or without brackets.
func HostAllowed(host string, allowed []string) bool {
	name, port := splitHost(host)
	if name == "" {
		return false
	}
	for i := 0; i < len(loopbackHosts); i++ {
		if name == loopbackHosts[i] {
			return true
		}
	}
	for i := 0; i < len(allowed); i++ {
		allowedName, allowedPort := splitHost(allowed[i])
		if allowedName != name {
			continue
		}
		if allowedPort == "" || allowedPort == port {
			return true
		}
	}
	return false
}

// OriginAllowed reports whether a request's Origin header is acceptable.
//
// With an empty allowlist, a missing Origin (non-browser client) is accepted,
// as is an http(s) origin that is either same-origin with requestHost or a
// loopback host on any port (local development). requestHost must already
// have passed HostAllowed for the same-origin case to be meaningful.
//
// With an allowlist, Origin must be present and match an entry on scheme,
// host, and port; default ports (80 for http, 443 for https) are equivalent
// to an omitted port.
func OriginAllowed(origin, requestHost string, allowed []string) bool {
	if len(allowed) > 0 {
		return originInAllowlist(origin, allowed)
	}
	if origin == "" {
		return true
	}
	scheme, host, ok := parseOrigin(origin)
	if !ok || (scheme != "http" && scheme != "https") {
		return false
	}
	if host == normalizeHostPort(scheme, requestHost) {
		return true
	}
	return HostAllowed(host, nil)
}

func originInAllowlist(origin string, allowed []string) bool {
	scheme, host, ok := parseOrigin(origin)
	if !ok {
		return false
	}
	for i := 0; i < len(allowed); i++ {
		allowedScheme, allowedHost, ok := parseOrigin(allowed[i])
		if !ok {
			continue
		}
		if scheme == allowedScheme && host == allowedHost {
			return true
		}
	}
	return false
}

// parseOrigin returns the lower-cased scheme and host[:port] of an origin,
// with any default port removed.
func parseOrigin(origin string) (scheme, host string, ok bool) {
	if origin == "" {
		return "", "", false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", "", false
	}
	scheme = strings.ToLower(u.Scheme)
	return scheme, normalizeHostPort(scheme, u.Host), true
}

// normalizeHostPort lower-cases host and drops the scheme's default port.
func normalizeHostPort(scheme, host string) string {
	host = strings.ToLower(host)
	switch {
	case scheme == "http" && strings.HasSuffix(host, ":80"):
		return strings.TrimSuffix(host, ":80")
	case scheme == "https" && strings.HasSuffix(host, ":443"):
		return strings.TrimSuffix(host, ":443")
	}
	return host
}

// splitHost splits host or host:port into a lower-cased, unbracketed name
// and a port (empty when absent).
func splitHost(hostport string) (name, port string) {
	if h, p, err := net.SplitHostPort(hostport); err == nil {
		return strings.ToLower(h), p
	}
	return strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(hostport, "["), "]")), ""
}
