package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gateNS runs a script in a throwaway user+network namespace (an
// unprivileged user has CAP_NET_ADMIN there): the real nft and ip load
// the real gate files without touching this machine. It skips where the
// kernel or the tools do not allow it.
func gateNS(t *testing.T, env []string, script string) string {
	t.Helper()
	if _, err := exec.LookPath("nft"); err != nil {
		t.Skip("nft not installed")
	}
	if err := exec.Command("unshare", "-rn", "true").Run(); err != nil {
		t.Skipf("no unprivileged network namespaces here: %v", err)
	}
	//nolint:gosec // G204: the script is a test constant, run in a throwaway namespace
	cmd := exec.Command("unshare", "-rn", "bash", "-euo", "pipefail", "-c", "ip link set lo up\n"+script)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("namespace script failed: %v\n%s", err, out)
	}
	return string(out)
}

func gateEnv(t *testing.T, mac string) []string {
	t.Helper()
	dir := t.TempDir()
	envFile := filepath.Join(dir, "gate.env")
	body := "INVERTER_IP=192.0.2.10\n"
	if mac != "" {
		body += "INVERTER_MAC=" + mac + "\n"
	}
	if err := os.WriteFile(envFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	nft, err := filepath.Abs("gate.nft")
	if err != nil {
		t.Fatal(err)
	}
	return []string{
		"GATE_ENV=" + envFile, "GATE_NFT=" + nft, "STATE_DIR=" + filepath.Join(dir, "state"),
		"GATEWAY_IP=192.0.2.1", "INVERTER_IP=", "INVERTER_MAC=",
	}
}

// TestGateLifecycle (audit S2, S3, S10): the boot-time drop blocks the
// inverter before the network is up; `up` loads the gate — MAC-scoped
// rules, an IPv6 forward drop, rate-limited logging, drops never
// limited — and removes the boot-time drop in the same transaction;
// `down` leaves nothing behind.
func TestGateLifecycle(t *testing.T) {
	out := gateNS(t, gateEnv(t, "02:00:00:00:00:01"), `
echo "== early"; bash apply-gate.sh early; nft list ruleset
echo "== up"; bash apply-gate.sh up; nft list ruleset
echo "== down"; bash apply-gate.sh down; nft list ruleset; echo "== end"
`)
	early := section(out, "== early", "== up")
	up := section(out, "== up", "== down")
	down := section(out, "== down", "== end")

	for _, want := range []string{"table ip foxess_gate_boot", "ip saddr 192.0.2.10 counter", "ether saddr 02:00:00:00:00:01 counter", "table ip6 foxess_gate6"} {
		if !strings.Contains(early, want) {
			t.Errorf("early: missing %q\n%s", want, early)
		}
	}
	if strings.Contains(up, "foxess_gate_boot") {
		t.Errorf("up: the boot-time drop survived the gate\n%s", up)
	}
	for _, want := range []string{
		"table ip foxess_gate", "table ip6 foxess_gate6",
		"ether saddr 02:00:00:00:00:01 tcp dport 14431 tproxy",
		`limit rate 10/minute burst 5 packets log prefix "foxess-gate: drop-fwd "`,
		"ip saddr 192.0.2.10 counter packets 0 bytes 0 drop",
	} {
		if !strings.Contains(up, want) {
			t.Errorf("up: missing %q\n%s", want, up)
		}
	}
	for line := range strings.SplitSeq(up, "\n") {
		if strings.Contains(line, "limit rate") && strings.HasSuffix(strings.TrimSpace(line), " drop") {
			t.Errorf("up: a drop is rate-limited (excess traffic would pass): %s", line)
		}
	}
	if strings.Contains(down, "table") {
		t.Errorf("down: tables left\n%s", down)
	}
}

// TestGateWithoutMAC: with no INVERTER_MAC the MAC rules are omitted and
// the gate is the IP-scoped one.
func TestGateWithoutMAC(t *testing.T) {
	out := gateNS(t, gateEnv(t, ""), "bash apply-gate.sh up; nft list ruleset")
	if strings.Contains(out, "ether saddr") || strings.Contains(out, "@INVERTER_MAC@") {
		t.Errorf("MAC rules without a MAC:\n%s", out)
	}
	if !strings.Contains(out, "ip saddr 192.0.2.10 tcp dport 14431 tproxy") {
		t.Errorf("IP-scoped gate missing:\n%s", out)
	}
}

// TestGateConcurrentUp: the installer's `enable --now` starts the gate
// and the rule-loss check together, and the check re-applies a gate it
// finds missing: two `up`s at once. Both must succeed (the loser's
// `ip rule add` failed with "File exists", leaving foxess-gate.service
// failed — seen on an OpenStick, 2026-11-19), and the result is one
// gate, not two copies of its rules.
func TestGateConcurrentUp(t *testing.T) {
	out := gateNS(t, gateEnv(t, ""), `
fail=0
for i in 1 2 3 4 5 6 7 8; do bash apply-gate.sh up 2>&1 & done
for i in 1 2 3 4 5 6 7 8; do wait -n || fail=1; done
echo "== failed=$fail"
nft list ruleset; ip rule show
`)
	if !strings.Contains(out, "== failed=0") {
		t.Fatalf("a concurrent up failed:\n%s", out)
	}
	if n := strings.Count(out, "ip saddr 192.0.2.10 tcp dport 14431 tproxy"); n != 1 {
		t.Errorf("tproxy rule %d times, want 1:\n%s", n, out)
	}
	if n := strings.Count(out, "fwmark 0x1 lookup 100"); n != 1 {
		t.Errorf("fwmark rule %d times, want 1:\n%s", n, out)
	}
}

func section(s, from, to string) string {
	i := strings.Index(s, from)
	j := strings.Index(s, to)
	if i < 0 || j < i {
		return ""
	}
	return s[i:j]
}

// TestGateCheckWithoutConntrack: conntrack is optional — without it the
// check still verifies the rules and passes on a healthy gate, skipping
// only the flow scan (it reported RULE-LOSS and failed every 5 minutes).
func TestGateCheckWithoutConntrack(t *testing.T) {
	env := append(gateEnv(t, ""), "CONNTRACK=/nonexistent/conntrack", "APPLY=false")
	out := gateNS(t, env, `
bash apply-gate.sh up >/dev/null
if bash check-gate.sh; then echo "== check ok"; else echo "== check failed $?"; fi
`)
	if !strings.Contains(out, "== check ok") || strings.Contains(out, "RULE-LOSS") {
		t.Fatalf("a healthy gate without conntrack failed the check:\n%s", out)
	}
}

// TestGateCheckConntrackTuples: a conntrack line carries the original
// AND the reply tuple; only the original names the inverter's
// destination. The live 14431 flow (rendering captured on the gateway host,
// 2026-11-17) passes; a flow to another port alerts. Either way the
// check leaves its result in the status file the app reads (F2).
func TestGateCheckConntrackTuples(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "conntrack")
	write(t, fake, `#!/bin/sh
cat "$FLOWS"
`)
	if err := os.Chmod(fake, 0o755); err != nil { //nolint:gosec // G302: a test script must be executable
		t.Fatal(err)
	}
	live := "tcp      6 431995 ESTABLISHED src=192.0.2.10 dst=8.209.116.72 sport=61836 dport=14431 src=8.209.116.72 dst=192.0.2.10 sport=14431 dport=61836 [ASSURED] mark=0 use=1\n"
	other := "tcp      6 431995 ESTABLISHED src=192.0.2.10 dst=203.0.113.9 sport=40000 dport=443 src=203.0.113.9 dst=192.0.2.10 sport=443 dport=40000 [ASSURED] mark=0 use=1\n"
	for name, tc := range map[string]struct {
		flows string
		ok    bool
	}{"live 14431 flow": {live, true}, "port 443": {live + other, false}} {
		flows := filepath.Join(dir, strings.ReplaceAll(name, " ", "_"))
		write(t, flows, tc.flows)
		status := filepath.Join(dir, "status-"+strings.ReplaceAll(name, " ", "_"))
		env := append(gateEnv(t, ""), "CONNTRACK="+fake, "FLOWS="+flows, "APPLY=true", "STATUS_DIR="+status)
		out := gateNS(t, env, `
bash apply-gate.sh up >/dev/null
if bash check-gate.sh; then echo "== check ok"; else echo "== check failed"; fi
`)
		if got := strings.Contains(out, "== check ok"); got != tc.ok {
			t.Errorf("%s: check ok = %v, want %v\n%s", name, got, tc.ok, out)
		}
		b, err := os.ReadFile(filepath.Join(status, "status")) //nolint:gosec // G304: the test's own temp dir
		if err != nil {
			t.Fatalf("%s: no status file: %v", name, err)
		}
		want := "result=ok\n"
		if !tc.ok {
			want = "result=rule-loss\n"
		}
		if s := string(b); !strings.Contains(s, want) || !strings.Contains(s, "ts=") || (!tc.ok && !strings.Contains(s, "detail=off-allowlist inverter flow")) {
			t.Errorf("%s: status file:\n%s", name, s)
		}
	}
}
