package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/abtreece/fanctl/internal/config"
)

// runRestoreAuto hands fan control back to the BMC's automatic thermal
// management and exits. The systemd unit runs it as ExecStopPost, so a stopped
// or crashed daemon never leaves the fans pinned.
//
// It exists as a subcommand rather than a raw `ipmitool` line in the unit file
// because the unit cannot know the connection: an out-of-band (lanplus) setup
// has no local /dev/ipmi0 to talk to, and the ipmitool binary lives in /usr/bin
// on Debian-family and /usr/sbin on RHEL-family hosts. Going through fanctl
// reuses whatever connection the config file already describes.
func runRestoreAuto(args []string) int {
	fs := flag.NewFlagSet("fanctl restore-auto", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cfgPath := fs.String("config", config.DefaultPath, "Path to config file (for the BMC connection)")
	timeout := fs.Duration("timeout", 15*time.Second, "Deadline for the restore command")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	cfg := loadConfigOrDefault(*cfgPath)
	return restoreAuto(os.Stdout, os.Stderr, newIPMIClient(cfg), *timeout)
}

// autoSetter is the subset of *ipmi.Client restore-auto needs, for testability.
type autoSetter interface {
	SetAuto(context.Context) error
}

func restoreAuto(stdout, stderr io.Writer, client autoSetter, timeout time.Duration) int {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if err := client.SetAuto(ctx); err != nil {
		_, _ = fmt.Fprintf(stderr, "restore-auto: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintln(stdout, "restored BMC automatic fan control")
	return 0
}
