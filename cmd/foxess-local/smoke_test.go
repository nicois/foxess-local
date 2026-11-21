// The stage-1b local smoke (the pre-flight before a live install, no inverter): the
// full stack through run() — the embedded prox ladder (chanpipe legs,
// never the TPROXY listener), the scripted cloud (mirror mode: pushes a
// limit write and a firmware chunk, which full mode holds), a TLS client as the inverter, the
// REST API (tokens, work-mode command, state, maintenance) and the bus (SSE
// replay at the end). The in-memory legs keep every byte byte-exact.
package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nicois/prox/pkg/capture"
	"github.com/nicois/prox/pkg/chanpipe"
	pconfig "github.com/nicois/prox/pkg/config"
	prox "github.com/nicois/prox/pkg/prox"
	"github.com/stretchr/testify/require"

	"foxesslocal/internal/api"
	"foxesslocal/internal/config"
	"foxesslocal/internal/frame"
	"foxesslocal/internal/store"
)

// The scripted session constants (the frame builder mirrors the codec:
// start×2, dev(4), fn, plen(2) BE, payload, crc16(dev..payload) LE,
// terminator ×2 — the nibble swap of start).
var (
	smokeDevInv        = [4]byte{0x11, 0, 0, 1}
	smokeDevRegA       = [4]byte{0x2a, 0, 0x7b, 0xc5}
	smokeDevRegB       = [4]byte{0x2c, 0, 0x7b, 0xce}
	smokeDevRegC       = [4]byte{0x2b, 0, 0x7b, 0xd3}
	smokeDevS2C        = [4]byte{0x11, 0xaa, 0xbb, 0xcc}
	smokeDevEcho       = [4]byte{0x92, 0, 0, 0}
	smokeDevReadResp   = [4]byte{0x91, 0, 0, 0}                                                   // a read response: the request lead 0x11 | 0x80
	smokePduWriteLim   = []byte{0x01, 0x10, 0xa0, 0x3d, 0x00, 0x02, 0x04, 0x00, 0x00, 0x13, 0x88} // fn10 @41021 (0xA03D) x2: import 5000 as U32 (hi 0, lo 5000)
	smokePduWriteWM    = []byte{0x01, 0x06, 0xa0, 0x28, 0x00, 0x01}                               // fn06 @41000 = 1 (feed-in)
	smokePduReadWM     = []byte{0x01, 0x03, 0xa0, 0x28, 0x00, 0x01}                               // fn03 @41000 ×1 (read before write)
	smokePduReadWMResp = []byte{0x01, 0x03, 0x02, 0x00, 0x00}                                     // 41000 = 0 (self-use)
	smokeRegPayload    = []byte{0x01, 0x00, 0x01, 0x11, 0x0f, '6', '0', '9', 'W', '6', 'F', 'G', 'F', '5', 'A', 'Y', 'B', '1', '1', '7', 0x00, 0x00, 0x4c}
)

const (
	smokeStartC2S = 0x7e
	smokeStartS2C = 0x7f
	smokeFnData   = 0x10
	smokeEnvFnS2C = 0x2f // the s2c Modbus envelope fn the scripted cloud uses
)

func smokeFrame(start byte, dev [4]byte, fn byte, payload []byte) []byte {
	body := make([]byte, 0, 13+len(payload))
	body = append(body, start, start)
	body = append(body, dev[:]...)
	//nolint:gosec // G115: test frame lengths are far under 64 KiB.
	body = append(body, fn, byte(len(payload)>>8), byte(len(payload)))
	body = append(body, payload...)
	crc := frame.CRC16(body[2:])
	//nolint:gosec // G115: the byte shifts keep the values in range.
	body = append(body, byte(crc), byte(crc>>8))
	term := (start&0x0f)<<4 | start>>4
	return append(body, term, term)
}

func smokeTelePayload(sol, grid, haus, acport, batt uint32, soc byte) []byte {
	p := make([]byte, 150)
	binary.BigEndian.PutUint32(p[35:], sol)
	binary.BigEndian.PutUint32(p[39:], grid)
	binary.BigEndian.PutUint32(p[43:], haus)
	binary.BigEndian.PutUint32(p[59:], acport)
	binary.BigEndian.PutUint32(p[67:], batt)
	p[72] = 0x02 // charge state: idle
	p[79] = soc
	return p
}

func smokeRegFrames() []byte {
	var all []byte
	all = append(all, smokeFrame(smokeStartC2S, smokeDevRegA, 0x5f, smokeRegPayload)...)
	all = append(all, smokeFrame(smokeStartC2S, smokeDevRegB, 0x76, []byte{0xe1, 0x01, 0x04, 0x00})...)
	all = append(all, smokeFrame(smokeStartC2S, smokeDevRegC, 0x0c, []byte{0x00})...)
	return all
}

// smokeTestAddr presents the declared client address on the in-memory
// client leg (the ladder derives Src/SrcPort from RemoteAddr, exactly
// as on a real TPROXY socket).
type smokeTestAddr struct {
	net.Conn
	remote net.Addr
}

func (a smokeTestAddr) RemoteAddr() net.Addr { return a.remote }

// smokeCA builds a self-signed CA pair (the inverter trusts any CA —
// OQ-2; the scripted inverter verifies nothing).
func smokeCA(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "smoke-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

// smokeCloudTLS builds the scripted cloud's server identity: a
// self-signed CA plus a leaf with the IP SAN of the cloud endpoint
// (the ladder's upstream leg is a verified TLS client — the real cloud
// endpoint on 14431 speaks TLS). The validity spans the synctest
// bubble's virtual clock (testing/synctest starts groups at
// 2000-01-01 UTC): the upstream x509 check runs on group time.
func smokeCloudTLS(t *testing.T, ip string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	notBefore := time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC)
	notAfter := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: "smoke-cloud-ca"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	require.NoError(t, err)
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: "smoke-cloud"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP(ip)},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caTmpl, &leafKey.PublicKey, caKey)
	require.NoError(t, err)
	ca, err := x509.ParseCertificate(caDER)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}, pool
}

//nolint:gocognit,gocyclo,cyclop,funlen,maintidx // the stage-1b smoke is one scripted end-to-end scenario
func TestStage1bSmoke(t *testing.T) {
	dir := t.TempDir()

	certPEM, keyPEM := smokeCA(t)
	caDir := filepath.Join(dir, "ca")
	require.NoError(t, os.MkdirAll(caDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(caDir, "ca.pem"), certPEM, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(caDir, "ca-key.pem"), keyPEM, 0o600))

	pcfg := pconfig.Default()
	pcfg.Listen = "127.0.0.1:0" // the smoke drives the ladder directly (no TPROXY)
	pcfg.Ports = []int{14431}
	pcfg.DataRoot = "" // embedded: the foxess store owns persistence
	pcfg.CAPaths = []pconfig.CAPath{{CertPEM: filepath.Join(caDir, "ca.pem"), KeyPEM: filepath.Join(caDir, "ca-key.pem")}}
	p, err := prox.New(pcfg)
	require.NoError(t, err)

	// The ladder's upstream leg is a verified TLS client (the real
	// cloud endpoint speaks TLS on 14431): trust the scripted cloud's
	// CA.
	cloudCert, cloudRoots := smokeCloudTLS(t, "203.0.116.72")
	p.Ladder.UpstreamRoots = cloudRoots

	cfgPath := filepath.Join(dir, "config.toml")
	cfgTOML := fmt.Sprintf(`
[server]
inverter_listen = "127.0.0.1:0"
http_listen = "127.0.0.1:0"

[certs]
manifest = %q

[store]
path = %q

[forward]
mode = "full"

[retention]
chunk_bytes_hours = 1

[sim]
enabled = true
api_key = "smoke-key"
station_id = "ST-1"
capacity_kw = 5

[tokens]
path = %q
`, filepath.Join(dir, "cert-manifest.json"), filepath.Join(dir, "foxess.db"),
		filepath.Join(dir, "tokens.json"))
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfgTOML), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cert-manifest.json"), []byte("{}"), 0o600))

	cfg, err := config.Load(cfgPath)
	require.NoError(t, err)

	// Seed the installer token before run (same file: run's store sees
	// it; the plaintext is shown once, here).
	seed, err := api.NewTokenStore(cfg.Tokens.Path, time.Now)
	require.NoError(t, err)
	_, installerPlain, err := seed.Create("installer", api.RoleInstaller)
	require.NoError(t, err)
	installerBearer := "Bearer " + installerPlain

	h, err := run(cfg, withProx(p), withStoreOpen(
		// Pool headroom: the root-group flusher shares the DB with the
		// bubble's readers — a bubble GetConn that parks on the pool
		// creates a database/sql wait channel the flusher could send
		// on (fatal across groups); with headroom the bubble never
		// parks.
		func(path, ver string) (*store.Store, error) { return store.OpenWithMaxOpen(path, 4, ver) }))
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.ln.Close(); _ = h.st.Close() }) // LIFO: after the bubble and its assertions

	// The REST surface is driven in-process from the bubble (rest):
	// a real TCP listener would put the handler goroutines in a
	// different synctest group, and a command worker spawned there
	// could not select on the bubble's chanpipe legs (fatal: select on
	// a synctest channel from outside the bubble).

	synctest.Test(t, func(t *testing.T) {
		src := &net.TCPAddr{IP: net.ParseIP("192.168.4.20"), Port: 44723}
		dst := &net.TCPAddr{IP: net.ParseIP("203.0.116.72"), Port: 14431}

		// The clocked loops get a bubble-owned ctx: a root ctx's Done
		// channel would make the sim's select non-durable and freeze
		// the clock too.
		bctx, bcancel := context.WithCancel(context.Background())
		h.Start(bctx) // the sim push loop (the prox is pre-built: no listener)

		invRelay, invClient := chanpipe.New()
		upRelay, upCloud := chanpipe.New()
		p.Ladder.Dial = func(net.Addr) (net.Conn, error) { return upRelay, nil }

		srvDone := make(chan error, 1)
		go func() { srvDone <- p.Ladder.Serve(smokeTestAddr{Conn: invRelay, remote: src}, dst) }()

		// ---------- scripted inverter (TLS client) ----------
		invErr := make(chan error, 4)
		armed := make(chan struct{}, 1)
		go func() {
			//nolint:gosec // G402: scripted smoke endpoint — the trust anchor is the smoke's own CA
			tc := tls.Client(invClient, &tls.Config{ServerName: "203.0.116.72", InsecureSkipVerify: true})
			if err := tc.Handshake(); err != nil {
				invErr <- fmt.Errorf("handshake: %w", err)
				return
			}
			if _, err := tc.Write(smokeRegFrames()); err != nil { // 67 B
				invErr <- fmt.Errorf("reg: %w", err)
				return
			}
			// Full mode: the real cloud owns the bootstrap — the
			// scripted cloud's trio is relayed here (profile §3).
			trio, err := smokeReadExact(tc, 45)
			if err != nil {
				invErr <- fmt.Errorf("read trio: %w", err)
				return
			}
			if trio[2] != 0xaa || trio[18+2] != 0xac || trio[31+2] != 0xab {
				invErr <- fmt.Errorf("trio dev0s: % x", trio[2:6])
				return
			}
			if _, err := tc.Write(smokeFrame(smokeStartC2S, smokeDevInv, smokeFnData,
				smokeTelePayload(1200, 400, 800, 0, 0xfffffc18, 31))); err != nil { // tele 1
				invErr <- fmt.Errorf("tele1: %w", err)
				return
			}
			time.Sleep(5 * time.Second) // the tele cadence (virtual)
			if _, err := tc.Write(smokeFrame(smokeStartC2S, smokeDevInv, smokeFnData,
				smokeTelePayload(900, 200, 700, 0, 0xfffff8f0, 42))); err != nil { // tele 2
				invErr <- fmt.Errorf("tele2: %w", err)
				return
			}
			// The relayed cloud fn10 write, byte-exact.
			got, err := smokeReadExact(tc, 24)
			if err != nil {
				invErr <- fmt.Errorf("read got: %w", err)
				return
			}
			want := smokeFrame(smokeStartS2C, smokeDevS2C, smokeEnvFnS2C, smokePduWriteLim)
			if !bytes.Equal(got, want) {
				invErr <- fmt.Errorf("forwarded write: got % x want % x", got, want)
				return
			}
			// The firmware chunk the cloud pushed with the write is held
			// (full mode passes only the kinds the cloud is known to
			// send; a firmware push needs a maintenance window). Not
			// reading it is the check: the next bytes this inverter reads
			// must be the gateway's command, not 1037 B of firmware.
			// Armed: from here the inverter parks on the injected
			// frame read. The test body issues the REST command only
			// after this signal (its socket wait is non-durable and
			// would freeze the clock — nobody needs it past this point).
			armed <- struct{}{}
			// The gateway reads 41000 before writing (inverter wear:
			// an unchanged value is never rewritten); the device
			// answers self-use (0), so the feed-in write follows.
			rd, err := smokeReadExact(tc, 19)
			if err != nil {
				invErr <- fmt.Errorf("read rd: %w", err)
				return
			}
			if !bytes.Equal(rd[9:15], smokePduReadWM) {
				invErr <- fmt.Errorf("work-mode read PDU: % x", rd)
				return
			}
			if _, err := tc.Write(smokeFrame(smokeStartC2S, smokeDevReadResp, smokeFnData, smokePduReadWMResp)); err != nil {
				invErr <- fmt.Errorf("read response: %w", err)
				return
			}
			// The injected work-mode frame (REST below): PDU byte-exact;
			// the device tail is the session's (substituted).
			wm, err := smokeReadExact(tc, 19)
			if err != nil {
				invErr <- fmt.Errorf("read wm: %w", err)
				return
			}
			if !bytes.Equal(wm[9:15], smokePduWriteWM) { // payload starts after start(2)+dev(4)+fn(1)+plen(2)
				invErr <- fmt.Errorf("work-mode PDU: % x", wm)
				return
			}
			// The read-modify-write echo — the gateway's ack.
			if _, err := tc.Write(smokeFrame(smokeStartC2S, smokeDevEcho, smokeFnData, smokePduWriteWM)); err != nil {
				invErr <- fmt.Errorf("echo: %w", err)
				return
			}
			invErr <- nil
		}()

		// ---------- scripted cloud (the upstream leg) ----------
		// The ladder dials the upstream and speaks TLS to it; the cloud
		// side completes the handshake before the app bytes flow.
		cloudErr := make(chan error, 4)
		go func() {
			cl := tls.Server(upCloud, &tls.Config{Certificates: []tls.Certificate{cloudCert}})
			hctx, hcancel := context.WithTimeout(bctx, 30*time.Second)
			if err := cl.HandshakeContext(hctx); err != nil {
				hcancel()
				cloudErr <- fmt.Errorf("cloud handshake: %w", err)
				return
			}
			hcancel()
			buf := new(bytes.Buffer)
			hdr := make([]byte, 1)
			// The cloud's bootstrap trio (the real cloud's answers to the
			// registration — full mode relays them, profile §3),
			// time-derived S/q, one triplet.
			var cloudTrio []byte
			for _, f := range frame.BootstrapTrio([3]byte{0x2a, 0x2c, 0x2b}, frame.TrioParamsAt(time.Now())) {
				cloudTrio = append(cloudTrio, f...)
			}
			// The trio goes out once the registration is drained (67 B);
			// the write + firmware once both teles are in (reg 67 + tele
			// 163 ×2 = 393 B): deterministic ordering of the bus events
			// (the two snapshots before the config/system pair).
			for {
				if _, err := io.ReadFull(cl, hdr); err != nil {
					if err != io.EOF && err != io.ErrUnexpectedEOF {
						cloudErr <- fmt.Errorf("drain: %w", err)
					} else {
						cloudErr <- nil // clean EOF: the session ended
					}
					return
				}
				buf.Write(hdr)
				switch buf.Len() {
				case 67: // reg drained: the bootstrap trio
					if _, err := cl.Write(cloudTrio); err != nil {
						cloudErr <- fmt.Errorf("trio push: %w", err)
						return
					}
				case 67 + 163 + 163: // both teles drained
					wr := smokeFrame(smokeStartS2C, smokeDevS2C, smokeEnvFnS2C, smokePduWriteLim)
					fw := smokeFrame(smokeStartS2C, smokeDevS2C, smokeFnData, bytes.Repeat([]byte{0xa5}, 1024))
					if _, err := cl.Write(append(wr, fw...)); err != nil {
						cloudErr <- fmt.Errorf("push: %w", err)
						return
					}
				}
			}
		}()

		// ---------- REST ----------
		// healthz (no auth).
		code, _ := rest(t, h, "GET", "/healthz", "", "")
		require.Equal(t, 200, code)

		// token create (the seeded installer) — the plaintext is shown
		// once; everything else is Bearer thereafter.
		code, b := rest(t, h, "POST", "/api/v1/config/tokens", installerBearer, `{"name":"ha","role":"standard"}`)
		require.Equal(t, 201, code, "%s", b)
		var created struct {
			Token string `json:"token"`
		}
		require.NoError(t, json.Unmarshal(b, &created))
		require.NotEmpty(t, created.Token)
		standardBearer := "Bearer " + created.Token

		// The inverter is parked on the injected-frame read; from here
		// its remaining steps are pure rendezvous (no virtual time), so
		// the socket wait below cannot freeze the clock mid-script.
		<-armed

		// C-008: the write gate needs an identified, verified device —
		// seed what the settings poll would have read from the KH10.
		smokeSeedVerifiedDevice(h.st)

		// work-mode (standard): the gateway injects the fn06 frame; the
		// inverter echoes; the ack resolves the handler's wait.
		code, b = rest(t, h, "PUT", "/api/v1/work-mode", standardBearer, `{"work_mode":"feed-in"}`)
		require.Equal(t, 200, code, "%s", b)

		// The inverter side finished (handshake … echo) without error.
		require.NoError(t, <-invErr)

		// The tele rows are enqueued in the store; settle them before
		// the REST state read (the flusher runs on real time, the
		// bubble on virtual).
		h.st.Flush()

		// state (standard): the tele is online; the posture is full.
		code, b = rest(t, h, "GET", "/api/v1/state", standardBearer, "")
		require.Equal(t, 200, code, "%s", b)
		var st struct {
			Online      bool   `json:"online"`
			ForwardMode string `json:"forward_mode"`
			Battery     struct {
				SocPct int `json:"soc_pct"`
			} `json:"battery"`
			Limits struct {
				ExportW int `json:"export_w"`
				ImportW int `json:"import_w"`
			} `json:"limits"`
		}
		require.NoError(t, json.Unmarshal(b, &st))
		require.True(t, st.Online)
		require.Equal(t, 42, st.Battery.SocPct) // tele 2 is the newest
		require.Equal(t, "full", st.ForwardMode)
		require.Equal(t, 0, st.Limits.ExportW) // the cloud fn10 write landed in the store
		require.Equal(t, 5000, st.Limits.ImportW)

		// maintenance (standard): the window is closed — the view is
		// inactive (so the firmware chunk above was held, not relayed).
		code, b = rest(t, h, "GET", "/api/v1/maintenance", standardBearer, "")
		require.Equal(t, 200, code, "%s", b)
		var maint struct {
			Active bool `json:"active"`
		}
		require.NoError(t, json.Unmarshal(b, &maint))
		require.False(t, maint.Active)

		// ---------- settle: close the session, then the loops ----------
		require.NoError(t, invClient.Close())
		require.NoError(t, upCloud.Close())
		require.NoError(t, <-srvDone)
		require.NoError(t, <-cloudErr)

		h.st.WaitStreams() // inside the bubble (the stream persisters are group goroutines)
		bcancel()          // the sim's push loop exits on the bubble ctx
	})

	// ---------- post-bubble: the persisted facts ----------
	h.st.Flush()

	audit, err := h.st.QueryAudit(time.Time{}, "flow-open", "serve", "tls", "flow-close", "command")
	require.NoError(t, err)
	auditKinds := map[string]bool{}
	for _, a := range audit {
		auditKinds[a.Kind] = true
	}
	for _, k := range []string{"flow-open", "serve", "tls", "flow-close", "command"} {
		require.True(t, auditKinds[k], "audit kind %s present", k)
	}

	// full mode: the cloud fn10 write and the firmware chunk both
	// forwarded (no hold any more).
	fwdS2C, err := h.st.AuditFrames("forwarded", capture.DirS2C)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(fwdS2C), 2)

	// The bus replay (SSE Last-Event-ID's data source): the session's
	// discrete events in order, then the latest snapshot (snapshots are
	// not replayed: the next supersedes each).
	_, replay, _, _ := h.bus.Connect(0, true)
	kinds := make([]string, 0, len(replay))
	for _, ev := range replay {
		kinds = append(kinds, ev.Kind)
	}
	require.Equal(t, []string{
		api.EventConnection, // online (the first telemetry of the session)
		api.EventConfig,     // the cloud's fn10 write: limits (the observer's config event)
		api.EventConfig,     // identity learned (the seeded model read, as the poll would)
		api.EventConfig,     // identity (the seeded version read)
		api.EventConfig,     // work-mode learned (the seeded 41000 read)
		api.EventConfig,     // the work-mode command acked
		api.EventConnection, // offline (session down)
		api.EventSnapshot,   // tele 2, the latest
	}, kinds)

	// Chunk byte capture (knob on above): the persisted chunks carry the
	// raw bytes — the trace-contribution path end to end (EnableBytes
	// before the first stream, G2).
	nb, err := h.st.ChunkPayloadCount()
	require.NoError(t, err)
	require.Positive(t, nb)
}

// smokeReadExact reads exactly n bytes. It returns the error rather
// than failing the test: it runs on the scripted inverter's goroutine,
// where testify's require must not be used.
func smokeReadExact(c *tls.Conn, n int) ([]byte, error) {
	b := make([]byte, n)
	_, err := io.ReadFull(c, b)
	return b, err
}

func rest(t *testing.T, h *handle, method, path, auth, body string) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Host = "127.0.0.1:18080" // the Host check: an address, as a browser on the LAN sends
	// Same-host operator client: the LAN-only guard (task 5b) admits
	// loopback (httptest's default 192.0.2.1 remote would 403).
	req.RemoteAddr = "127.0.0.1:1234"
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.srv.Handler.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// smokeSeedVerifiedDevice records the reference KH10's identity reads
// (model "KH10", fw 1.73/1.03/1.71) and its work-mode register, as the
// settings poll would on a live session.
func smokeSeedVerifiedDevice(st *store.Store) {
	t0 := time.Now()
	model := make([]uint16, 15)
	const name = "KH10           "
	for i := range len(name) {
		model[i] = uint16(name[i])
	}
	st.RecordRegisters(t0, 30000, model)
	st.RecordRegisters(t0, 30016, []uint16{0x173, 0x103, 0x171})
	// only the work-mode register: the write gate is per feature, and
	// the smoke later asserts the limits the cloud's own write set
	st.RecordRegisters(t0, 41000, []uint16{0})
	st.Flush()
}
