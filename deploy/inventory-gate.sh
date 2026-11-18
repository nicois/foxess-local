#!/usr/bin/env bash
# foxess gate pre-state inventory — deploy/inventory-gate.sh
# (install: /usr/local/lib/foxess-local/inventory-gate.sh, root)
#
# Runbook 1a.0 (done 2026-11-15; kept for re-installs and audits): record
# the live state BEFORE the gate (or a cutover) touches anything.
# READ-ONLY — no rule is added, removed, or changed.
# The output is the reference for:
#   - which mechanism owns the pre-existing 14431 redirect
#     (firewalld --direct? legacy iptables? an nftables table?) — the
#     1a.1 decision (nft draft vs controlled conversion) and
#   - the rollback.sh post-state check (the surviving redirect dump
#     must match what this recorded)
#
# Usage: inventory-gate.sh [output-file]
#   Defaults to /var/lib/foxess-gate/inventory-<UTCstamp>.txt (also
#   printed to stdout).

set -euo pipefail

# INVERTER_IP: the environment, else /etc/foxess-local/gate.env (the
# installer seeds it from --inverter-ip). There is no default.
gate_env_value() {
    local f="${GATE_ENV:-/etc/foxess-local/gate.env}"
    [ -f "$f" ] && sed -n "s/^$1=//p" "$f" | tail -1
}
INVERTER_IP="${INVERTER_IP:-$(gate_env_value INVERTER_IP)}"
if [ -z "$INVERTER_IP" ]; then
    echo "$(basename "$0"): INVERTER_IP is not set (add it to /etc/foxess-local/gate.env)" >&2
    exit 2
fi
OUT="${1:-/var/lib/foxess-gate/inventory-$(date -u +%Y%m%dT%H%M%SZ).txt}"
mkdir -p "$(dirname "$OUT")"

section() { echo; echo "=== $* ==="; }

{
    section "host / time"
    hostname; date -u

    section "nftables: full ruleset"
    nft list ruleset 2>/dev/null || echo "(nft: no ruleset / not installed)"

    section "legacy iptables"
    iptables-save 2>/dev/null || echo "(no legacy iptables)"
    iptables -t mangle -S 2>/dev/null | grep -i tproxy || true

    section "firewalld direct rules"
    if command -v firewall-cmd >/dev/null 2>&1; then
        firewall-cmd --state 2>/dev/null || echo "(firewalld not running)"
        firewall-cmd --direct --get-all-chains 2>/dev/null || true
        firewall-cmd --direct --get-all-rules 2>/dev/null || true
    else
        echo "(firewalld not installed)"
    fi

    section "fwmark routing (mark 1 / table 100 — the TPROXY mechanics)"
    ip rule show
    ip route show table 100 2>/dev/null || echo "(no local route in table 100)"

    section "listeners on the relevant ports (31280 prox / 14431 cloud)"
    ss -ltnp 2>/dev/null | grep -E '[:.]?(31280|14431)\b' || echo "(none listening)"

    section "services"
    systemctl list-units --type=service --all 'foxess*' 'prox*' --no-pager 2>/dev/null \
        || systemctl status prox.service --no-pager

    section "conntrack: inverter flows (check-gate.sh (b) parses THIS rendering)"
    if command -v conntrack >/dev/null 2>&1; then
        conntrack -L 2>/dev/null | grep "src=${INVERTER_IP}" || echo "(no inverter flows right now)"
    else
        echo "(conntrack tool not installed — the detector's flow check cannot work; record this)"
    fi
} | tee "$OUT"

echo
echo "inventory written to $OUT"
