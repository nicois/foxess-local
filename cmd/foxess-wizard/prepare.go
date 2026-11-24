package main

import (
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"foxesslocal/internal/wizard/identity"
	"foxesslocal/internal/wizard/label"
)

// labelMode: the label files are the preparer's (they hold the hotspot
// password, which is printed on the stick anyway).
const (
	labelMode    = 0o600
	labelDirMode = 0o700
)

// initCmd creates the stick's identity once (provision-openstick.sh
// --appliance). An existing one is kept: the label names it.
func initCmd(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("init", flag.ContinueOnError)
	fl.SetOutput(stderr)
	cfg := fl.String("config", identity.Path, "where the identity is kept")
	if err := fl.Parse(args); err != nil {
		return exitUsage
	}
	if id, err := identity.Read(*cfg); err == nil {
		_, _ = fmt.Fprintf(stdout, "init: kept %s (%s)\n", id.SSID, *cfg)
		return 0
	} else if !errors.Is(err, fs.ErrNotExist) {
		_, _ = fmt.Fprintf(stderr, "init: %v (not replaced: fix or remove it)\n", err)
		return exitFailed
	}
	id, err := identity.Generate(rand.Reader)
	if err == nil {
		err = identity.Write(*cfg, id)
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "init: %v\n", err)
		return exitFailed
	}
	_, _ = fmt.Fprintf(stdout, "init: created %s (%s)\n", id.SSID, *cfg)
	return 0
}

// labelCmd writes the stick's label as <ssid>-label.txt and .png in
// -out, and prints the text.
func labelCmd(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("label", flag.ContinueOnError)
	fl.SetOutput(stderr)
	cfg := fl.String("config", identity.Path, "the stick's identity")
	out := fl.String("out", ".", "the directory for the label files")
	if err := fl.Parse(args); err != nil {
		return exitUsage
	}
	id, err := identity.Read(*cfg)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "label: %v\n", err)
		return exitFailed
	}
	img, err := label.PNG(id)
	if err == nil {
		err = os.MkdirAll(*out, labelDirMode)
	}
	base := filepath.Join(*out, id.SSID+"-label")
	if err == nil {
		err = os.WriteFile(base+".png", img, labelMode)
	}
	if err == nil {
		err = os.WriteFile(base+".txt", []byte(label.Text(id)), labelMode)
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "label: %v\n", err)
		return exitFailed
	}
	_, _ = fmt.Fprintf(stdout, "%s\nlabel: %s.png and %s.txt\n", label.Text(id), base, base)
	return 0
}
