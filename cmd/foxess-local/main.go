// Command foxess-local is the single foxess-local gateway binary.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"foxesslocal/internal/api"
	"foxesslocal/internal/certgen"
	"foxesslocal/internal/config"
	"foxesslocal/internal/emudhra"
	"foxesslocal/internal/update"
)

// exitUsage is the exit code for CLI usage errors.
const exitUsage = 2

// exitRunFailed is the exit code for a failed run start.
const exitRunFailed = 1

// shutdownTimeout bounds the graceful shutdown on the way out of run.
const shutdownTimeout = 30 * time.Second

// exitScanFailed is the exit code for a failed emudhra-check scan.
const exitScanFailed = 1

// exitCertgenFailed is the exit code for a failed certgen run.
const exitCertgenFailed = 1

// exitTokensFailed is the exit code for a failed tokens mint.
const exitTokensFailed = 1

// certOutDirMode is the permission for the certgen output directory.
const certOutDirMode = 0o700

// certFileMode is the permission for minted cert artifacts.
const certFileMode = 0o600

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: foxess-local run|config|doctor|version|upgrade|verify-release|tokens|certgen|emudhra-check [flags]")
		os.Exit(exitUsage)
	}
	switch os.Args[1] {
	case "run":
		os.Exit(runCmd(os.Args[2:]))
	case "config":
		os.Exit(configCmd(os.Args[2:], os.Stdout, os.Stderr))
	case "certgen":
		os.Exit(certgenRun(os.Args[2:]))
	case "emudhra-check":
		os.Exit(emudhraCheck(os.Args[2:]))
	case "tokens":
		os.Exit(tokensCmd(os.Args[2:]))
	case "version":
		// the bare release tag: the self-upgrade's self-test compares it
		fmt.Println(version)
	case "upgrade":
		os.Exit(upgradeCmd(os.Args[2:]))
	case "doctor":
		os.Exit(doctorCmd(os.Args[2:]))
	case "verify-release":
		os.Exit(verifyReleaseCmd(os.Args[2:]))
	default:
		fmt.Fprintln(os.Stderr, "unknown subcommand:", os.Args[1])
		os.Exit(exitUsage)
	}
}

func runCmd(args []string) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath(), "config file")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitRunFailed
	}
	for _, w := range cfg.Warnings {
		slog.Warn("run: " + w)
	}
	sctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// a self-upgrade ends the process cleanly; systemd (Restart=always)
	// starts the new binary
	ctx, restart := context.WithCancel(sctx)
	defer restart()

	h, err := run(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitRunFailed
	}
	h.upd.Restart = func() {
		slog.Info("update: restarting into the new binary")
		restart()
	}
	h.Start(ctx)
	h.ServeHTTP()
	// The inverter listener binds asynchronously (prox exposes no ready
	// signal): its own "listening" log line is the readiness proof; a
	// bind that does not clear within listenerBindWait is fatal (DEF-009).
	mode, _, _ := h.fwd.Get() // the effective mode (a UI choice wins over the file)
	fmt.Printf("foxess-local %s up: http=%s inverter=%s (binding; waits up to %s) forward=%s\n",
		appVersion(), h.ln.Addr().String(), cfg.Server.InverterListen, listenerBindWait, mode)

	code, ferr := awaitExit(ctx, h.fatal)
	if cause := exitCause(ferr); cause != "" && h.upd != nil && h.upd.U != nil {
		if err := update.NoteExit(filepath.Dir(h.upd.U.Exe), cause); err != nil {
			fmt.Fprintln(os.Stderr, "upgrade marker:", err)
		}
	}
	fmt.Fprintln(os.Stderr, "shutting down")
	dctx, dcancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer dcancel()
	if err := h.Shutdown(dctx); err != nil {
		fmt.Fprintln(os.Stderr, "shutdown:", err)
	}
	return code
}

func emudhraCheck(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: foxess-local emudhra-check <dir-with-pem-files>")
		return exitUsage
	}
	matches := 0
	err := filepath.WalkDir(args[0], func(path string, d fs.DirEntry, err error) error { //nolint:gosec // G703: the CLI scans an operator-specified directory
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".pem") {
			return err
		}
		fileMatches, ferr := scanPEMFile(path)
		if ferr != nil {
			return ferr
		}
		matches += fileMatches
		return nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitScanFailed
	}
	fmt.Printf("scanned; %d small-key intermediate(s) found (expected 8)\n", matches)
	return 0
}

// scanPEMFile reports every certificate in the PEM file at path against the
// eMudhra small-key set and returns how many matched.
func scanPEMFile(path string) (int, error) {
	matches := 0
	data, rerr := os.ReadFile(path) //nolint:gosec // G304: path comes from the operator-specified scan directory
	if rerr != nil {
		return 0, rerr
	}
	for rest := data; len(rest) > 0; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil || block.Type != pemCertType {
			break
		}
		cert, perr := x509.ParseCertificate(block.Bytes)
		if perr != nil {
			continue
		}
		if k, ok := emudhra.SmallKeyFor(cert, emudhra.SmallKeys); ok {
			matches++
			fmt.Printf("SMALL-KEY k=%d subject=%q serial=%x file=%s\n",
				k, cert.Subject.CommonName, cert.SerialNumber.Bytes(), path)
		} else {
			fmt.Printf("ok (no small key) subject=%q serial=%x file=%s\n",
				cert.Subject.CommonName, cert.SerialNumber.Bytes(), path)
		}
	}
	return matches, nil
}

// certgenRun mints the broker certificate from the configured anchor and
// writes cert.pem, key.pem, and cert-manifest.json into -out.
func certgenRun(args []string) int {
	fs := flag.NewFlagSet("certgen", flag.ExitOnError)
	ips := fs.String("ip", defaultLeafIP.String(), "comma-separated leaf iPAddress SANs")
	hosts := fs.String("host", "", "comma-separated leaf DNS SANs")
	anchor := fs.String("anchor", string(certgen.AnchorSelfSigned), "anchor kind: selfsigned|small-key|extracted-ca|own-ca")
	issuer := fs.String("issuer", "", "issuer cert PEM (all anchors except selfsigned)")
	issuerKey := fs.String("issuer-key", "", "issuer key PEM (extracted-ca, own-ca)")
	smallKey := fs.Int("small-key", 0, "small-key value (small-key anchor; 0 = first that validates)")
	out := fs.String("out", "", "output dir for cert.pem, key.pem, cert-manifest.json")
	validity := fs.Duration("validity", 0, "leaf cert validity (0 = default 5y)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if !certgen.IsValidAnchor(certgen.AnchorKind(*anchor)) {
		fmt.Fprintf(os.Stderr, "unknown anchor kind %q (want one of: selfsigned, small-key, extracted-ca, own-ca)\n", *anchor)
		return exitUsage
	}
	opts, err := certgenOpts(*ips, *hosts, *anchor, *issuer, *issuerKey, *smallKey)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitCertgenFailed
	}
	if *validity > 0 {
		opts.Validity = *validity
	}
	certPEM, keyPEM, m, err := certgen.Generate(opts, time.Now)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitCertgenFailed
	}
	return writeCertgenOut(*out, certPEM, keyPEM, m)
}

// certgenOpts assembles the Generate options from the flags (the SANs
// are flags, not config: only the CA half is used at runtime — prox
// mints the inverter-facing leaf per connection).
func certgenOpts(ipList, hostList, anchor, issuer, issuerKey string, smallKey int) (certgen.Options, error) {
	var ips []netip.Addr
	for _, s := range splitList(ipList) {
		ip, err := netip.ParseAddr(s)
		if err != nil {
			return certgen.Options{}, fmt.Errorf("-ip: %q is not an IP address", s)
		}
		ips = append(ips, ip)
	}
	opts := certgen.Options{Kind: certgen.AnchorKind(anchor), SANHosts: splitList(hostList), SANIPs: ips}
	switch opts.Kind {
	case certgen.AnchorSelfSigned:
		// selfsigned mints its own anchor; no issuer material
	case certgen.AnchorSmallKey, certgen.AnchorExtracted, certgen.AnchorOwnCA:
		issuerCert, err := loadPEMCert(issuer)
		if err != nil {
			return certgen.Options{}, err
		}
		opts.IssuerCert = issuerCert
		if opts.Kind == certgen.AnchorSmallKey {
			k, err := resolveSmallKey(issuerCert, smallKey)
			if err != nil {
				return certgen.Options{}, err
			}
			opts.IssuerKey = emudhra.KeyFromSmallK(k)
		} else {
			key, err := loadECKey(issuerKey)
			if err != nil {
				return certgen.Options{}, err
			}
			opts.IssuerKey = key
		}
	}
	return opts, nil
}

// resolveSmallKey returns the small-key value to sign with: the flag value if
// given, else the first SmallKey whose P-384 public key matches the issuer.
func resolveSmallKey(issuer *x509.Certificate, smallKey int) (int, error) {
	if smallKey != 0 {
		if _, ok := emudhra.SmallKeyFor(issuer, []int{smallKey}); !ok {
			return 0, fmt.Errorf("-small-key %d does not match the issuer's P-384 public key", smallKey)
		}
		return smallKey, nil
	}
	k, ok := emudhra.SmallKeyFor(issuer, emudhra.SmallKeys)
	if !ok {
		return 0, errors.New("issuer is not a known eMudhra small-key intermediate")
	}
	return k, nil
}

// writeCertgenOut writes cert.pem, key.pem, and cert-manifest.json into dir
// and prints the manifest JSON to stdout.
func writeCertgenOut(dir string, certPEM, keyPEM []byte, m certgen.Manifest) int {
	if err := os.MkdirAll(dir, certOutDirMode); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitCertgenFailed
	}
	if err := os.WriteFile(filepath.Join(dir, "cert.pem"), certPEM, certFileMode); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitCertgenFailed
	}
	if err := os.WriteFile(filepath.Join(dir, "key.pem"), keyPEM, certFileMode); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitCertgenFailed
	}
	if err := certgen.WriteManifest(filepath.Join(dir, "cert-manifest.json"), m); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitCertgenFailed
	}
	data, err := json.Marshal(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitCertgenFailed
	}
	fmt.Println(string(data))
	return 0
}

// loadPEMCert reads the first CERTIFICATE block from the PEM file at path.
func loadPEMCert(path string) (*x509.Certificate, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the issuer path is operator-specified
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("%s: no CERTIFICATE block", path)
	}
	return x509.ParseCertificate(block.Bytes)
}

// loadECKey reads the first PEM block from path and parses it as an EC key.
func loadECKey(path string) (*ecdsa.PrivateKey, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the issuer key path is operator-specified
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("%s: no PEM block", path)
	}
	return x509.ParseECPrivateKey(block.Bytes)
}

// tokensCmd dispatches the tokens subcommand (create only — day-2
// list/revoke go through the REST API).
func tokensCmd(args []string) int {
	usage := `usage: foxess-local tokens create -config <file> -name <name> -role <role>
  run with the service STOPPED: a running service holds the token map
  in memory and rewrites the file from it on its next mutation, so a
  token minted while it runs will not authenticate and gets deleted.`
	if len(args) == 0 || args[0] != "create" {
		fmt.Fprintln(os.Stderr, usage)
		return exitUsage
	}
	return tokensCreateRun(args[1:])
}

// tokensCreateRun mints one token into the configured store file and
// prints the plaintext exactly once (the file holds the SHA-256 hash
// only — losing the plaintext means re-minting).
func tokensCreateRun(args []string) int {
	fs := flag.NewFlagSet("tokens create", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath(), "config file")
	name := fs.String("name", "", "token name (required; unique in the store)")
	role := fs.String("role", "", "token role: read-only|standard|expert|installer (required; no default)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *name == "" || *role == "" {
		fmt.Fprintln(os.Stderr, "tokens create: -name and -role are both required")
		return exitUsage
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitTokensFailed
	}
	if cfg.Tokens.Path == "" {
		fmt.Fprintln(os.Stderr, "tokens create: tokens.path is empty (in-memory mode); set [tokens] path in the config")
		return exitTokensFailed
	}
	ts, err := api.NewTokenStore(cfg.Tokens.Path, time.Now)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitTokensFailed
	}
	tk, plaintext, err := ts.Create(*name, api.Role(*role))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitTokensFailed
	}
	fmt.Printf("%s  (token %s, name %s, role %s)\n", plaintext, tk.ID, tk.Name, tk.Role)
	return 0
}

// splitList splits a comma-separated flag value, dropping empty items.
func splitList(v string) []string {
	var out []string
	for s := range strings.SplitSeq(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
