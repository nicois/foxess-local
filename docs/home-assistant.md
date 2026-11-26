# Home Assistant

foxess-local is meant to sit under Home Assistant: it owns the inverter
connection, and Home Assistant owns tariffs, automations and the Energy
dashboard. Two ways to connect them:

- **The foxess-control integration** (HACS) with its **local backend**,
  which talks to this app instead of the FoxESS cloud. That backend is
  in development; this page will link to its setup when it ships.
- **Plain Home Assistant configuration**, below. It needs nothing but the
  REST API, and is what the integration's local backend builds on.

The API's full contract is served by your install at
`http://<this machine>:18080/api/v1/openapi.yaml` (the same document as
[`api/openapi.yaml`](../api/openapi.yaml)): every endpoint, field, unit,
sign convention and error code.

## 1. Tokens

Create the tokens in the web UI, **System → Access**
(installer role), and keep them in Home Assistant's `secrets.yaml`:

| Token | Role | For |
|---|---|---|
| `home-assistant` | read-only | the sensors |
| `home-assistant-control` | standard | work mode and schedule commands (only if you want them) |

```yaml
# secrets.yaml — the whole header value, "Bearer " included
foxess_read: "Bearer fl_…"
foxess_control: "Bearer fl_…"
```

A token is shown once, when it is created. Without a token, the
dashboard's data is readable by anyone on your network while
**public read-only access** is on (the default); the sensors below send
one anyway, so they keep working if you turn that off.

## 2. Sensors

`GET /api/v1/state` is the live snapshot, refreshed every 5 seconds.
One `rest:` block reads it once and splits it into sensors:

```yaml
# configuration.yaml
rest:
  - resource: http://foxess.local:18080/api/v1/state
    headers:
      Authorization: !secret foxess_read
    scan_interval: 10
    sensor:
      - name: FoxESS solar power
        value_template: "{{ value_json.power.pv_w }}"
        unit_of_measurement: W
        device_class: power
        state_class: measurement
      - name: FoxESS grid power        # + importing, − exporting
        value_template: "{{ value_json.power.grid_w }}"
        unit_of_measurement: W
        device_class: power
        state_class: measurement
      - name: FoxESS load power
        value_template: "{{ value_json.power.load_w }}"
        unit_of_measurement: W
        device_class: power
        state_class: measurement
      - name: FoxESS AC-coupled solar  # the second CT (meter 2), if fitted
        value_template: >-
          {{ -value_json.power.meter2_w if value_json.power.meter2_w is not none else none }}
        availability: "{{ value_json.present.meter2 }}"
        unit_of_measurement: W
        device_class: power
        state_class: measurement
      - name: FoxESS battery power     # + charging, − discharging
        value_template: "{{ value_json.power.battery_w }}"
        unit_of_measurement: W
        device_class: power
        state_class: measurement
      - name: FoxESS battery SoC
        value_template: "{{ value_json.battery.soc_pct }}"
        unit_of_measurement: "%"
        device_class: battery
        state_class: measurement
      - name: FoxESS battery SoC estimate   # between the whole-percent readings, 2 decimals
        value_template: "{{ value_json.battery.soc_estimate_pct }}"
        unit_of_measurement: "%"
        device_class: battery
        state_class: measurement
      - name: FoxESS battery coldest cell
        value_template: "{{ value_json.temperatures.cell_min_c }}"
        unit_of_measurement: °C
        device_class: temperature
        state_class: measurement
      - name: FoxESS BMS charge limit
        value_template: "{{ value_json.battery.bms_charge_limit_a }}"
        unit_of_measurement: A
        device_class: current
        state_class: measurement
      - name: FoxESS work mode
        value_template: "{{ value_json.active_mode.mode if value_json.active_mode else 'unknown' }}"
    binary_sensor:
      - name: FoxESS inverter online
        value_template: "{{ value_json.online }}"
        device_class: connectivity
```

Replace `foxess.local` with your machine's name or address. A value the
inverter has not reported yet is `null` in the JSON (the temperatures
and the charge limit until the first reading), which Home Assistant
shows as unknown.

### Energy dashboard

The Energy dashboard needs energy (kWh) that only ever increases. The
inverter keeps lifetime counters, and `/state` reports them under
`energy` (read every 30 s; they match the FoxESS app and survive
restarts). Add them to the same `rest:` block:

```yaml
      - name: FoxESS grid export energy
        value_template: "{{ value_json.energy.grid_export_kwh }}"
        unit_of_measurement: kWh
        device_class: energy
        state_class: total_increasing
      - name: FoxESS grid import energy
        value_template: "{{ value_json.energy.grid_import_kwh }}"
        unit_of_measurement: kWh
        device_class: energy
        state_class: total_increasing
      - name: FoxESS solar energy
        value_template: "{{ value_json.energy.pv_kwh }}"
        unit_of_measurement: kWh
        device_class: energy
        state_class: total_increasing
      - name: FoxESS battery charge energy
        value_template: "{{ value_json.energy.battery_charge_kwh }}"
        unit_of_measurement: kWh
        device_class: energy
        state_class: total_increasing
      - name: FoxESS battery discharge energy
        value_template: "{{ value_json.energy.battery_discharge_kwh }}"
        unit_of_measurement: kWh
        device_class: energy
        state_class: total_increasing
```

Pick those in the Energy dashboard. `load_kwh` (household use) and
`output_kwh` (the inverter's AC output) are there too. A counter is
`null` until its first read, and on inverter models that do not report
it.

## 3. Commands (optional)

Commands are confirmed: the request returns once the inverter has read
the new value back (or with an error that says why not). Writes are
rationed to protect the inverter's memory: a setting accepts at most 10
real writes an hour (more is refused with HTTP 429, and you get a
notification), and a write of the value the inverter already holds is
not sent at all. Request bodies are strict: a misspelt field is refused
(HTTP 400), never ignored.

```yaml
rest_command:
  foxess_work_mode:
    url: http://foxess.local:18080/api/v1/work-mode
    method: PUT
    headers:
      Authorization: !secret foxess_control
    content_type: application/json
    payload: '{"work_mode": "{{ mode }}"}'   # self-use | feed-in
```

```yaml
# an automation action
action: rest_command.foxess_work_mode
data:
  mode: feed-in
```

The schedule (`PUT /api/v1/schedule`) takes the whole 8-group table;
read the current one from `/state`'s `schedule`, change it, and send it
back. The spec documents every field.

## 4. Notifications into Home Assistant

The app can send its alerts to a Home Assistant **webhook trigger**: the
inverter offline for 10 minutes, the gate losing its rules, a failed
command or schedule, a write refused by the hourly limit, low disk, an
upgrade rolled back, and cloud-forwarding or maintenance-window changes.

1. Create an automation with a **Webhook** trigger; pick a hard-to-guess
   ID and allow only local access.
2. In the web UI, **System → Notifications**, set the webhook
   URL to `http://homeassistant.local:8123/api/webhook/<the ID>` and press
   **Send a test**.

The JSON body:

```json
{"source": "foxess-local", "event": "inverter-offline", "severity": "critical",
 "title": "Inverter offline", "message": "No readings from the inverter for 10m0s.",
 "ts": "2026-11-18T03:12:00Z"}
```

```yaml
# the automation
triggers:
  - trigger: webhook
    webhook_id: foxess-local-<random>
    local_only: true
actions:
  - action: notify.mobile_app_your_phone
    data:
      title: "{{ trigger.json.title }}"
      message: "{{ trigger.json.message }}"
```

`severity` is `critical`, `warning` or `info` (only "back online" and
the test are info) — filter on it to page yourself only for the
critical ones.

## 5. For integration authors

- `GET /api/v1/events` is a Server-Sent Events stream: `snapshot` (the
  live values, ~5 s), `config` (any confirmed change, with the full
  state), `connection` (online / offline) and `system` (alerts, mode
  and window changes). It resumes from `Last-Event-ID`: the missed
  `config`, `connection` and `system` events, then the latest snapshot
  (snapshots are not replayed; each carries the id of the last
  discrete event sent, so a resume works from any event).
- `GET /api/v1/history?range=24h` gives bucketed series and energy
  totals; `GET /api/v1/power/average?window=5m` a trailing mean.
- `GET /readyz` (no token) answers 503 while something is wrong, and
  `GET /metrics` serves Prometheus metrics.
- Errors are RFC 7807 problem documents with a stable `code`; branch on
  it, not on the text.
- `state.energy` holds the lifetime counters (kWh, 0.1 kWh resolution,
  `as_of`, and `sources` saying each comes from an inverter register).
  They never decrease. If the inverter's counter goes backwards
  (inverter replaced, firmware reset), the new value is reported and a
  `system` event `counter-reset` names the field: re-baseline rather than
  booking a negative delta. Every `snapshot` event carries `energy` once
  it has been read.
- `state.write_budget` shows, per setting, how many real writes remain
  in the rolling hour (`limit`, `remaining`, `window_s`,
  `next_free_at`). A write the inverter already holds costs nothing; a
  `PUT /schedule` costs one however many groups changed. Over the budget
  a write is refused with `429 write_budget`, carrying `setting`,
  `remaining: 0`, `next_free_at` and a `Retry-After` header. Pace
  frequent adjustments against `remaining` and keep some writes in
  reserve for the ones that matter. The limits are configurable
  (`[writes]` in the config file).
- `GET /api/v1/config` includes `site_time_zone` (IANA, e.g.
  `Australia/Sydney`): the inverter's clock and schedule follow it, not
  the host's zone. `active_mode` in `/state` is null until the
  inverter's clock has been read or a site zone chosen.
