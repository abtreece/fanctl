package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"time"

	"github.com/abtreece/fanctl/internal/config"
	"github.com/abtreece/fanctl/internal/ipmi"
)

// runProbe verifies that manual fan control is actually honored by this BMC.
//
// This exists because of a real trap: on Dell 12G the iDRAC's forced baseline
// (e.g. for a third-party PCIe card) sits around 30% duty, so commanding 30%
// looks like it "did nothing". Probing with a LOW duty and measuring the RPM
// drop is the only reliable way to confirm control works — and to catch
// firmware that silently ignores the raw commands.
func runProbe(args []string) int {
	fs := flag.NewFlagSet("fanctl probe", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cfgPath := fs.String("config", config.DefaultPath, "Path to config file (for ipmitool binary)")
	low := fs.Int("percent", 15, "Low fan duty percent to command during the probe")
	settle := fs.Duration("settle", 25*time.Second, "Time to wait for fans to spin down before re-reading")
	dropPct := fs.Int("min-drop", 20, "Minimum % RPM drop to consider control working")
	restore := fs.Bool("restore-auto", true, "Restore BMC automatic control when the probe finishes")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	cfg := loadConfigOrDefault(*cfgPath)
	client := newIPMIClient(cfg)
	return probe(os.Stdout, os.Stderr, client, probeOptions{
		low:        *low,
		settle:     *settle,
		minDropPct: *dropPct,
		restore:    *restore,
	})
}

type probeOptions struct {
	low        int
	settle     time.Duration
	minDropPct int
	restore    bool
}

// fanController is the subset of *ipmi.Client probe needs, for testability.
type fanController interface {
	Fans(context.Context) ([]ipmi.Fan, error)
	SetManual(context.Context) error
	SetPercent(context.Context, int) error
	SetAuto(context.Context) error
}

func probe(stdout, stderr io.Writer, client fanController, opt probeOptions) int {
	ctx := context.Background()

	before, err := client.Fans(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "probe: read fans: %v\n", err)
		return 1
	}
	baseline := ipmi.AverageRPM(before)
	_, _ = fmt.Fprintf(stdout, "baseline:   %d RPM across %d fans\n", baseline, len(before))
	if baseline == 0 {
		_, _ = fmt.Fprintln(stderr, "probe: no fan RPM readings; cannot probe")
		return 1
	}

	_, _ = fmt.Fprintf(stdout, "commanding: manual control at %d%%\n", opt.low)
	if err := client.SetManual(ctx); err != nil {
		_, _ = fmt.Fprintf(stderr, "probe: set manual: %v\n", err)
		return 1
	}
	if err := client.SetPercent(ctx, opt.low); err != nil {
		_, _ = fmt.Fprintf(stderr, "probe: set percent: %v\n", err)
		return 1
	}

	_, _ = fmt.Fprintf(stdout, "waiting:    %s for fans to settle...\n", opt.settle)
	sleep(ctx, opt.settle)

	after, err := client.Fans(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "probe: re-read fans: %v\n", err)
		return 1
	}
	low := ipmi.AverageRPM(after)
	_, _ = fmt.Fprintf(stdout, "at %d%%:     %d RPM\n", opt.low, low)

	if opt.restore {
		if err := client.SetAuto(ctx); err != nil {
			_, _ = fmt.Fprintf(stderr, "probe: restore auto: %v\n", err)
		} else {
			_, _ = fmt.Fprintln(stdout, "restored:   BMC automatic control")
		}
	}

	drop := baseline - low
	dropPct := drop * 100 / baseline
	_, _ = fmt.Fprintf(stdout, "\nRPM dropped %d (%d%%) when commanded to %d%%.\n", drop, dropPct, opt.low)
	if dropPct >= opt.minDropPct {
		_, _ = fmt.Fprintln(stdout, "RESULT: manual fan control WORKS on this host.")
		return 0
	}
	_, _ = fmt.Fprintln(stdout, "RESULT: manual fan control appears to be IGNORED.")
	_, _ = fmt.Fprintln(stdout, "  The BMC accepted the commands but RPM did not drop. Likely causes:")
	_, _ = fmt.Fprintln(stdout, "  - iDRAC firmware that disables raw fan control (Dell did this in some builds), or")
	_, _ = fmt.Fprintln(stdout, "  - the forced baseline already sits near the commanded duty (try a lower --percent).")
	return 1
}

// sleep waits for d or until ctx is cancelled.
func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// loadConfigOrDefault loads cfg from path, falling back to built-in defaults
// when the file is absent. Used by subcommands that only need a few fields.
func loadConfigOrDefault(path string) *config.Config {
	cfg := config.Default(path)
	if err := config.LoadFile(path, cfg); err != nil && !errors.Is(err, fs.ErrNotExist) {
		_, _ = fmt.Fprintf(os.Stderr, "warning: %v; using defaults\n", err)
	}
	return cfg
}
