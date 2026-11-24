// Command foxess-wizard is the OpenStick setup wizard (design D-031):
// a root-owned appliance tool that gets a stick onto the home Wi-Fi,
// points the inverter at it, and explains what is wrong when something
// is. It is outside C-006's scope (the constraint's scope note): it
// never speaks the inverter's protocol and never sits in the data path.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"foxesslocal/internal/wizard/qmi"
)

// version is stamped at build time (-X main.version), like foxess-local.
var version = "dev"

const (
	exitUsage  = 2
	exitFailed = 1
)

// imeiTimeout bounds one IMEI read; the modem answers in milliseconds
// once its device exists (~9.5 s after boot, spike S4).
const imeiTimeout = 10 * time.Second

const usage = "usage: foxess-wizard run|demo|version|imei-check|init|label [flags]"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, usage)
		return exitUsage
	}
	switch args[0] {
	case "version":
		_, _ = fmt.Fprintln(stdout, version)
		return 0
	case "imei-check":
		return imeiCheck(args[1:], stdout, stderr)
	case "run":
		return runCmd(args[1:], stderr)
	case "demo":
		return demoCmd(args[1:], stderr)
	case "init":
		return initCmd(args[1:], stdout, stderr)
	case "label":
		return labelCmd(args[1:], stdout, stderr)
	default:
		_, _ = fmt.Fprintln(stderr, usage)
		return exitUsage
	}
}

// imeiCheck is preparation's check that the wizard can read a valid
// IMEI (provision-openstick.sh --appliance). It prints the last four
// digits only, to compare with the cover.
func imeiCheck(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("imei-check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dev := fs.String("dev", "/dev/wwan0qmi0", "the modem's QMI control device")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	f, err := os.OpenFile(*dev, os.O_RDWR, 0)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "imei-check: %v\n", err)
		return exitFailed
	}
	defer func() { _ = f.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), imeiTimeout)
	defer cancel()
	imei, err := qmi.ReadIMEI(ctx, f)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "imei-check: %v\n", err)
		return exitFailed
	}
	_, _ = fmt.Fprintf(stdout, "imei-check: a valid IMEI, %s (compare the last four digits with the cover)\n", imei)
	return 0
}
