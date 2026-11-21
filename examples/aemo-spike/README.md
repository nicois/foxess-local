# aemo-spike: a foxess-local sidecar

An example automation for foxess-local, and a skeleton to adapt for your
own. While the NEM's 5-minute dispatch price for your region is high,
it force-discharges the battery.
The inverter stops the discharge at a cut-off SoC.

It needs no Home Assistant. It runs beside foxess-local, on the gateway
host or an OpenStick, as one static binary of about 7 MB. One decision
peaks at about 14 MB of memory, and it does one HTTP fetch to AEMO every
5 minutes. It talks to the inverter only through foxess-local's REST
API, with its own token. foxess-local stays the only write path, with
its write budget and write gate.

## The pattern

A sidecar decides *when* to act. foxess-local decides *how*.

| The sidecar | foxess-local |
|---|---|
| reads the price and the SoC | owns the schedule table |
| decides start / renew / stop (`decide.go`) | puts the force window in a free group without moving yours |
| holds a **lease** — `PUT /api/v1/schedule/override` with `until` 30 min ahead, renewed while it wants it | clears the window when the lease ends, on `DELETE`, or after a restart |
| sends notices — `POST /api/v1/notices` | delivers them to your webhook / ntfy and lists them under Activity → Notices |

**Keep schedule group 1 disabled.** The inverter runs the first enabled
group (by number) whose window covers the time, and foxess-local treats
any disabled group as free. With group 1 free, an override always goes
there and always runs first. If group 1 is in use, the override needs a
free group numbered below every window it falls inside. Without one it
is refused (`409 override_no_group`), for example during a force charge
in group 1.

Why a lease:

- **The sidecar can fail safely.** If the sidecar dies, crashes or loses
  the network, nothing is left behind. The override lapses within one
  lease, and foxess-local clears the group, so it does not come back
  tomorrow. Schedule windows repeat daily.
- **The SoC limit holds without the sidecar.** The inverter stops at the
  window's cut-off SoC (`fd_soc_pct`). The sidecar's SoC checks only
  decide whether to start or renew.
- **Writes are limited.** Each start, renewal and stop is one write to
  the inverter's memory, and the schedule allows 10 an hour. With
  30-minute leases renewed when less than 10 minutes remain, a long
  spike costs about 3 writes an hour. Lease ends are rounded to 5
  minutes, and a renewal that asks for the same end is not written.
- **One holder at a time.** An override held by another token (another
  sidecar, or HA) is left alone: `409 override_held`. You end any
  override from "Running now" in the web UI (expert or installer).

## Setting it up

1. **Create a token for the sidecar.** In the web UI go to System →
   Access, and create a `standard` token named `aemo-spike`. The name
   matters: the sidecar recognises its own override by the holder's name
   (`-name`). It is also the origin shown on its notices.
2. **Build it** (Go 1.27, no dependencies):
   ```sh
   CGO_ENABLED=0 GOARCH=arm64 go build -trimpath -ldflags=-s -o aemo-spike ./examples/aemo-spike   # OpenStick
   ```
   Use `GOARCH=amd64` or `GOARCH=arm GOARM=7` for other hosts.
3. **Try it without changing anything:**
   ```sh
   FOXESS_TOKEN=… ./aemo-spike -once -dry-run
   # INFO decision action=hold reason="VIC1 at $36/MWh" soc=62 price="VIC1 $35.94/MWh for 11:50"
   ```
4. **Install it as a service.** See `aemo-spike.service`. It runs as a
   dynamic user with no filesystem writes, under `MemoryMax=48M`.

## The knobs

| Flag | Default | Meaning |
|---|---|---|
| `-region` | VIC1 | NSW1, QLD1, SA1, TAS1, VIC1 |
| `-start-above` | 300 | start at or above this price, $/MWh |
| `-stop-below` | 200 | once running, stop at or below this price (hysteresis) |
| `-start-soc` | 40 | do not start below this SoC |
| `-cutoff-soc` | 30 | the inverter stops discharging here |
| `-power` | 5000 | the window's power, W |
| `-min-hold` | 10m | once started, run at least this long |
| `-lease` / `-renew-before` | 30m / 10m | the lease |

Some plans reward exporting while the wholesale price is high. That
reward is why discharging on a price spike pays. Choose the thresholds
for your plan.

Grid export can be capped by your connection agreement, for example
5 kW above your own use. If your inverter's export limit is set to that
cap, it applies whatever `-power` says.

## Adapting it

- **Another price feed.** Implement `PriceSource` (`aemo.go`). The
  NEMWeb reader is the example: a ~125 KB listing, then the newest
  ~21 KB report only when it is new, and the `DISPATCH,PRICE` table's
  `RRP` for the region.
- **Another policy.** `Policy.Decide` (`decide.go`) is a pure function
  of the price, the SoC and the lease, with a table
  test (`TestDecide`). Change it there.
- **Force charge on negative prices.** The same endpoint takes
  `"mode": "force-charge"`, where `fd_soc_pct` is the charge target.
- **Other languages.** It is three HTTP calls: `GET /api/v1/state`,
  `PUT`/`DELETE /api/v1/schedule/override`, and `POST /api/v1/notices`.
  The server's own description is at `GET /api/v1/openapi.yaml`.

## What it does not do

- It does not forecast. It reacts to the price of the interval being
  dispatched now.
- It does not keep state across restarts. The override's `holder` and
  `since`, read from `/state`, are all it needs.
- It does not clear the override when it stops. The override runs to the
  end of its lease, at most 30 minutes, without a write on the way out.
