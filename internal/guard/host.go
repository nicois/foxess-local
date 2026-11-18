package guard

import (
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// localSuffixes are name suffixes that public DNS does not resolve (mDNS,
// RFC 8375 home.arpa, and the usual home-router domains): a rebinding
// attacker cannot serve them.
var localSuffixes = []string{".local", ".home.arpa", ".lan", ".internal"}

// HostCheck refuses requests whose Host header does not name this
// machine (audit S6, DNS rebinding): a hostile web page that points its
// own domain at the gateway's address arrives with that domain in Host.
// Accepted: IP literals, localhost, the machine's own name (hostname),
// names under a local-only suffix, and the configured extra names.
// Others get 421 Misdirected Request before any handler runs.
func HostCheck(hostname string, extra []string) func(http.Handler) http.Handler {
	names := map[string]bool{"localhost": true}
	if hostname != "" {
		names[strings.ToLower(hostname)] = true
	}
	for _, n := range extra {
		names[strings.ToLower(strings.TrimSuffix(n, "."))] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !hostAllowed(r.Host, names) {
				slog.Warn("guard: refused a request for another host name", "host", r.Host, "remote", r.RemoteAddr)
				http.Error(w, "foxesslocal answers only to its own address or name (Host "+r.Host+
					" is not one; add it to [net] host_names in the config file)", http.StatusMisdirectedRequest)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// hostAllowed applies HostCheck's rule to one Host header value.
func hostAllowed(hostport string, names map[string]bool) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
	if host == "" {
		return false
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	if names[host] {
		return true
	}
	for _, s := range localSuffixes {
		if strings.HasSuffix(host, s) {
			return true
		}
	}
	return false
}
