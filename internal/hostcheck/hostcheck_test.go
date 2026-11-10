package hostcheck_test

import (
	"testing"

	"github.com/mrf/agentwatch/internal/hostcheck"
)

func TestHostAllowed(t *testing.T) {
	tests := []struct {
		name    string
		host    string
		allowed []string
		want    bool
	}{
		{name: "localhost no port", host: "localhost", want: true},
		{name: "localhost with port", host: "localhost:8080", want: true},
		{name: "localhost mixed case", host: "LocalHost:8080", want: true},
		{name: "ipv4 loopback", host: "127.0.0.1:9000", want: true},
		{name: "ipv6 loopback bracketed with port", host: "[::1]:9000", want: true},
		{name: "ipv6 loopback bracketed no port", host: "[::1]", want: true},
		{name: "empty host rejected", host: "", want: false},
		{name: "rebinding hostname rejected", host: "evil.example", want: false},
		{name: "rebinding hostname with port rejected", host: "evil.example:8080", want: false},
		{name: "localhost suffix rejected", host: "localhost.evil.example", want: false},
		{name: "loopback prefix rejected", host: "127.0.0.1.evil.example", want: false},
		{name: "other loopback address rejected", host: "127.0.0.2:8080", want: false},
		{name: "lan address rejected by default", host: "192.168.1.5:8080", want: false},
		{name: "configured host any port", host: "agentwatch.lan:8080", allowed: []string{"agentwatch.lan"}, want: true},
		{name: "configured host case-insensitive", host: "AgentWatch.LAN", allowed: []string{"agentwatch.lan"}, want: true},
		{name: "configured host with port exact", host: "agentwatch.lan:8080", allowed: []string{"agentwatch.lan:8080"}, want: true},
		{name: "configured host with port mismatch", host: "agentwatch.lan:9090", allowed: []string{"agentwatch.lan:8080"}, want: false},
		{name: "configured ipv6 bracketed", host: "[fd00::1]:8080", allowed: []string{"[fd00::1]"}, want: true},
		{name: "configured ipv6 bare", host: "[fd00::1]:8080", allowed: []string{"fd00::1"}, want: true},
		{name: "configured hosts keep loopback defaults", host: "localhost:8080", allowed: []string{"agentwatch.lan"}, want: true},
		{name: "unconfigured host still rejected", host: "evil.example", allowed: []string{"agentwatch.lan"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hostcheck.HostAllowed(tt.host, tt.allowed); got != tt.want {
				t.Fatalf("HostAllowed(%q, %v) = %v, want %v", tt.host, tt.allowed, got, tt.want)
			}
		})
	}
}

func TestOriginAllowed_Default(t *testing.T) {
	tests := []struct {
		name        string
		origin      string
		requestHost string
		want        bool
	}{
		{name: "no origin allowed for non-browser clients", origin: "", requestHost: "127.0.0.1:8080", want: true},
		{name: "same origin", origin: "http://127.0.0.1:8080", requestHost: "127.0.0.1:8080", want: true},
		{name: "localhost dev origin other port", origin: "http://localhost:3000", requestHost: "127.0.0.1:8080", want: true},
		{name: "ipv6 loopback origin", origin: "http://[::1]:3000", requestHost: "127.0.0.1:8080", want: true},
		{name: "remote origin rejected", origin: "http://evil.example", requestHost: "127.0.0.1:8080", want: false},
		{name: "opaque null origin rejected", origin: "null", requestHost: "127.0.0.1:8080", want: false},
		{name: "unparseable origin rejected", origin: "http://%zz", requestHost: "127.0.0.1:8080", want: false},
		{name: "non-http scheme rejected", origin: "file://localhost", requestHost: "127.0.0.1:8080", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hostcheck.OriginAllowed(tt.origin, tt.requestHost, nil); got != tt.want {
				t.Fatalf("OriginAllowed(%q, %q, nil) = %v, want %v", tt.origin, tt.requestHost, got, tt.want)
			}
		})
	}
}

func TestOriginAllowed_Allowlist(t *testing.T) {
	allowed := []string{"https://app.example.com", "http://dev.example.com:3000"}
	tests := []struct {
		name   string
		origin string
		want   bool
	}{
		{name: "exact match", origin: "https://app.example.com", want: true},
		{name: "case-insensitive", origin: "HTTPS://App.Example.com", want: true},
		{name: "explicit default port equals implicit", origin: "https://app.example.com:443", want: true},
		{name: "scheme mismatch rejected", origin: "http://app.example.com", want: false},
		{name: "port mismatch rejected", origin: "https://app.example.com:8443", want: false},
		{name: "explicit port match", origin: "http://dev.example.com:3000", want: true},
		{name: "explicit port mismatch", origin: "http://dev.example.com:3001", want: false},
		{name: "missing port when allowlist has one", origin: "http://dev.example.com", want: false},
		{name: "unlisted host rejected", origin: "https://evil.example", want: false},
		{name: "missing origin rejected", origin: "", want: false},
		{name: "localhost not implicitly allowed", origin: "http://localhost:3000", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hostcheck.OriginAllowed(tt.origin, "127.0.0.1:8080", allowed); got != tt.want {
				t.Fatalf("OriginAllowed(%q, allowlist) = %v, want %v", tt.origin, got, tt.want)
			}
		})
	}
}
