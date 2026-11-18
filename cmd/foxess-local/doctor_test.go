package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeHost scripts the commands and HTTP answers doctor sees.
type fakeHost struct {
	units    map[string]string // systemctl show output per unit
	gateShow int               // apply-gate.sh show exit code
	health   int               // /healthz status (0 = unreachable)
	ready    int
	readyB   string
	root     bool
	token    string
}

func (f *fakeHost) env(t *testing.T) doctorEnv {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfg, []byte("[server]\nhttp_listen = \"0.0.0.0:18080\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return doctorEnv{
		cfgPath: cfg,
		root:    f.root,
		token:   f.token,
		run: func(name string, args ...string) (string, int, error) {
			switch {
			case name == "systemctl" && len(args) > 1 && args[0] == "show":
				out, ok := f.units[args[1]]
				if !ok {
					return "ActiveState=inactive\nLoadState=not-found\n", 0, nil
				}
				return out, 0, nil
			case strings.HasSuffix(name, "apply-gate.sh"):
				return "", f.gateShow, nil
			}
			return "", 0, errors.New("not found: " + name)
		},
		get: func(url, _ string) (int, []byte, error) {
			if !strings.HasPrefix(url, "http://127.0.0.1:18080/") {
				return 0, nil, errors.New("unexpected url " + url)
			}
			if strings.HasSuffix(url, "/healthz") {
				if f.health == 0 {
					return 0, nil, errors.New("connection refused")
				}
				return f.health, []byte(`{"status":"ok"}`), nil
			}
			return f.ready, []byte(f.readyB), nil
		},
	}
}

const healthyReady = `{"status":"ready","checks":[
 {"id":"listener","title":"Inverter listener","status":"ok","detail":"listening on port 14431"},
 {"id":"inverter_connected","title":"Inverter connected","status":"ok","detail":"connected"}]}`

func healthyHost() *fakeHost {
	return &fakeHost{
		units: map[string]string{
			"foxess-local.service":      "ActiveState=active\nSubState=running\nNRestarts=0\nLoadState=loaded\n",
			"foxess-gate.service":       "ActiveState=active\nSubState=exited\nLoadState=loaded\n",
			"foxess-gate-check.timer":   "ActiveState=active\nSubState=waiting\nLoadState=loaded\n",
			"foxess-gate-check.service": "ActiveState=inactive\nResult=success\nLoadState=loaded\n",
		},
		health: 200, ready: 200, readyB: healthyReady, root: true,
	}
}

// TestDoctorHealthy (F3, audit U4): a healthy install passes every check
// and exits 0.
func TestDoctorHealthy(t *testing.T) {
	f := healthyHost()
	var out bytes.Buffer
	if code := doctor(f.env(t), &out); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	for _, want := range []string{"Configuration", "Service", "Gate rules", "Last gate check", "Inverter listener", "Inverter connected"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("no %q line:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "FAIL") || strings.Contains(out.String(), "WARN") {
		t.Errorf("a healthy install reported a problem:\n%s", out.String())
	}
}

// TestDoctorProblems: each problem is reported with its fix and a
// failure exits 1 — a restart loop (audit R3), the last gate check
// failing, the app's own readiness failures (with the detail and fix
// when a token is given), and the rules unchecked without root.
func TestDoctorProblems(t *testing.T) {
	f := healthyHost()
	f.units["foxess-local.service"] = "ActiveState=active\nSubState=running\nNRestarts=7\nLoadState=loaded\n"
	f.units["foxess-gate-check.service"] = "ActiveState=inactive\nResult=exit-code\nLoadState=loaded\n"
	f.root = false
	f.token = "fl_x"
	f.ready = 503
	f.readyB = `{"status":"not_ready","checks":[{"id":"inverter_connected","title":"Inverter connected","status":"fail",
		"detail":"no readings for 5m0s","fix":"set the inverter's gateway to this machine"}]}`
	var out bytes.Buffer
	code := doctor(f.env(t), &out)
	s := out.String()
	if code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, s)
	}
	for _, want := range []string{"restarted it 7 times", "RULE-LOSS", "sudo", "no readings for 5m0s", "set the inverter's gateway"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q:\n%s", want, s)
		}
	}
}

// TestDoctorAppDown: an app that does not answer is a failure that says
// how to look, and the readiness checks are not attempted.
func TestDoctorAppDown(t *testing.T) {
	f := healthyHost()
	f.health = 0
	f.units["foxess-local.service"] = "ActiveState=activating\nSubState=auto-restart\nNRestarts=3\nLoadState=loaded\n"
	var out bytes.Buffer
	if code := doctor(f.env(t), &out); code != 1 {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	s := out.String()
	if !strings.Contains(s, "not answering") || !strings.Contains(s, "journalctl -u foxess-local") || strings.Contains(s, "Inverter listener") {
		t.Errorf("app down:\n%s", s)
	}
}
