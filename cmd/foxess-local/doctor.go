package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"foxesslocal/internal/api"
	"foxesslocal/internal/config"
)

// applyGate is the installed gate script (its show mode is read-only).
const applyGate = "/usr/local/lib/foxess-local/apply-gate.sh"

// doctorTimeout bounds each HTTP request doctor makes; doctorMaxBody
// bounds a response it reads.
const (
	doctorTimeout = 5 * time.Second
	doctorMaxBody = 1 << 20
)

// unitActive is systemd's ActiveState of a running (or exited
// RemainAfterExit) unit.
const unitActive = "active"

// doctorEnv is what doctor reads: the config file, commands (systemctl,
// the gate script), the app's HTTP surface, and whether it runs as root.
type doctorEnv struct {
	cfgPath string
	url     string // the app's base URL; "" = from the config's http_listen
	token   string // FOXESS_TOKEN: the readiness details need one
	root    bool
	// run runs a command: its output, exit code, and an error when it
	// could not run at all.
	run func(name string, args ...string) (string, int, error)
	// get fetches url with an optional bearer token.
	get func(url, token string) (int, []byte, error)
}

// doctorCmd is `foxess-local doctor` (F3, audit U4): one command that
// checks the install end to end — config, services, the gate, and the
// app's own readiness — and says how to fix what fails. It changes
// nothing. Exit 1 when a check fails.
func doctorCmd(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	cfgPath := fs.String("config", config.DefaultPath(), "config file")
	url := fs.String("url", "", "the app's base URL (default: from the config's http_listen)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	client := &http.Client{Timeout: doctorTimeout}
	return doctor(doctorEnv{
		cfgPath: *cfgPath,
		url:     *url,
		token:   os.Getenv("FOXESS_TOKEN"),
		root:    os.Geteuid() == 0,
		run: func(name string, args ...string) (string, int, error) {
			out, err := exec.Command(name, args...).CombinedOutput() //nolint:gosec // G204: fixed commands and unit names
			if ee, ok := errors.AsType[*exec.ExitError](err); ok {
				return string(out), ee.ExitCode(), nil
			}
			return string(out), 0, err
		},
		get: func(url, token string) (int, []byte, error) {
			req, err := http.NewRequest(http.MethodGet, url, nil) //nolint:noctx // bounded by the client timeout
			if err != nil {
				return 0, nil, err
			}
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			resp, err := client.Do(req)
			if err != nil {
				return 0, nil, err
			}
			defer func() { _ = resp.Body.Close() }()
			b, err := io.ReadAll(io.LimitReader(resp.Body, doctorMaxBody))
			return resp.StatusCode, b, err
		},
	}, os.Stdout)
}

// doctor runs every check, prints them, and returns the exit code.
func doctor(env doctorEnv, out io.Writer) int {
	var w strings.Builder
	defer func() { _, _ = io.WriteString(out, w.String()) }()
	fmt.Fprintf(&w, "foxess-local doctor %s\n\n", appVersion())
	cfg, cfgCheck := doctorConfig(env.cfgPath)
	checks := []api.Check{cfgCheck}
	checks = append(checks, doctorServices(env)...)
	checks = append(checks, doctorGateRules(env))
	base := env.url
	if base == "" {
		base = appURL(cfg)
	}
	checks = append(checks, doctorApp(env, base)...)
	failed := false
	for _, c := range checks {
		printCheck(&w, c)
		failed = failed || c.Status == api.CheckFail
	}
	if failed {
		w.WriteString("\nSome checks fail: see each fix above.\n")
		return 1
	}
	w.WriteString("\nNo failures.\n")
	return 0
}

var statusWord = map[string]string{api.CheckOK: "ok  ", api.CheckPending: "wait", api.CheckWarn: "WARN", api.CheckFail: "FAIL"}

func printCheck(w *strings.Builder, c api.Check) {
	line := fmt.Sprintf("  %s  %s", statusWord[c.Status], c.Title)
	if c.Detail != "" {
		line += " — " + c.Detail
	}
	w.WriteString(line + "\n")
	if c.Fix != "" {
		w.WriteString("        fix: " + c.Fix + "\n")
	}
}

// doctorConfig loads the config (the defaults stand in when it fails).
func doctorConfig(path string) (*config.Config, api.Check) {
	c := api.Check{ID: "config", Title: "Configuration"}
	cfg, err := config.Load(path)
	if err != nil {
		c.Status, c.Detail = api.CheckFail, err.Error()
		c.Fix = "Correct the file, then check it with `foxess-local config -config " + path + "`."
		return nil, c
	}
	c.Status, c.Detail = api.CheckOK, path
	if n := len(cfg.Warnings); n > 0 {
		c.Status, c.Detail = api.CheckWarn, fmt.Sprintf("%s: %s", path, strings.Join(cfg.Warnings, "; "))
		c.Fix = "Remove or rename the keys named."
	}
	return cfg, c
}

// appURL is the app's base URL from http_listen (a wildcard address is
// reached over loopback).
func appURL(cfg *config.Config) string {
	listen := config.DefaultHTTPListen
	if cfg != nil && cfg.Server.HTTPListen != "" {
		listen = cfg.Server.HTTPListen
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://" + listen
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// unitProps is `systemctl show <unit>`'s Key=Value lines (nil when
// systemctl is unavailable).
func unitProps(env doctorEnv, unit string) map[string]string {
	out, _, err := env.run("systemctl", "show", unit, "--no-pager")
	if err != nil {
		return nil
	}
	props := map[string]string{}
	for l := range strings.SplitSeq(out, "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			props[k] = v
		}
	}
	return props
}

// doctorServices checks the app and gate units, and the last gate check.
func doctorServices(env doctorEnv) []api.Check {
	app := api.Check{ID: "service", Title: "Service (foxess-local)"}
	p := unitProps(env, "foxess-local.service")
	if p == nil || p["LoadState"] == "not-found" {
		app.Status, app.Detail = api.CheckWarn, "no foxess-local systemd unit (a container install checks the app below)"
		return []api.Check{app}
	}
	restarts, _ := strconv.Atoi(p["NRestarts"])
	switch {
	case p["ActiveState"] != unitActive:
		app.Status, app.Detail = api.CheckFail, fmt.Sprintf("%s (%s), restarted %d times", p["ActiveState"], p["SubState"], restarts)
		app.Fix = "See why: `journalctl -u foxess-local -n 50`."
	case restarts > 0:
		app.Status = api.CheckWarn
		app.Detail = fmt.Sprintf("running, but systemd restarted it %d times since it was last started", restarts)
		app.Fix = "See why it stopped: `journalctl -u foxess-local -b | grep -iE 'fatal|panic|error'`."
	default:
		app.Status, app.Detail = api.CheckOK, "running"
	}
	return []api.Check{app, unitCheck(env, "foxess-gate.service", "Gate (foxess-gate)",
		"Start it: `sudo systemctl start foxess-gate`."), lastGateCheck(env)}
}

// unitCheck: a unit that should be active.
func unitCheck(env doctorEnv, unit, title, fix string) api.Check {
	c := api.Check{ID: unit, Title: title}
	p := unitProps(env, unit)
	switch {
	case p == nil || p["LoadState"] == "not-found":
		c.Status, c.Detail, c.Fix = api.CheckFail, "not installed", "Re-run `sudo bash installer.sh install` (it keeps the config)."
	case p["ActiveState"] != unitActive:
		c.Status, c.Detail, c.Fix = api.CheckFail, p["ActiveState"], fix
	default:
		c.Status, c.Detail = api.CheckOK, unitActive
	}
	return c
}

// lastGateCheck: the 5-minute rule-loss check's timer, and its last
// result (a failure means it found RULE-LOSS and re-applied the gate).
func lastGateCheck(env doctorEnv) api.Check {
	c := unitCheck(env, "foxess-gate-check.timer", "Last gate check",
		"Start it: `sudo systemctl enable --now foxess-gate-check.timer`.")
	if c.Status != api.CheckOK {
		return c
	}
	p := unitProps(env, "foxess-gate-check.service")
	switch r := p["Result"]; r {
	case "success", "":
		c.Detail = "the rules were in place at the last 5-minute check"
	default:
		c.Status = api.CheckFail
		c.Detail = "the last check failed (" + r + "): it found RULE-LOSS or could not re-apply the gate"
		c.Fix = "See `journalctl -t foxess-gate -n 20`; re-apply with `sudo systemctl restart foxess-gate`."
	}
	return c
}

// doctorGateRules: the live ruleset (root only: nft needs it).
func doctorGateRules(env doctorEnv) api.Check {
	c := api.Check{ID: "gate_rules", Title: "Gate rules"}
	if !env.root {
		c.Status, c.Detail = api.CheckPending, "not checked: reading the firewall needs root — run `sudo foxess-local doctor`"
		return c
	}
	if _, code, err := env.run(applyGate, "show"); err != nil || code != 0 {
		c.Status, c.Detail = api.CheckFail, "the gate's table or routing is missing: the inverter is not filtered"
		c.Fix = "Re-apply it: `sudo systemctl restart foxess-gate`."
		return c
	}
	c.Status, c.Detail = api.CheckOK, "table and routing present"
	return c
}

// doctorApp: the app answers, then its own readiness checks.
func doctorApp(env doctorEnv, base string) []api.Check {
	c := api.Check{ID: "app", Title: "App"}
	code, _, err := env.get(base+"/healthz", "")
	if err != nil || code != http.StatusOK {
		c.Status, c.Detail = api.CheckFail, "not answering at "+base
		c.Fix = "See why: `journalctl -u foxess-local -n 50`."
		return []api.Check{c}
	}
	c.Status, c.Detail = api.CheckOK, "answering at "+base
	out := []api.Check{c}
	_, body, err := env.get(base+"/readyz", env.token)
	var r struct {
		Checks []api.Check `json:"checks"`
	}
	if err != nil || json.Unmarshal(body, &r) != nil {
		return append(out, api.Check{
			ID: "readyz", Title: "Readiness", Status: api.CheckFail,
			Detail: "no readiness report from " + base + "/readyz", Fix: "Upgrade: this version predates it.",
		})
	}
	out = append(out, r.Checks...)
	if env.token == "" {
		out = append(out, api.Check{
			ID: "token", Title: "Details", Status: api.CheckPending,
			Detail: "set FOXESS_TOKEN=<a token> for each check's detail and fix",
		})
	}
	return out
}
