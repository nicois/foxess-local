#!/usr/bin/env bash
# probe-registers.sh — read the candidate KH live-value / version register
# blocks through the running foxess-local (POST /api/v1/registers/read,
# installer token) and save the raw answers for decoding (profile §8).
#
# Reads only — nothing is written to the inverter. Each block is one
# gateway command (one in flight at a time); an unsupported block answers
# with a Modbus exception (502 command_failed, code in the detail).
#
# Usage:  FOXESS_TOKEN=fl_... tools/probe-registers.sh [out-file]
# Env:    FOXESS_URL (default http://127.0.0.1:18080)
set -euo pipefail

: "${FOXESS_TOKEN:?set FOXESS_TOKEN to an installer token}"
url="${FOXESS_URL:-http://127.0.0.1:18080}"
out="${1:-probe-$(date +%Y%m%dT%H%M%S).txt}"

# fn addr count  label  (candidate map: nathanmarlor/foxess_modbus, KH groups)
blocks=(
	"4 10016 3  version-KH_PRE119"
	"3 30016 3  version-KH_PRE133"
	"3 36001 3  version-KH_133"
	"3 31000 29 realtime-31000-31028"
	"3 31039 16 realtime-31039-31054"
	"4 11000 59 realtime-PRE119-11000-11058"
	"4 11096 9  realtime-PRE119-11096-11104"
	"3 39070 8  pv-strings-39070-39077"
	"3 39168 2  grid-ct-39168-39169"
	"3 39279 8  pv-power-39279-39286"
	"3 37617 16 bms-37617-37632"
)

{
	echo "# probe $(date -Is) url=$url"
	echo "# also record now: snap61 SoC/energy + pack voltage (GET /api/v1/state)"
	curl -fsS -m 10 "$url/api/v1/state" | tr -d '\n' | cut -c1-400
	echo
	for b in "${blocks[@]}"; do
		read -r fn addr count label <<<"$b"
		echo "## $label fn=$fn addr=$addr count=$count"
		body=$(printf '{"fn":%d,"addr":%d,"count":%d}' "$fn" "$addr" "$count")
		curl -sS -m 30 -H "Authorization: Bearer $FOXESS_TOKEN" \
			-H 'Content-Type: application/json' -d "$body" \
			-w '\nhttp=%{http_code}\n' "$url/api/v1/registers/read" || echo "curl failed"
	done
} | tee "$out"
echo "saved: $out" >&2
