package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"foxesslocal/internal/quirks"

	"foxesslocal/internal/identity"
	"foxesslocal/internal/settings"
)

// SettingsAdmin is what the settings and setup endpoints need beyond
// SettingsView (settings.Settings implements it).
type SettingsAdmin interface {
	SettingsView
	Sources() map[string]settings.Source
	Set(key, value, acting string) error
	Reset(key, acting string) error
	SetupCompleted() bool
	CompleteSetup(acting string) error
}

// settingsOrder is the response order (and the keys PUT accepts, except
// forward mode, which has its own endpoint).
var settingsOrder = []string{
	settings.KeyCapacityWh, settings.KeyPublicReadOnly, settings.KeyPublicAllPages, settings.KeyForwardMode,
	settings.KeyNotifyWebhook, settings.KeyNotifyNtfy,
	settings.KeyWriteDelay,
	settings.KeySocThreshold, settings.KeySocHorizon, settings.KeySocPValue, settings.KeySocWindow,
	settings.KeyUpdateInterval, settings.KeyUpdatePrerelease, settings.KeyUpdateAutoApply,
	settings.KeySiteTimeZone, settings.KeyMeter2Label,
}

func init() { settingsOrder = append(settingsOrder, quirks.Keys()...) }

// secretShown stands in for a secret setting's value below installer.
const secretShown = "(set)"

// secretValue is a secret setting's API value: null when off, the URL
// for an installer, secretShown for anyone else.
func secretValue(v string, installer bool) any {
	switch {
	case v == "":
		return nil
	case installer:
		return v
	}
	return secretShown
}

// settingsRoutes registers the settings and setup endpoints (F12).
func (s *Server) settingsRoutes(mux *http.ServeMux) {
	s.handle(mux, "GET /api/v1/settings", s.protect(RoleReadOnly, s.handleSettingsGet))
	// expert: the tuning knobs; each other key also needs an installer
	// (checked per key in the handler)
	s.handle(mux, "PUT /api/v1/settings", s.protect(RoleExpert, s.handleSettingsPut))
	s.handle(mux, "GET /api/v1/quirks", s.protect(RoleReadOnly, s.handleQuirks))
	s.handle(mux, "POST /api/v1/notify/test", s.protect(RoleInstaller, s.handleNotifyTest))
	s.handle(mux, "GET /api/v1/setup", s.protect(RoleInstaller, s.handleSetup))
	s.handle(mux, "POST /api/v1/setup/complete", s.protect(RoleInstaller, s.handleSetupComplete))
	s.handle(mux, "POST /api/v1/clock/set", s.protect(RoleInstaller, s.handleClockSet))
}

// settingItem is one setting: its effective value and where it came from.
type settingItem struct {
	Key    string `json:"key"`
	Value  any    `json:"value"`
	Source string `json:"source"`
}

type settingsView struct {
	Settings []settingItem `json:"settings"`
}

// admin returns the live settings, or writes 503 (a server built without
// them: tests and tools).
func (s *Server) admin(w http.ResponseWriter) (SettingsAdmin, bool) {
	a, ok := s.info.Settings.(SettingsAdmin)
	if !ok {
		writeProblem(w, http.StatusServiceUnavailable, problem{
			Title: "Settings unavailable", Code: "settings_unavailable",
			Detail: "this server runs without stored settings",
		})
	}
	return a, ok
}

func (s *Server) settingsBody(a SettingsAdmin, tk Token) settingsView {
	v, src := a.Get(), a.Sources()
	var capWh any
	if v.CapacityWh > 0 {
		capWh = v.CapacityWh
	}
	installer := roleLevel[tk.Role] >= roleLevel[RoleInstaller]
	vals := map[string]any{
		settings.KeyCapacityWh:       capWh,
		settings.KeyPublicReadOnly:   v.PublicReadOnly,
		settings.KeyPublicAllPages:   v.PublicAllPages,
		settings.KeyForwardMode:      string(v.ForwardMode),
		settings.KeyNotifyWebhook:    secretValue(v.NotifyWebhook, installer),
		settings.KeyNotifyNtfy:       secretValue(v.NotifyNtfy, installer),
		settings.KeyWriteDelay:       int(v.WriteDelay / time.Second),
		settings.KeySocThreshold:     v.SocThresholdPct,
		settings.KeySocHorizon:       int(v.SocHorizon / time.Minute),
		settings.KeySocPValue:        v.SocPValue,
		settings.KeySocWindow:        int(v.SocWindow / time.Minute),
		settings.KeyUpdateInterval:   int(v.UpdateInterval / time.Hour),
		settings.KeyUpdatePrerelease: v.UpdatePrerelease,
		settings.KeyUpdateAutoApply:  v.UpdateAutoApply,
		settings.KeySiteTimeZone:     nil,
		settings.KeyMeter2Label:      v.Meter2Label,
	}
	if v.SiteZone != nil {
		vals[settings.KeySiteTimeZone] = v.SiteZone.String()
	}
	for id, on := range v.Quirks {
		vals[quirks.Key(id)] = on
	}
	out := settingsView{}
	for _, k := range settingsOrder {
		out.Settings = append(out.Settings, settingItem{Key: k, Value: vals[k], Source: string(src[k])})
	}
	return out
}

// handleSettingsGet is GET /settings: the UI-settable settings with
// their sources (ui > config > default).
func (s *Server) handleSettingsGet(w http.ResponseWriter, _ *http.Request, tk Token) {
	a, ok := s.admin(w)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.settingsBody(a, tk))
}

// handleSettingsPut is PUT /settings (installer): a partial map of key →
// value; null resets a key to the config file's (or the default) value.
// All values are checked before any is stored.
func (s *Server) handleSettingsPut(w http.ResponseWriter, r *http.Request, tk Token) {
	a, ok := s.admin(w)
	if !ok {
		return
	}
	var body map[string]json.RawMessage
	if err := readBody(r, &body); err != nil {
		writeProblem(w, http.StatusBadRequest, badField("body", err.Error()))
		return
	}
	changes, p := parseSettingChanges(body)
	if p != nil {
		writeProblem(w, http.StatusBadRequest, *p)
		return
	}
	if p := installerKeys(tk, changes); p != nil {
		writeProblem(w, http.StatusForbidden, *p)
		return
	}
	if p := s.zoneConfirm(r, changes); p != nil {
		writeProblem(w, http.StatusConflict, *p)
		return
	}
	for _, c := range changes {
		var err error
		if c.reset {
			err = a.Reset(c.key, tk.Name)
		} else {
			err = a.Set(c.key, c.value, tk.Name)
		}
		if err != nil {
			s.internal(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, s.settingsBody(a, tk))
}

// installerKeys: an expert changes only the tuning knobs — one installer
// key refuses the whole body (nothing is stored).
func installerKeys(tk Token, changes []settingChange) *problem {
	if roleLevel[tk.Role] >= roleLevel[RoleInstaller] {
		return nil
	}
	for _, c := range changes {
		if !settings.Expert(c.key) {
			return &problem{
				Title: "Insufficient role", Code: "insufficient_role",
				Detail:       c.key + " needs an installer token",
				RequiredRole: string(RoleInstaller), TokenRole: string(tk.Role),
			}
		}
	}
	return nil
}

// settingChange is one validated change (reset: back to the file's or
// the default value).
type settingChange struct {
	key, value string
	reset      bool
}

// parseSettingChanges validates a PUT body: every key known and
// UI-settable, every value well-formed — or the first problem.
func parseSettingChanges(body map[string]json.RawMessage) ([]settingChange, *problem) {
	var changes []settingChange
	for _, k := range settingsOrder {
		raw, present := body[k]
		if !present {
			continue
		}
		delete(body, k)
		if k == settings.KeyForwardMode {
			return nil, badFieldPtr(k, "set with PUT /forward-mode (it applies to the cloud connection at once)")
		}
		v, err := settingValue(k, raw)
		if err != nil {
			return nil, badFieldPtr(k, err.Error())
		}
		changes = append(changes, settingChange{key: k, value: v, reset: string(raw) == "null"})
	}
	for k := range body {
		return nil, badFieldPtr(k, "not a setting the UI can change")
	}
	return changes, nil
}

// settingValue converts one JSON value to the setting's stored form
// ("" = null, a reset) and validates it.
func settingValue(key string, raw json.RawMessage) (string, error) {
	if string(raw) == "null" {
		return "", nil
	}
	switch key {
	case settings.KeyNotifyWebhook, settings.KeyNotifyNtfy:
		return urlValue(raw)
	case settings.KeyCapacityWh:
		var n int
		if err := json.Unmarshal(raw, &n); err != nil || n < 1 || n > settings.MaxCapacityWh {
			return "", fmt.Errorf("must be a whole number of Wh from 1 to %d, or null", settings.MaxCapacityWh)
		}
		return strconv.Itoa(n), nil
	case settings.KeyWriteDelay, settings.KeySocThreshold, settings.KeySocHorizon, settings.KeySocPValue, settings.KeySocWindow,
		settings.KeyUpdateInterval:
		var f float64
		if err := json.Unmarshal(raw, &f); err != nil {
			return "", errors.New("must be a number, or null")
		}
		v := strconv.FormatFloat(f, 'g', -1, 64)
		if err := settings.Check(key, v); err != nil {
			return "", err
		}
		return v, nil
	case settings.KeySiteTimeZone:
		return zoneValue(raw)
	case settings.KeyMeter2Label:
		return labelValue(raw)
	default: // the booleans
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return "", fmt.Errorf("must be true, false or null")
		}
		return strconv.FormatBool(b), nil
	}
}

// urlValue is a notification URL value ("" = off).
func urlValue(raw json.RawMessage) (string, error) {
	var u string
	if err := json.Unmarshal(raw, &u); err != nil {
		return "", fmt.Errorf("must be a URL, \"\" (off) or null")
	}
	if err := settings.ValidateURL(u); err != nil {
		return "", err
	}
	return u, nil
}

// labelValue is a site.meter2_label value: a preset or the installer's
// own words.
func labelValue(raw json.RawMessage) (string, error) {
	var l string
	if err := json.Unmarshal(raw, &l); err != nil {
		return "", errors.New("must be ac_solar, generator or a label of up to 32 characters, or null")
	}
	if err := settings.Check(settings.KeyMeter2Label, l); err != nil {
		return "", err
	}
	return l, nil
}

// zoneValue is a site.time_zone value: an IANA zone name.
func zoneValue(raw json.RawMessage) (string, error) {
	var z string
	if err := json.Unmarshal(raw, &z); err != nil {
		return "", errors.New("must be a time zone name (Area/City, e.g. Australia/Melbourne), or null")
	}
	if err := settings.Check(settings.KeySiteTimeZone, z); err != nil {
		return "", err
	}
	return z, nil
}

// Check statuses (the setup checklist's and readiness's).
const (
	checkOK      = "ok"
	checkWarn    = "warn"
	checkFail    = "fail"
	checkPending = "pending"
)

// The exported check statuses, for the host's own readiness checks.
const (
	CheckOK      = checkOK
	CheckWarn    = checkWarn
	CheckFail    = checkFail
	CheckPending = checkPending
)

// ReadyInverterConnected is the readiness check for live inverter
// readings. The OpenStick wizard reads it (D-031).
const ReadyInverterConnected = "inverter_connected"

// Check is one line of the setup checklist or the readiness report: a
// fix whenever it is not ok.
type Check struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
	// Action is a one-press remedy the UI offers (installer):
	// "set_clock" sets the inverter's clock to the site's time.
	Action string `json:"action,omitempty"`
}

type setupView struct {
	Completed bool    `json:"completed"`
	Checks    []Check `json:"checks"`
	Battery   struct {
		// Modules is the battery module count from the inverter's
		// module snapshot (null until it arrives).
		Modules    *int `json:"modules"`
		CapacityWh *int `json:"capacity_wh"`
	} `json:"battery"`
}

// handleSetup is GET /setup (installer): the first-run checklist.
func (s *Server) handleSetup(w http.ResponseWriter, _ *http.Request, _ Token) {
	a, ok := s.admin(w)
	if !ok {
		return
	}
	qs, err := s.st.QueryState()
	if err != nil {
		s.internal(w, err)
		return
	}
	st, err := s.buildState(qs)
	if err != nil {
		s.internal(w, err)
		return
	}
	out := setupView{Completed: a.SetupCompleted()}
	out.Checks = s.setupChecks(st)
	if p60, ok, err := s.st.LatestPayload(classSnap60); err == nil && ok {
		if n := len(identity.FromSnapshots(nil, p60).BatterySNs); n > 0 {
			out.Battery.Modules = &n
		}
	}
	if c := a.Get().CapacityWh; c > 0 {
		out.Battery.CapacityWh = &c
	}
	writeJSON(w, http.StatusOK, out)
}

// setupChecks is the first-run checklist for a state (also in a problem
// report's facts).
func (s *Server) setupChecks(st State) []Check {
	return []Check{
		s.checkConnected(st),
		checkIdentified(st.Identity),
		checkProfile(st.Identity),
		s.checkClock(),
		checkCapacity(s.info.capacityWh()),
	}
}

// handleSetupComplete is POST /setup/complete (installer).
func (s *Server) handleSetupComplete(w http.ResponseWriter, _ *http.Request, tk Token) {
	a, ok := s.admin(w)
	if !ok {
		return
	}
	if err := a.CompleteSetup(tk.Name); err != nil {
		s.internal(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// gatewayFix is the usual cure for an inverter that does not reach us.
const gatewayFix = "Set the inverter's network gateway to this machine's address (in the dongle's " +
	"network settings, or with a DHCP reservation whose gateway option points here), then wait a few " +
	"minutes or power-cycle the inverter. If it still does not connect, check the gate with " +
	"`sudo /usr/local/lib/foxess-local/apply-gate.sh show` and `journalctl -u foxess-local`."

// noInverterFix is the cure for an install without an inverter address.
const noInverterFix = "On an OpenStick appliance, the setup wizard sets it. Otherwise re-run the installer " +
	"with the inverter's address: `sudo ./installer.sh install --inverter-ip <address>`."

func (s *Server) checkConnected(st State) Check {
	c := Check{ID: ReadyInverterConnected, Title: "Inverter connected"}
	switch {
	case !st.Online && s.noInverter != nil && s.noInverter():
		c.Status, c.Detail, c.Fix = checkWarn, "no inverter address is configured yet", noInverterFix
	case st.Online:
		c.Status, c.Detail = checkOK, "connected, with readings in the last 15 seconds"
	case st.AsOf != nil && s.clock().Sub(*st.AsOf) < OnlineWindow:
		c.Status, c.Detail = checkPending, "data arrived just now; waiting for the live readings to settle"
	case st.AsOf != nil:
		ago := s.clock().Sub(*st.AsOf).Round(time.Second)
		c.Status, c.Detail, c.Fix = checkFail, fmt.Sprintf("no readings for %s (last at %s)", ago, st.AsOf.In(time.Local).Format("2006-01-02 15:04")), gatewayFix
	default:
		c.Status, c.Detail, c.Fix = checkFail, "the inverter has not connected yet", gatewayFix
	}
	return c
}

func checkIdentified(id Identity) Check {
	c := Check{ID: "identified", Title: "Inverter identified"}
	if id.Model == "" {
		c.Status, c.Detail = checkPending, "the model and firmware arrive within a few minutes of connecting"
		c.Fix = "wait for the inverter to connect (it sends its details every few minutes)"
		return c
	}
	c.Status = checkOK
	c.Detail = fmt.Sprintf("%s, firmware %s / %s / %s", id.Model, id.Firmware.Master, id.Firmware.Slave, id.Firmware.Manager)
	return c
}

func checkProfile(id Identity) Check {
	c := Check{ID: "verified", Title: "Settings support"}
	switch {
	case id.VerifiedProfile != nil:
		c.Status, c.Detail = checkOK, "a verified model and firmware: readings and settings are supported"
	case id.Model == "":
		c.Status, c.Detail = checkPending, "known once the inverter is identified"
	default:
		c.Status = checkWarn
		c.Detail = "not yet verified: live readings are shown (marked tentative); settings stay locked"
		c.Fix = "Help verify it: System → Help → Download bundle, and share it in a GitHub issue."
	}
	return c
}

// whPerKWh converts the stored Wh for display.
const whPerKWh = 1000

func checkCapacity(capWh int) Check {
	c := Check{ID: "battery_capacity", Title: "Battery capacity"}
	if capWh > 0 {
		c.Status, c.Detail = checkOK, fmt.Sprintf("%.2f kWh usable", float64(capWh)/whPerKWh)
		return c
	}
	c.Status, c.Detail = checkFail, "not set: the stored energy cannot be shown"
	c.Fix = "Enter the battery's usable capacity (modules × kWh per module; an EQ4800 module is 4.66 kWh)."
	return c
}

// quirkItem is one known quirk with its effective state (GET /quirks).
type quirkItem struct {
	quirks.Quirk
	Enabled bool   `json:"enabled"`
	Source  string `json:"source"` // default | config | ui
}

type quirksView struct {
	Quirks []quirkItem `json:"quirks"`
}

// handleQuirks is GET /quirks: every known inverter quirk, what turning
// it on does, where it was seen, its default and its effective state.
// Read-only; an expert changes one with PUT /settings (quirks.<id>).
func (s *Server) handleQuirks(w http.ResponseWriter, _ *http.Request, _ Token) {
	a, ok := s.admin(w)
	if !ok {
		return
	}
	v, src := a.Get(), a.Sources()
	out := quirksView{Quirks: []quirkItem{}}
	for _, q := range quirks.All() {
		out.Quirks = append(out.Quirks, quirkItem{Quirk: q, Enabled: v.Quirks[q.ID], Source: string(src[quirks.Key(q.ID)])})
	}
	writeJSON(w, http.StatusOK, out)
}
