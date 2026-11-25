# Automations (sidecars)

An automation (a "sidecar") is a separate program that decides *when*
the inverter should do something, from data foxess-local does not have:
wholesale prices, the weather. It acts through the
REST API with its own named token, like Home Assistant does, and it can
run on the gateway host beside foxess-local. foxess-local stays the only
write path (C-003): a sidecar never talks to the inverter.

`examples/aemo-spike/` is the worked example: a force discharge while
the NEM's dispatch price is high.

### D-029: The schedule override — a force window with an expiry, owned by foxess-local
**Context** (owner, 2026-11-21): a sidecar should force-discharge while
the price spikes, unless the SoC is too low. On the KH a force
discharge is a schedule window: `PUT /work-mode` has only self-use and
feed-in. Splicing a window into the table from outside means a blind
full-table replace (racing the owner's edits), avoiding windows that
cross (`schedule_overlap`), the 8-group limit, and midnight. Worst,
windows repeat daily: a sidecar that dies mid-discharge leaves a window
that runs again tomorrow at the same time.
**Decision** (owner, 2026-11-21): `PUT /schedule/override` takes a mode
(force discharge or force charge), an `until`, the cut-off SoC and the
power. foxess-local places and clears the window:
- **Placement.** The first enabled group by index whose window holds the
  time runs (`activeMode`), and enabled windows may nest but not cross.
  The window is cut short where another group's window begins inside
  it, or where a window it starts inside ends first, so it lies inside
  (or beside) every group it meets. It takes the lowest free group below
  every group it lies inside. The owner's groups never move: each
  group's parameter block (57000 + 50·(k−1): import / export limit, PV)
  belongs to its slot, and the API does not write those blocks. No free
  group in front is `409 override_no_group`; a scheduler that is off is
  `409 override_unavailable` (turning it on would start every group).
- **Convention: keep group 1 disabled** (owner, 2026-11-21). A disabled
  group is taken to be free for an override; group 1 runs before every
  other group, so with it free an override always has a place. Nothing
  enforces it: a reservation setting with a `PUT /schedule` guard was
  rejected (the owner prefers the convention; the cloud can rewrite the
  table in `full` or a window anyway). When group 1 is in use, placement
  falls back to the rule above.
- **The inverter enforces the SoC.** The window's cut-off SoC
  (`fd_soc_pct`) stops a force discharge at that SoC; the sidecar does
  not watch it to stop in time. Max SoC and min SoC on grid come from
  the group it runs inside (else 100 / 10).
- **A lease.** One override at a time, held by the token that started it
  (another token: `409 override_held`). The holder renews by sending the
  same request with a later `until`: the group and start stay, the end
  moves. A holder that stops lets it lapse at `until`. The window ends at
  the inverter's 23:59 at the latest; the holder renews after midnight.
- **Clearing.** At the window's end (plus 5 s), on `DELETE` (the holder,
  an expert or an installer), and after a restart, foxess-local clears
  the group: one schedule write, the rest of the table as stored. The
  override is stored (`settings` key `schedule_override.active`) *before*
  its write, so a group on the inverter is always one the expiry loop
  knows; a write confirmed not applied restores the previous state. A
  failed clear is retried every minute (the window recurs only the next
  day). A group the table shows changed by someone else since the last
  confirmed write is left as it is.
- **Never queued.** With no inverter session it is `503
  inverter_offline` and nothing is written: a queued override would land
  after its caller gave up.
- **Writes.** Each start, renewal (when the end changes), and clear is
  one `schedule` write against its budget (10 an hour). A renewal that
  asks for the same minute is not written. A sidecar renews in steps
  (the example: 30-minute leases, renewed when under 10 minutes remain).
- The audit names the holder; `GET /state` carries `schedule_override`;
  the system events `schedule-override-started` / `-ended` reach the UI
  and are notified at `info` (ntfy priority 3): the holder, mode, power,
  window and cut-off; the end gives why (expired, ended by …). A renewal
  is not news. (Owner, 2026-11-25; until then only the sidecar notified,
  D-030. The example now sends only what foxess-local cannot know: a
  short "Price spike" notice with the price at the start, and a refusal;
  no notice at the end.)
  "Running now" names the holder and the end, with "End override now".
**Rejected**:
- a sidecar editing `PUT /schedule` itself: the races and the daily
  recurrence above, re-solved by every sidecar;
- shifting the owner's groups down to put the override in group 1: the
  parameter blocks would no longer match their windows;
- an inverter window longer than the lease, ended early by foxess-local:
  if foxess-local stops, the inverter would run on to the long end.
**Traces**: `internal/api::TestPlanOverride`, `TestScheduleOverrideStart`,
`TestScheduleOverrideRenew`, `TestScheduleOverrideDelete`,
`TestScheduleOverrideExpiry`, `TestScheduleOverrideOwnerChanged`,
`TestScheduleOverrideRefusals`, `TestRunOverridesExpiry` (synctest);
`cmd/foxess-local::TestScheduleOverrideNotices`.

### D-030: Notices from automations, and a kept list of every notice
**Context** (owner, 2026-11-21): a sidecar should tell the owner what it
did (a discharge started, ended, or was refused) through the same
webhook / ntfy targets, attributed to it, without its own notification
setup. The notifier dropped a notice when no target was set, and the
outbox forgot a notice once delivered, so the web UI had nothing to list.
**Decision** (owner, 2026-11-21):
- `POST /notices` (standard): event (`[a-z0-9][a-z0-9-]{0,39}`),
  severity, a one-line title (≤ 120), a message (≤ 1000, no control
  characters but newlines: the title becomes ntfy's `Title` header). It
  goes through the notifier like foxess-local's own notices: the outbox
  (survives a restart, retried), both targets, and the 15-minute dedup
  (keyed by origin, event and message).
- **Origin** is the token's name: the webhook body carries `origin`
  (absent on foxess-local's own), ntfy's title is `origin: title`. The
  event is delivered as `sidecar.<event>`, so a sidecar can never pass
  for one of foxess-local's events (`inverter-offline`, `grid-lost`) in an
  HA automation. `critical` (ntfy priority 5) needs an expert or
  installer token.
- **Budget**: 20 an hour per token (rolling, in memory: a restart resets
  it), `429 notice_rate` with Retry-After.
- **The list**: every notice that is news (not a repeat) is journalled
  (`notify.WithJournal`) before the target check, into the `notices`
  table (schema v14; the newest 500, none older than 30 days), whether or
  not a target is set. `GET /notices` (read-only) pages it newest first;
  Activity → Notices shows it, refreshed on the `notice` system event (an
  automation's notice is also toasted; foxess-local's own already has its
  own event). The outbox gains `origin` (schema v14).
**Rejected**: a separate notice channel per sidecar (its own targets):
the owner configures notifications once; a sidecar-chosen severity
without a cap: a standard token could page the owner at priority 5.
**Traces**: `internal/notify::TestNotifyJournalAndOrigin`;
`internal/store::TestNotices`, `TestOutboxSurvivesReopen`;
`internal/api::TestNoticePost`, `TestNoticeRefusals`.

## The worked example: `examples/aemo-spike`

A force discharge on NEM price spikes (AEMO's NEMWeb DispatchIS reports,
the `DISPATCH,PRICE` table's `RRP`): some plans reward exporting while
the wholesale price is high. Its only network sources are NEMWeb and
the foxess-local API. Policy (`Policy.Decide`) is a pure
function with a table test; the loop holds a 30-minute lease, renewed
under 10 minutes left, with ends rounded to 5 minutes. Standard library
only; ~7 MB static, ~14 MB peak RSS for one decision; a systemd unit
with `DynamicUser`, `MemoryMax=48M`. It is not shipped: an example to
copy.
**Traces**: `examples/aemo-spike::TestParseDispatchCSV` (a real report's
rows, public data), `TestNEMWebLatest`, `TestDecide`,
`TestTickStartsAndNotifies`, `TestNextTick`.
