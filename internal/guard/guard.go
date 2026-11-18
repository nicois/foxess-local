// Package guard implements the LAN-only exposure guard (spec §5.6).
// The operator surface binds to all interfaces but refuses non-local
// clients: a host accidentally in a DMZ or behind port-forwarding gets
// a self-explanatory 403 instead of a working dashboard. This is a
// misconfiguration guard, not a security boundary — the tokens stay
// the access control for clients that are local.
package guard

import (
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// localPrefixes are the client ranges served by default: loopback
// (same-host nginx / curl), RFC 1918, link-local, IPv6 ULA.
var localPrefixes = []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("fc00::/7"),
}

// Wrapper returns the guard middleware. allowPublic admits everything
// (informed exposure); extra are CIDRs beyond localPrefixes
// (e.g. Tailscale's 100.64.0.0/10).
func Wrapper(allowPublic bool, extra []netip.Prefix) func(http.Handler) http.Handler {
	prefixes := append(append(make([]netip.Prefix, 0, len(localPrefixes)+len(extra)), localPrefixes...), extra...)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host, _, _ := net.SplitHostPort(r.RemoteAddr) // "" when no port
			if host == "" {
				host = r.RemoteAddr
			}
			if !localClient(host, allowPublic, prefixes) {
				slog.Warn("guard: rejected non-local client", "remote", host)
				reject(w, strings.HasPrefix(r.URL.Path, "/api/"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// localClient reports whether host is inside the served ranges.
// Unparseable sources fail closed.
func localClient(host string, allowPublic bool, prefixes []netip.Prefix) bool {
	if allowPublic {
		return true
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	for _, p := range prefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// reject writes the 403 that explains itself: problem+json under
// /api, a plain page elsewhere.
func reject(w http.ResponseWriter, apiPath bool) {
	const msg = "foxesslocal serves clients on the local network only. This request came from a non-local address — see the [net] section of the config file."
	if apiPath {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"title":"Local network only","status":403,"code":"local_network_only","detail":"` + msg + `"}`))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte("<!doctype html><meta charset=utf-8><title>Local network only</title><h1>Local network only</h1><p>" + msg + "</p>"))
}
