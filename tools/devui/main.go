// Command devui serves the web UI and the real API over a throwaway store
// seeded with a plausible KH10 day — for UI work and screenshots without
// an inverter, the egress gate or root. It never touches the network
// beyond its listen address and never writes outside its temp directory.
//
//	go run ./tools/devui [-addr 127.0.0.1:18999] [-model KH10] [-master 1.73]
//
// It prints an installer token; the UI's sign-in takes it.
//
// Load tests and profiling: -db serves an existing database (e.g. one
// built by tools/synthdb) instead of seeding a temporary one, and -pprof
// serves the Go profiler on a second (loopback) address:
//
//	go run ./tools/devui -db /tmp/synth.db -pprof 127.0.0.1:6060
package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/http/pprof"
	"os"
	"path/filepath"
	"time"

	"github.com/nicois/prox/pkg/capture"

	apispec "foxesslocal/api"
	"foxesslocal/internal/api"
	"foxesslocal/internal/config"
	"foxesslocal/internal/forward"
	"foxesslocal/internal/frame"
	"foxesslocal/internal/gateway"
	"foxesslocal/internal/mcp"
	"foxesslocal/internal/notify"
	"foxesslocal/internal/policy"
	"foxesslocal/internal/settings"
	"foxesslocal/internal/socest"
	"foxesslocal/internal/store"
	"foxesslocal/internal/web"
	"foxesslocal/internal/wincap"
)

// dropInjector swallows command frames: there is no inverter.
type dropInjector struct{}

func (dropInjector) InjectS2C(string, []byte) error { return nil }

func main() {
	addr := flag.String("addr", "127.0.0.1:18999", "listen address")
	model := flag.String("model", "KH10", "model string to seed")
	master := flag.Uint("master", 0x173, "master firmware register value (0x173 = 1.73)")
	allPages := flag.Bool("public-all-pages", false, "tokenless sessions may open every page ([access] public_all_pages)")
	capacity := flag.Int("capacity", 0, "battery capacity, Wh, as the config file would set it (0 = unset: the setup wizard asks)")
	dbPath := flag.String("db", "", "serve this existing database instead of a seeded temporary one")
	pprofAddr := flag.String("pprof", "", "serve the Go profiler on this address (e.g. 127.0.0.1:6060)")
	eps := flag.Int("eps", 0, "simulated EPS (backup) port load, W")
	flag.Parse()
	epsW = int32(*eps) //nolint:gosec // G115: a small wattage flag

	dir, err := os.MkdirTemp("", "foxess-devui-")
	if err != nil {
		log.Fatal(err)
	}
	st := openStore(*dbPath, dir, *model, uint16(*master)) //nolint:gosec // G115: flag is a register value
	if *pprofAddr != "" {
		go servePprof(*pprofAddr)
	}

	gw := gateway.New(dropInjector{}, time.Now)
	set, fwd := devSettings(st, *allPages, *capacity)
	maint := forward.NewMaintenance(st, time.Now, func() policy.ForwardMode { m, _, _ := fwd.Get(); return m }, func(string, string) {})
	tok, err := api.NewTokenStore(filepath.Join(dir, "tokens.json"), time.Now)
	if err != nil {
		log.Fatal(err)
	}
	_, plain, err := tok.Create("devui", api.RoleInstaller)
	if err != nil {
		log.Fatal(err)
	}
	bus := api.NewBus()
	// notifications go to the targets set in Settings, as in the service
	notifier := notify.New(func() notify.Targets {
		v := set.Get()
		return notify.Targets{WebhookURL: v.NotifyWebhook, NtfyURL: v.NotifyNtfy}
	}, time.Now)
	go notifier.Run(context.Background())
	var est socest.Estimator
	apiH := api.New(st, gw, maint, tok, fwd, bus, time.Now, api.WithSocEstimate(&est), api.WithInfo(api.ServerInfo{
		Version: "devui", RawHours: 72, BucketDays: 365, Settings: set,
	}), api.WithNotifier(notifier),
		api.WithCaptures(wincap.New(filepath.Join(dir, "captures"), 256<<20, time.Now, "devui")))
	go live(st, bus, &est, func() int { return set.Get().CapacityWh })
	mux := http.NewServeMux()
	mux.Handle("/api/v1/{rest...}", apiH)
	if m, err := mcp.New(apiH, apispec.OpenAPI, "devui"); err == nil {
		mux.Handle("/mcp", m)
	}
	mux.Handle("/", web.Handler())
	fmt.Printf("devui: http://%s  (store %s)\ninstaller token: %s\n", *addr, dir, plain)
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

// devSettings are live settings, as in the service (the setup wizard
// and Settings); capacity seeds the battery capacity as a config value
// would.
func devSettings(st *store.Store, allPages bool, capacity int) (*settings.Settings, *forward.State) {
	cfg := &config.Config{}
	cfg.Access.PublicReadOnly = true
	cfg.Access.PublicAllPages = allPages
	cfg.Battery.CapacityWh = capacity
	cfg.Forward.Mode = policy.ModeOff
	set, err := settings.New(st, cfg, time.Now)
	if err != nil {
		log.Fatal(err)
	}
	fwd := forward.New(st, time.Now, set.Get().ForwardMode)
	fwd.OnChange(func(m policy.ForwardMode, _ string) error { return set.Persist(settings.KeyForwardMode, string(m)) })
	return set, fwd
}

// openStore opens dbPath (an existing database, e.g. from tools/synthdb)
// or a seeded temporary one in dir.
func openStore(dbPath, dir, model string, master uint16) *store.Store {
	path := dbPath
	if path == "" {
		path = filepath.Join(dir, "devui.db")
	}
	t0 := time.Now()
	st, err := store.Open(path, "devui")
	if err != nil {
		log.Fatal(err)
	}
	if dbPath == "" {
		seed(st, time.Now(), model, master)
	} else {
		fmt.Printf("devui: opened %s in %v\n", path, time.Since(t0).Round(time.Millisecond))
	}
	return st
}

// servePprof serves the Go profiler on its own mux.
func servePprof(addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	fmt.Printf("devui: pprof on http://%s/debug/pprof/\n", addr)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Print(srv.ListenAndServe())
}

// tele is a plausible operating point at time t (a sunny day).
// It models the KH's measured balance (2026-11-17): a ~3 % conversion
// loss; the EPS (backup) port load (-eps) is part of both the AC output
// and the household load; home = AC output + grid.
func tele(t time.Time) frame.Tele {
	h := float64(t.Hour()) + float64(t.Minute())/60
	sun := math.Max(0, math.Sin((h-6)/13*math.Pi))
	pv := int32(9000 * sun)
	load := int32(600+400*math.Sin(h/3)) + epsW
	batt := (pv - load) / 2
	ac := int32(float64(pv-batt) * 0.97)
	grid := load - ac
	soc := 40 + 50*sun
	return frame.Tele{
		Sol: pv, Grid: grid, Haus: load, Acport: ac, Batt: batt, Eps: epsW, Soc: byte(soc),
		Temps: &frame.Temps{
			BattDeciC: int(220 + 60*sun), InvDeciC: int(280 + 150*sun), AmbDeciC: int(180 + 90*sun),
			BMSChargeDeciA: int(500 - 3*soc), // the limit tapers as the battery fills
		},
	}
}

// maybeSnap61 records the battery snapshot the inverter pushes about
// every 5 minutes, with the BMS's lowest / highest cell temperature
// (tag 1874, 0.1 °C LE) trailing the day's warmth.
func maybeSnap61(st *store.Store, t time.Time, stepS int64) {
	if t.Unix()%300 >= stepS {
		return
	}
	h := float64(t.Hour()) + float64(t.Minute())/60
	warm := math.Max(0, math.Sin((h-8)/14*math.Pi))
	p := make([]byte, 626)
	copy(p, "DEVUI0000000001")
	p[64], p[65] = 0x18, 0x74
	binary.LittleEndian.PutUint16(p[66:], uint16(200+50*warm)) // highest cell
	binary.LittleEndian.PutUint16(p[68:], uint16(160+40*warm)) // lowest cell
	st.RecordFrame(store.FrameRecord{
		ConnID: devuiConn, Seq: int(t.Unix()), Dir: capture.DirC2S, TS: t,
		Class: frame.KindSnap61, Plen: len(p), Payload: p, Decision: store.DecisionForwarded,
	})
}

// devuiConn is the demo store's one connection.
const devuiConn = "devui-1"

// seedActivity seeds the Activity page: two operator actions and a
// blocked firmware upgrade task.
func seedActivity(st *store.Store, seq int, now time.Time) {
	seedFirmwareTask(st, seq, now)
	st.RecordAudit(now.Add(-40*time.Minute), "forward-mode", "→ off by installer")
	st.RecordAudit(now.Add(-20*time.Minute), "write-override", "revoked by installer")
}

// seedFirmwareTask records a cloud firmware upgrade task, blocked outside
// a maintenance window (profile §6; placeholder address), for the
// Messages and Activity pages.
func seedFirmwareTask(st *store.Store, seq int, now time.Time) {
	task := frame.JSONMsg(0x17, []byte(`{"task_type":"upgrade","data":{"mcu_type":"A","notify_url":"https://example.invalid/status","url":"https://example.invalid/private/firmware/00000000-0000-0000-0000-000000000000?taskId=1","filename":"10-300-11057-37_K10_Manager_V1.71.bin","crc":59102,"size":764904}}`))
	st.RecordFrame(store.FrameRecord{
		ConnID: devuiConn, Seq: seq, Dir: capture.DirS2C, TS: now.Add(-30 * time.Second),
		Class: frame.KindOther, Plen: len(task), Payload: task, Decision: store.DecisionDropped,
		Reason: "mode:telemetry_only deny:kind=other", Decoded: frame.DescribeOther(task),
	})
}

// epsW is the simulated EPS (backup) port load (-eps).
var epsW int32

// live appends a telemetry row every 5 s (the poller's cadence) and
// publishes the snapshot event the UI's live view runs on (the app's
// seedClock records the inverter's clock reading Melbourne wall time (no
// site zone is set, so Setup asks for one).
func seedClock(st *store.Store, now time.Time) {
	mel, err := time.LoadLocation("Australia/Melbourne")
	if err != nil {
		return
	}
	w := now.In(mel)
	//nolint:gosec // G115: calendar fields fit uint16
	st.RecordRegisters(now, 0x9C40, []uint16{
		uint16(w.Year()), uint16(w.Month()), uint16(w.Day()),
		uint16(w.Hour()), uint16(w.Minute()), uint16(w.Second()),
	})
}

// shape: api.SnapshotEvent, as cmd/foxess-local publishes it).
func live(st *store.Store, bus *api.Bus, est *socest.Estimator, capWh func() int) {
	for t := range time.Tick(5 * time.Second) {
		maybeSnap61(st, t, 5)
		tl := tele(t)
		st.RecordTelemetry(devuiConn, t, tl)
		state := "idle"
		switch {
		case tl.Batt > 0:
			state = "charging"
		case tl.Batt < 0:
			state = "discharging"
		}
		bus.PublishSnapshot(api.SnapshotEvent{
			TS: t,
			Power: api.Power{
				PVW: int(tl.Sol), GridW: int(tl.Grid), LoadW: int(tl.Haus),
				AcPortW: int(tl.Acport), BatteryW: int(tl.Batt), EpsW: int(tl.Eps),
			},
			SocPct: int(tl.Soc), SocEstimatePct: estimate(est, t, tl, capWh()), BatteryState: state,
		})
	}
}

// estimate feeds the SoC estimate as the service does (D-028).
func estimate(est *socest.Estimator, t time.Time, tl frame.Tele, capWh int) *float64 {
	v := est.Observe(t, int(tl.Soc), int(tl.Batt), capWh)
	if capWh <= 0 {
		return nil
	}
	v = api.RoundPct(v)
	return &v
}

func seed(st *store.Store, now time.Time, model string, master uint16) {
	for t := now.Add(-2 * time.Hour); t.Before(now); t = t.Add(30 * time.Second) {
		maybeSnap61(st, t, 30)
		st.RecordTelemetry(devuiConn, t, tele(t))
	}
	regs := make([]uint16, 15)
	padded := model + "               "
	for i := range 15 {
		regs[i] = uint16(padded[i])
	}
	st.RecordRegisters(now, 30000, regs)
	st.RecordRegisters(now, 30016, []uint16{master, 0x103, 0x171})
	seedClock(st, now)
	st.RecordRegisters(now, 41000, []uint16{0})
	block := make([]uint16, 40)
	for a, v := range map[int]uint16{
		41007: 450, 41008: 450, 41010: 100, 41011: 11, 41012: 100, 41015: 3, 41016: 100,
		41022: 12500, 41024: 5000, 41027: 560, 41028: 400, 41029: 0xFFCD, 41032: 100,
	} {
		block[a-41007] = v
	}
	st.RecordRegisters(now, 41007, block)
	st.RecordRegisters(now, 41200, []uint16{10000, 7000, 0}) // load / generation limits
	st.RecordRegisters(now, 48000, []uint16{1, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	table := make([]uint16, 80)
	groups := [][]uint16{
		{1, 0x0B02, 0x0D3A, 6, 0x640A, 99, 9948, 0, 1, 2},
		{1, 0x1132, 0x1400, 7, 0x640A, 50, 10500, 0, 3, 2},
		{1, 0x0000, 0x173B, 1, 0x640B, 10, 0, 0, 1, 2},
	}
	for k := range 8 {
		g := []uint16{0, 0, 0, 1, 0x640A, 10, 0, 0, 0, 2}
		if k < len(groups) {
			g = groups[k]
		}
		copy(table[10*k:], g)
	}
	st.RecordRegisters(now, 48010, table)
	// the groups' parameter blocks as the owner's 2026-11-18 session left
	// them: force charge import ≤ 35 kW with solar, force discharge export
	// ≤ 10 kW without solar (r1:r2 / r3:r4 U32, r6 from-PV power)
	for k, p := range [][]uint16{
		{2, 0, 35000, 1, 34464, 0, 20000},
		{2, 1, 34464, 0, 10000, 0, 0},
		{2, 1, 34464, 1, 34464, 0, 20000},
	} {
		blk := make([]uint16, 50)
		copy(blk, p)
		st.RecordRegisters(now, 57000+50*k, blk)
	}
	snap := make([]byte, 141)
	copy(snap, "DEVUI0000000001")
	copy(snap[33:], "1.73")
	copy(snap[39:], "1.03")
	copy(snap[45:], "1.71")
	copy(snap[93:], model)
	frames := []struct {
		class frame.Kind
		dir   capture.Dir
		p     []byte
	}{
		{frame.KindSnap01, capture.DirC2S, snap},
		{frame.KindKeepalive, capture.DirC2S, []byte("K12104DEVUI0000000001")},
		{frame.KindMbread, capture.DirS2C, []byte{0x01, 0x03, 0xA0, 0x28, 0x00, 0x01}},
		{frame.KindCfg91, capture.DirC2S, []byte{0x01, 0x03, 0x02, 0x00, 0x00}},
	}
	for i, f := range frames {
		st.RecordFrame(store.FrameRecord{
			ConnID: devuiConn, Seq: i, Dir: f.dir, TS: now.Add(time.Duration(i-len(frames)) * time.Minute),
			Class: f.class, Plen: len(f.p), Payload: f.p, Decision: store.DecisionForwarded,
		})
	}
	seedActivity(st, len(frames), now)
	st.Flush()
}
