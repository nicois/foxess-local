package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"foxesslocal/deploy"
	"foxesslocal/internal/config"
	"foxesslocal/internal/update"
)

// upgradeTimeout bounds a CLI upgrade (download + verify + self-test).
const upgradeTimeout = 5 * time.Minute

// exitUpgradeFailed is the exit code for a failed or refused upgrade.
const exitUpgradeFailed = 1

// upgradeCmd is `foxess-local upgrade [-check]`, run as the service
// user (it owns the binary): check for, or install, the latest signed
// release. The running service switches to it at its next restart;
// the web UI's update button installs and restarts in one step, without
// root.
func upgradeCmd(args []string) int {
	fs := flag.NewFlagSet("upgrade", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath(), "config file")
	checkOnly := fs.Bool("check", false, "only report whether a newer release exists")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitUpgradeFailed
	}
	svc := buildUpdate(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), upgradeTimeout)
	defer cancel()
	v := svc.CheckNow(ctx)
	switch {
	case v.Error != "":
		fmt.Fprintln(os.Stderr, "upgrade: check failed:", v.Error)
		return exitUpgradeFailed
	case !v.Available:
		fmt.Printf("up to date: %s (latest release %s)\n", v.Current, v.Latest)
		return 0
	case *checkOnly:
		fmt.Printf("update available: %s → %s (%s)\n", v.Current, v.Latest, v.URL)
		return 0
	}
	if err := svc.Apply(ctx); err != nil {
		if r, ok := errors.AsType[*update.ReasonError](err); ok {
			fmt.Fprintln(os.Stderr, "upgrade: not possible here:", r.Reason)
		} else {
			fmt.Fprintln(os.Stderr, "upgrade:", err)
		}
		return exitUpgradeFailed
	}
	fmt.Printf("installed %s (previous binary kept as foxess-local.prev).\n", v.Latest)
	fmt.Println("The running service switches to it at its next restart (`sudo systemctl restart foxess-local`).")
	fmt.Println("Tip: updating from the web UI (More → Settings → Updates) restarts the service for you, no root needed.")
	return 0
}

// verifyReleaseCmd is `foxess-local verify-release [-key FILE] DIR`:
// check a downloaded release directory (SHA256SUMS signature + every
// listed file's hash) against this binary's release key, or a given
// minisign .pub file. The release owner runs it before publishing;
// anyone can run it before a manual install.
func verifyReleaseCmd(args []string) int {
	fs := flag.NewFlagSet("verify-release", flag.ExitOnError)
	keyFile := fs.String("key", "", "minisign public key file (default: the key built into this binary)")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: foxess-local verify-release [-key FILE] DIR")
		return exitUsage
	}
	keyText := string(deploy.ReleaseKey)
	if *keyFile != "" {
		b, err := os.ReadFile(*keyFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return exitUpgradeFailed
		}
		keyText = string(b)
	}
	pk, err := update.ParsePublicKey(keyText)
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify-release: no usable release key:", err)
		return exitUpgradeFailed
	}
	files, err := update.VerifyDir(pk, fs.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify-release: FAILED:", err)
		return exitUpgradeFailed
	}
	for _, f := range files {
		fmt.Println("ok", f)
	}
	fmt.Printf("verified: %d files signed by key %x\n", len(files), pk.ID)
	return 0
}
