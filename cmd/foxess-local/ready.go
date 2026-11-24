package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"

	"foxesslocal/internal/api"
	"foxesslocal/internal/diskspace"
)

// tcpListen is /proc/net/tcp's LISTEN state.
const tcpListen = "0A"

// listening reports whether a local TCP socket listens on port (any
// address, IPv4 or IPv6). prox exposes no listener-ready signal, so
// readiness asks the kernel; it needs no privilege.
func listening(port int) bool {
	var rs []io.Reader
	for _, p := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		f, err := os.Open(p) //nolint:gosec // G304: the two fixed /proc paths
		if err != nil {
			continue
		}
		defer func() { _ = f.Close() }()
		rs = append(rs, f)
	}
	return listeningIn(port, rs...)
}

// listeningIn scans /proc/net/tcp{,6}-format tables for a LISTEN socket
// on port (local_address is HEXADDR:HEXPORT).
func listeningIn(port int, tables ...io.Reader) bool {
	for _, r := range tables {
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			f := strings.Fields(sc.Text())
			if len(f) < 4 || f[3] != tcpListen {
				continue
			}
			i := strings.LastIndexByte(f[1], ':')
			if i < 0 {
				continue
			}
			if p, err := strconv.ParseUint(f[1][i+1:], 16, 16); err == nil && int(p) == port {
				return true
			}
		}
	}
	return false
}

// listenerCheck is the readiness line for the inverter listener.
func listenerCheck(addr string, isListening func(port int) bool) api.Check {
	c := api.Check{ID: "listener", Title: "Inverter listener"}
	_, ps, err := net.SplitHostPort(addr)
	port, perr := strconv.Atoi(ps)
	if err != nil || perr != nil {
		c.Status, c.Detail = api.CheckFail, fmt.Sprintf("the listen address %q is not host:port", addr)
		c.Fix = "Fix `[server] inverter_listen` in /etc/foxess-local/config.toml."
		return c
	}
	if isListening(port) {
		c.Status, c.Detail = api.CheckOK, fmt.Sprintf("listening on port %d", port)
		return c
	}
	c.Status = api.CheckFail
	c.Detail = fmt.Sprintf("nothing listens on port %d: the inverter cannot connect", port)
	c.Fix = "After a restart the port can stay held for up to 2 minutes; if it persists, see `journalctl -u foxess-local`."
	return c
}

// diskCheck is the readiness line for the store's filesystem: a warning
// below the disk guard's critical level (it is discarding old raw data).
func diskCheck(free, total, minFree uint64, err error) api.Check {
	c := api.Check{ID: "disk", Title: "Disk space"}
	if err != nil {
		c.Status, c.Detail = api.CheckWarn, "free space unknown: "+err.Error()
		return c
	}
	crit := diskspace.Threshold(minFree, total)
	if free < crit {
		c.Status = api.CheckWarn
		c.Detail = fmt.Sprintf("%d MB free, below %d MB: the oldest raw data is being discarded", free/diskspace.MiB, crit/diskspace.MiB)
		c.Fix = "Free space on this filesystem, or shorten `[retention] raw_hours`."
		return c
	}
	c.Status, c.Detail = api.CheckOK, fmt.Sprintf("%d MB free", free/diskspace.MiB)
	return c
}

// readyChecks are the host's readiness checks (api.WithReadyChecks).
func (h *handle) readyChecks() []api.Check {
	free, total, err := diskspace.Free(h.storeDir())
	gate := gateStatusCheck(gateStatusPath, h.clk())
	if noInverter(gateEnvPath) {
		gate = gateIdleCheck()
	}
	return []api.Check{
		listenerCheck(h.cfg.Server.InverterListen, listening),
		diskCheck(free, total, minFreeBytes(h.cfg), err),
		gate,
	}
}
