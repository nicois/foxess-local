package main

import (
	"flag"
	"fmt"
	"io"

	"foxesslocal/internal/config"

	"github.com/BurntSushi/toml"
)

// exitConfigInvalid is the exit code for a config that does not load.
const exitConfigInvalid = 1

// configCmd validates a config without starting anything and prints the
// effective settings (every default resolved) as TOML — the pre-restart
// check after an upgrade. Retired-key warnings go to errOut.
func configCmd(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("config", flag.ContinueOnError)
	fs.SetOutput(errOut)
	cfgPath := fs.String("config", config.DefaultPath(), "config file")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		_, _ = fmt.Fprintln(errOut, err)
		return exitConfigInvalid
	}
	for _, w := range cfg.Warnings {
		_, _ = fmt.Fprintln(errOut, "warning:", w)
	}
	if err := toml.NewEncoder(out).Encode(cfg); err != nil {
		_, _ = fmt.Fprintln(errOut, err)
		return exitConfigInvalid
	}
	return 0
}
