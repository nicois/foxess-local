package api

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/nicois/prox/pkg/capture"

	"foxesslocal/internal/frame"
	"foxesslocal/internal/store"
)

// The frame verdicts (store.Decision; framesvc writes them).
const (
	decisionForwarded = store.DecisionForwarded
	decisionDropped   = store.DecisionDropped
	decisionInjected  = store.DecisionInjected
	decisionResidue   = store.DecisionResidue
)

// auditSourceCloud is the /audit source identity for the policy
// decisions (cloud-origin frames; the spec's example rows).
const auditSourceCloud = "cloud"

// auditActionDrop is the spec's action for a policy drop.
const auditActionDrop = "drop"

// auditScanCombos are the frame sets that carry a policy decision
// (s2c only — the policy judges the upstream direction; the action is
// the spec's allow/drop).
var auditScanCombos = [2]struct {
	decision store.Decision
	action   string
	dir      capture.Dir
}{
	{decisionForwarded, auditActionAllow, capture.DirS2C},
	{decisionDropped, auditActionDrop, capture.DirS2C},
}

// messageDirs are the frame sets /messages lists (both directions,
// every decision — the raw log).
var messageDirs = [6]struct {
	decision store.Decision
	dir      capture.Dir
}{
	{decisionForwarded, capture.DirS2C},
	{decisionDropped, capture.DirS2C},
	{decisionForwarded, capture.DirC2S},
	{decisionDropped, capture.DirC2S},
	{decisionInjected, capture.DirS2C},
	{decisionResidue, capture.DirC2S},
}

// auditEntry is the spec's AuditEntry (the /audit items).
type auditEntry struct {
	TS         time.Time `json:"ts"`
	Source     string    `json:"source"`
	SourceRole *string   `json:"source_role"`
	Action     string    `json:"action"`
	Summary    string    `json:"summary"`
	Confirmed  string    `json:"confirmed"`
}

// handleAudit is GET /audit (spec listAudit): the merged policy
// decisions + operator-command rows, filtered and paged. The policy
// decisions are frame-level (the cloud's Modbus requests, one per
// frame): below expert they are left out and cloud_hidden says so
// (owner, 2026-11-19).
func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request, tk Token) {
	page, pb, ok := parsePage(r)
	if !ok {
		writeProblem(w, http.StatusBadRequest, pb)
		return
	}
	q := r.URL.Query()
	source, action := q.Get("source"), q.Get("action")
	hideCloud := roleLevel[tk.Role] < roleLevel[RoleExpert]
	out, err := s.auditEntries(page, source, action, hideCloud)
	if err != nil {
		s.internal(w, err)
		return
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TS.After(out[j].TS) })
	if len(out) > page.limit {
		out = out[:page.limit]
	}
	writeJSON(w, http.StatusOK, struct {
		Items       []auditEntry `json:"items"`
		CloudHidden bool         `json:"cloud_hidden,omitempty"`
	}{out, hideCloud})
}

// auditEntries is one page of each source, before the merge: the policy
// decisions (the s2c frame verdicts, source cloud) and the
// operator-command rows (source = the token name). Each part is at most
// one page from the newest end, so the merged page is exact and the cost
// follows the page size, not the log.
func (s *Server) auditEntries(page pageParams, source, action string, hideCloud bool) ([]auditEntry, error) {
	out := []auditEntry{}
	if !hideCloud && (source == "" || source == auditSourceCloud) {
		pol, err := s.policyEntries(page, action)
		if err != nil {
			return nil, err
		}
		out = pol
	}
	if source == auditSourceCloud {
		return out, nil
	}
	cmds, err := s.commandEntries(page, func(e auditEntry) bool {
		return (source == "" || e.Source == source) && (action == "" || e.Action == action)
	})
	if err != nil {
		return nil, err
	}
	return append(out, cmds...), nil
}

// policyEntries is one page of the policy decisions (the s2c frame
// verdicts), limited to the action filter.
func (s *Server) policyEntries(page pageParams, action string) ([]auditEntry, error) {
	var sets []store.FrameSet
	actionOf := map[store.Decision]string{}
	for i := range auditScanCombos {
		c := auditScanCombos[i]
		if action != "" && action != c.action {
			continue
		}
		sets = append(sets, store.FrameSet{Dir: c.dir, Decision: c.decision})
		actionOf[c.decision] = c.action
	}
	recs, err := s.st.FramesPage(store.FramePage{
		Sets: sets, Since: page.since, Until: page.until, Before: page.before, Limit: page.limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]auditEntry, 0, len(recs))
	for j := range recs {
		out = append(out, policyEntry(actionOf[recs[j].Decision], &recs[j]))
	}
	return out, nil
}

// commandEntries pages the operator-command audit rows from the newest
// end until a page of them passes keep (or the log is exhausted).
func (s *Server) commandEntries(page pageParams, keep func(auditEntry) bool) ([]auditEntry, error) {
	var out []auditEntry
	before := page.before
	for len(out) < page.limit {
		evts, err := s.st.AuditPage(store.AuditQuery{
			Kinds: []string{auditKindCommand}, Since: page.since, Until: page.until,
			Before: before, Limit: page.limit,
		})
		if err != nil {
			return nil, err
		}
		for i := range evts {
			if e, ok := parseCommandAudit(evts[i]); ok && keep(e) {
				out = append(out, e)
			}
		}
		if len(evts) < page.limit {
			break
		}
		before = evts[len(evts)-1].TS
	}
	return out, nil
}

// policyEntry maps one policy-decision frame to the spec's AuditEntry
// (the summary is the decoded operation; the drop reason is appended).
func policyEntry(action string, rec *store.FrameRecord) auditEntry {
	confirmed := confirmedNotApplicable
	if action == auditActionAllow && rec.Class == classMbWrite {
		confirmed = confirmedUnknown
	}
	summary := summarizeFrame(rec)
	if rec.Reason != "" {
		summary += " [" + rec.Reason + "]"
	}
	return auditEntry{
		TS:        rec.TS,
		Source:    auditSourceCloud,
		Action:    action,
		Summary:   summary,
		Confirmed: confirmed,
	}
}

// parseCommandAudit decodes one operator-command audit row.
func parseCommandAudit(ev store.AuditEvent) (auditEntry, bool) {
	var d commandAuditDetail
	if err := json.Unmarshal([]byte(ev.Detail), &d); err != nil {
		return auditEntry{}, false
	}
	role := d.SourceRole
	return auditEntry{
		TS:         ev.TS,
		Source:     d.Source,
		SourceRole: &role,
		Action:     d.Action,
		Summary:    d.Summary,
		Confirmed:  d.Confirmed,
	}, true
}

// summarizeFrame is the human-readable operation (the spec example:
// "work-mode feed-in (41000=1)").
func summarizeFrame(rec *store.FrameRecord) string {
	mb, err := frame.ParseModbus(rec.Payload)
	if err != nil {
		return string(rec.Class)
	}
	switch mb.Fn {
	case fn06:
		switch mb.Addr {
		case regWorkModeAddr:
			mode := workModeSelfUse
			if mb.Value == 1 {
				mode = workModeFeedIn
			}
			return "work-mode " + mode + " (41000=" + strconv.Itoa(mb.Value) + ")"
		case regMeterCompAddr:
			return "meter-compensation " + strconv.Itoa(int16Value(mb.Value)) + "W (41029=" + strconv.Itoa(mb.Value) + ")"
		case regImportLimAddr:
			return "import-limit " + strconv.Itoa(mb.Value) + "W (41021=" + strconv.Itoa(mb.Value) + ")"
		case regExportLimAddr:
			return "export-limit " + strconv.Itoa(mb.Value) + "W (41023=" + strconv.Itoa(mb.Value) + ")"
		}
		return "write (addr=" + strconv.Itoa(mb.Addr) + " val=" + strconv.Itoa(mb.Value) + ")"
	case fn10:
		switch mb.Addr {
		case regModeEnAddr:
			return "schedule master flag (48000 r0)"
		case regSlotTableAddr:
			return "schedule push (48010 x80)"
		}
		return "write (addr=" + strconv.Itoa(mb.Addr) + " x" + strconv.Itoa(mb.Count) + ")"
	case fn03:
		return "read (addr=" + strconv.Itoa(mb.Addr) + " x" + strconv.Itoa(mb.Count) + ")"
	}
	return string(rec.Class)
}

// messageEntry is the spec's MessageEntry (the /messages items).
type messageEntry struct {
	TS      time.Time `json:"ts"`
	Dir     string    `json:"dir"`
	Frame   string    `json:"frame"`
	Length  int       `json:"length"`
	Hex     string    `json:"hex"`
	Summary *string   `json:"summary"`
	// Decision is what happened to the frame: forwarded, dropped,
	// injected (written by this app) or residue (a partial frame at
	// close).
	Decision string `json:"decision"`
	// Reason is the policy verdict or routing note (null when none): e.g.
	// "mode:telemetry_only read:…", "mode:telemetry_only deny:…", or an
	// answer to this app's own request "kept from the cloud".
	Reason *string `json:"reason"`
	// Origin is who injected an injected frame (gateway, bootstrap,
	// trigread); null otherwise.
	Origin *string `json:"origin"`
}

// Snapshot frame classes (the device-info snapshot series; the spec's
// FrameType names equal the class names).
const (
	classSnap01 = frame.KindSnap01
	classSnap02 = frame.KindSnap02
	classSnap04 = frame.KindSnap04
	classSnap60 = frame.KindSnap60
	classSnap61 = frame.KindSnap61
	classSnap65 = frame.KindSnap65
)

// Direction texts (the spec's Direction enum; the store dir maps to
// them and the /messages dir filter accepts them).
const (
	dirInverterToCloud = "inverter-to-cloud"
	dirCloudToInverter = "cloud-to-inverter"
)

// frameTypeUnknown is the spec's FrameType catch-all (the store class
// has no confirmed mapping yet).
const frameTypeUnknown = "unknown"

// frameTypeClasses maps the spec's FrameType filter values to the
// store class sets (firmware-metadata / firmware-chunk match nothing
// yet — the wire classes are unconfirmed, a recon item).
var frameTypeClasses = map[string][]frame.Kind{
	"telemetry-150":        {classTele},
	"keepalive-135":        {classKeepalive},
	string(classSnap01):    {classSnap01},
	string(classSnap02):    {classSnap02},
	string(classSnap04):    {classSnap04},
	string(classSnap60):    {classSnap60},
	string(classSnap61):    {classSnap61},
	string(classSnap65):    {classSnap65},
	"rssi":                 {classRSSI},
	"modbus-read":          {classMbread},
	"modbus-write":         {classMbWrite},
	"modbus-read-response": {classCfg91, classMbreadResp},
	"write-ack":            {classMbecho},
	"registration":         {classRegister},
	"firmware-metadata":    {},
	"firmware-chunk":       {},
	frameTypeUnknown:       {classOther, frame.KindBootstrapResp, frame.KindCfg40, frame.KindSerialQuery},
}

// dirText maps the store dirs to the spec's Direction enum.
var dirText = map[capture.Dir]string{
	capture.DirC2S: dirInverterToCloud,
	capture.DirS2C: dirCloudToInverter,
}

// handleMessages is GET /messages (spec listMessages): the raw frame
// log (both directions, every decision), filtered and paged.
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request, _ Token) {
	page, pb, ok := parsePage(r)
	if !ok {
		writeProblem(w, http.StatusBadRequest, pb)
		return
	}
	dir, classSet, perr := parseMessagesFilter(r)
	if perr != nil {
		writeProblem(w, http.StatusBadRequest, *perr)
		return
	}
	recs, err := s.st.FramesPage(store.FramePage{
		Sets: messageSets(dir), Classes: classSet,
		Since: page.since, Until: page.until, Before: page.before, Limit: page.limit,
	})
	if err != nil {
		s.internal(w, err)
		return
	}
	out := make([]messageEntry, 0, len(recs))
	for i := range recs {
		out = append(out, messageEntryOf(&recs[i]))
	}
	writeJSON(w, http.StatusOK, struct {
		Items []messageEntry `json:"items"`
	}{out})
}

// parseMessagesFilter validates the /messages dir + frame query
// params (empty = no filter; a bad value is a 400 problem) and returns
// the dir filter text and the classes (nil = no filter).
func parseMessagesFilter(r *http.Request) (string, []frame.Kind, *problem) {
	q := r.URL.Query()
	dir := q.Get("dir")
	switch dir {
	case "", dirInverterToCloud, dirCloudToInverter:
	default:
		return "", nil, badFieldPtr("dir", "must be inverter-to-cloud or cloud-to-inverter")
	}
	set, err := frameClassSet(q.Get("frame"))
	if err != nil {
		return "", nil, err
	}
	return dir, set, nil
}

// messageSets are the frame-log slices /messages lists for the dir
// filter (both directions when it is empty).
func messageSets(dir string) []store.FrameSet {
	sets := make([]store.FrameSet, 0, len(messageDirs))
	for i := range messageDirs {
		d := messageDirs[i]
		switch {
		case dir == dirInverterToCloud && d.dir != capture.DirC2S:
			continue
		case dir == dirCloudToInverter && d.dir != capture.DirS2C:
			continue
		}
		sets = append(sets, store.FrameSet{Dir: d.dir, Decision: d.decision})
	}
	return sets
}

// frameClassSet maps one spec FrameType filter to its classes (nil =
// no filter; an unknown type is a 400; a type with no confirmed class
// is an empty, non-nil list that matches nothing).
func frameClassSet(frameType string) ([]frame.Kind, *problem) {
	if frameType == "" {
		return nil, nil
	}
	classes, ok := frameTypeClasses[frameType]
	if !ok {
		return nil, badFieldPtr("frame", "unknown frame type "+frameType)
	}
	return append([]frame.Kind{}, classes...), nil
}

// messageEntryOf maps one frame record to the spec's MessageEntry
// (the summary is the decoded JSON when present, else absent).
func messageEntryOf(rec *store.FrameRecord) messageEntry {
	e := messageEntry{
		TS:     rec.TS,
		Dir:    dirText[rec.Dir],
		Frame:  frameTypeOf(rec.Class),
		Length: len(rec.Payload),
		Hex:    hex.EncodeToString(rec.Payload),
	}
	if len(rec.Decoded) > 0 {
		sum := string(rec.Decoded)
		e.Summary = &sum
	}
	e.Decision = string(rec.Decision)
	if rec.Reason != "" {
		r := rec.Reason
		e.Reason = &r
	}
	if rec.Origin != "" {
		o := rec.Origin
		e.Origin = &o
	}
	return e
}

// frameTypeOf maps one store class to the spec's FrameType.
func frameTypeOf(class frame.Kind) string {
	// frame.Kind is open-ended (SnapKind): the known classes, then the default
	//exhaustive:ignore
	switch class {
	case classTele:
		return "telemetry-150"
	case classKeepalive:
		return "keepalive-135"
	case classRSSI:
		return "rssi"
	case classMbread:
		return "modbus-read"
	case classMbWrite:
		return "modbus-write"
	case classCfg91, classMbreadResp:
		return "modbus-read-response"
	case classMbecho:
		return "write-ack"
	case classRegister:
		return "registration"
	case classSnap01, classSnap02, classSnap04, classSnap60, classSnap61, classSnap65:
		return string(class)
	default:
		return frameTypeUnknown
	}
}
