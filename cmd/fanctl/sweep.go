package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/abtreece/fanctl/internal/config"
	"github.com/abtreece/fanctl/internal/gpu"
	"github.com/abtreece/fanctl/internal/ipmi"
	"github.com/abtreece/fanctl/internal/runcmd"
)

// runSweep steps the commanded fan duty down through a series of values and
// records what each one costs, so a curve's bottom anchor can be chosen from
// measurements rather than guesses.
//
// This is `probe` generalised: probe answers "is manual control honoured at
// all" with a single low duty, while sweep answers "how far down is it worth
// going" across a range. Both exist because Dell's forced baseline hides the
// answer — an iDRAC that idles the fans at ~30% makes a commanded 30% look
// like a no-op, and firmware minimums mean the lowest useful duty is a
// property of the host, not of the curve.
//
// It runs through the configured connection for the same reason restore-auto
// does: an out-of-band (lanplus) BMC has no local /dev/ipmi0, and hardcoding
// `ipmitool raw` would silently only ever work in-band.
func runSweep(args []string) int {
	fs := flag.NewFlagSet("fanctl sweep", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cfgPath := fs.String("config", config.DefaultPath, "Path to config file (BMC connection and sensor selection)")
	steps := fs.String("steps", "10,8,6,4,2,0", "Comma-separated duty percents to visit, highest first")
	settle := fs.Duration("settle", 45*time.Second, "Time to let fans and temperatures settle at each step")
	maxTemp := fs.Int("max-temp", 60, "Abort if a selected sensor reaches this temperature (°C)")
	maxGPUTemp := fs.Int("max-gpu-temp", 75, "Abort if a GPU reaches this temperature (°C), when GPU monitoring is enabled")
	minRPM := fs.Int("min-rpm", 900, "Abort if any fan drops below this RPM")
	restore := fs.Bool("restore-auto", true, "Restore BMC automatic control when the sweep finishes")
	format := fs.String("format", "table", "Output format: table, tsv, or json")
	suggest := fs.Bool("suggest", false, "Analyse the results and recommend a curve floor")
	force := fs.Bool("force", false, "Run even if the fanctl daemon appears to be active")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	duties, err := parseSteps(*steps)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "sweep: %v\n", err)
		return 2
	}
	switch *format {
	case "table", "tsv", "json":
	default:
		_, _ = fmt.Fprintf(os.Stderr, "sweep: unknown format %q (want table, tsv, or json)\n", *format)
		return 2
	}

	// A running daemon re-evaluates on its own poll interval and re-asserts on
	// reassert_interval, so it would overwrite the commanded duty mid-step and
	// produce silently wrong numbers. Detection is best-effort: systemctl may
	// be absent, and it says nothing at all about a remote BMC's controller.
	if !*force && daemonActive() {
		_, _ = fmt.Fprintln(os.Stderr, "sweep: the fanctl daemon appears to be active; it will fight the sweep")
		_, _ = fmt.Fprintln(os.Stderr, "  stop it first:  sudo systemctl stop fanctl")
		_, _ = fmt.Fprintln(os.Stderr, "  or re-run with -force if this check is wrong (e.g. a remote BMC)")
		return 2
	}

	// Unlike doctor and probe, sweep insists on the config it was pointed at.
	// Falling back to built-in defaults would silently clear gpu.enabled, and a
	// sweep is the one subcommand that deliberately drives duty toward zero:
	// being GPU-blind here is exactly the unsafe state the daemon refuses to
	// enter. A typo'd -config must fail, not quietly widen the envelope.
	cfg := config.Default(*cfgPath)
	if err := config.LoadFile(*cfgPath, cfg); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "sweep: %v\n", err)
		_, _ = fmt.Fprintln(os.Stderr, "  sweep needs the host's config (sensor selection, connection, gpu.enabled)")
		_, _ = fmt.Fprintln(os.Stderr, "  point it at one with -config, or write one with: fanctl install")
		return 2
	}
	if err := config.Validate(cfg); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "sweep: %s: %v\n", *cfgPath, err)
		return 2
	}

	// On a GPU host the card is the thermally interesting part — a passively
	// cooled T4 lives on chassis airflow — so a sweep that only watched IPMI
	// sensors would be blind to exactly what it is putting at risk.
	var gpuReader gpuTempReader
	if cfg.GPU.Enabled {
		gpuReader = gpu.New(cfg.GPU.Command, gpu.ExecRunner)
	} else {
		_, _ = fmt.Fprintf(os.Stderr, "sweep: gpu monitoring is off in %s; this sweep watches IPMI sensors only\n", *cfgPath)
	}

	return sweep(os.Stdout, os.Stderr, newIPMIClient(cfg), sweepOptions{
		duties:     duties,
		settle:     *settle,
		maxTemp:    *maxTemp,
		maxGPUTemp: *maxGPUTemp,
		minRPM:     *minRPM,
		restore:    *restore,
		format:     *format,
		suggest:    *suggest,
		sensors:    cfg.Sensors,
		gpu:        gpuReader,
	})
}

type sweepOptions struct {
	duties     []int
	settle     time.Duration
	maxTemp    int
	maxGPUTemp int
	minRPM     int
	restore    bool
	format     string
	suggest    bool
	sensors    config.SensorConfig
	gpu        gpuTempReader // nil when GPU monitoring is disabled
}

// sweepController is the subset of *ipmi.Client sweep needs, for testability.
type sweepController interface {
	Fans(context.Context) ([]ipmi.Fan, error)
	Temperatures(context.Context) ([]ipmi.Temp, error)
	SetManual(context.Context) error
	SetPercent(context.Context, int) error
	SetAuto(context.Context) error
}

// gpuTempReader is the subset of *gpu.Reader sweep needs, for testability.
type gpuTempReader interface {
	Temperatures(context.Context) ([]gpu.Temp, error)
}

// sweepSample is one measured duty step. Duty is -1 for the baseline reading
// taken before manual control is asserted.
type sweepSample struct {
	Duty    int    `json:"duty"`
	AvgRPM  int    `json:"avg_rpm"`
	MinRPM  int    `json:"min_rpm"`
	MaxRPM  int    `json:"max_rpm"`
	TempC   int    `json:"temp_c"`
	Sensor  string `json:"sensor"`
	InletC  int    `json:"inlet_c,omitempty"`
	GPUC    int    `json:"gpu_c,omitempty"`
	HaveGPU bool   `json:"-"`
	Aborted string `json:"aborted,omitempty"`
}

// spreadPct is how far apart the fastest and slowest fans are, as a percentage
// of the average. It rises sharply once some fans hit their own floor while
// others still follow the commanded duty, which is the practical lower bound
// on useful control — below it, airflow gets uneven rather than merely lower.
func (s sweepSample) spreadPct() int {
	if s.AvgRPM == 0 {
		return 0
	}
	return (s.MaxRPM - s.MinRPM) * 100 / s.AvgRPM
}

const (
	// unevenSpreadRisePct is how many points of spread a step may gain over the
	// sweep's own reference spread before the fans count as no longer tracking
	// duty together. It is a *rise*, not an absolute, because chassis differ:
	// an R430 idles with its fans 37% apart at every duty, so an absolute
	// threshold would call a perfectly linear host broken at the first step.
	// What signals breakdown is the spread opening up as duty falls.
	unevenSpreadRisePct = 15
	// unevenSpreadAbsPct is a ceiling that applies however wide the reference
	// spread was, so a host whose very first step is already incoherent is not
	// endorsed just because nothing got worse afterwards.
	unevenSpreadAbsPct = 60
	// clampSlopeFracPct is the fraction of the steepest response seen so far
	// that a step's RPM-per-duty-point slope may fall to before the firmware is
	// treated as clamping. Comparing slopes rather than raw RPM drops keeps the
	// test independent of how finely the sweep was stepped.
	clampSlopeFracPct = 25
	// clampMinSlopeRPM catches a sweep that starts out already clamped, where
	// there is no healthy slope to compare against.
	clampMinSlopeRPM = 10
	// restoreTimeout bounds the hand-back to BMC automatic control.
	restoreTimeout = 15 * time.Second
)

func sweep(stdout, stderr io.Writer, client sweepController, opt sweepOptions) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The restore must not run on ctx: an interrupt cancels it, and that is
	// precisely the moment the fans most need handing back. Use a fresh
	// deadline instead.
	if opt.restore {
		defer func() {
			rctx, cancel := context.WithTimeout(context.Background(), restoreTimeout)
			defer cancel()
			if err := client.SetAuto(rctx); err != nil {
				_, _ = fmt.Fprintf(stderr, "sweep: restore auto: %v\n", err)
				_, _ = fmt.Fprintln(stderr, "  FANS MAY STILL BE PINNED -- run: fanctl restore-auto")
				return
			}
			_, _ = fmt.Fprintln(stdout, "restored BMC automatic control")
		}()
	}

	base, err := measure(ctx, client, opt, -1)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "sweep: baseline: %v\n", err)
		return 1
	}
	samples := []sweepSample{base}

	if err := client.SetManual(ctx); err != nil {
		_, _ = fmt.Fprintf(stderr, "sweep: set manual: %v\n", err)
		return 1
	}

	aborted := false
	for _, duty := range opt.duties {
		if err := client.SetPercent(ctx, duty); err != nil {
			_, _ = fmt.Fprintf(stderr, "sweep: set %d%%: %v\n", duty, err)
			return 1
		}
		_, _ = fmt.Fprintf(stderr, "commanded %d%%, settling %s...\n", duty, opt.settle)
		sleep(ctx, opt.settle)
		if ctx.Err() != nil {
			_, _ = fmt.Fprintln(stderr, "sweep: interrupted")
			aborted = true
			break
		}

		s, err := measure(ctx, client, opt, duty)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "sweep: measure %d%%: %v\n", duty, err)
			return 1
		}
		s.Aborted = abortReason(s, opt)
		samples = append(samples, s)
		if s.Aborted != "" {
			_, _ = fmt.Fprintf(stderr, "sweep: ABORT at %d%%: %s\n", duty, s.Aborted)
			aborted = true
			break
		}
	}

	render(stdout, samples, opt.format)
	if opt.suggest && opt.format != "json" {
		renderSuggestion(stdout, samples)
	}
	if aborted {
		return 1
	}
	return 0
}

// measure reads fans and temperatures once and folds them into a sample.
func measure(ctx context.Context, client sweepController, opt sweepOptions, duty int) (sweepSample, error) {
	sel := opt.sensors
	fans, err := client.Fans(ctx)
	if err != nil {
		return sweepSample{}, fmt.Errorf("read fans: %w", err)
	}
	if len(fans) == 0 {
		return sweepSample{}, errors.New("no fan RPM readings")
	}

	s := sweepSample{Duty: duty, AvgRPM: ipmi.AverageRPM(fans), MinRPM: fans[0].RPM, MaxRPM: fans[0].RPM}
	for _, f := range fans {
		if f.RPM < s.MinRPM {
			s.MinRPM = f.RPM
		}
		if f.RPM > s.MaxRPM {
			s.MaxRPM = f.RPM
		}
	}

	temps, err := client.Temperatures(ctx)
	if err != nil {
		return sweepSample{}, fmt.Errorf("read temperatures: %w", err)
	}
	t, name, ok := selectMaxTemp(temps, sel)
	if !ok {
		return sweepSample{}, errors.New("no selected temperature sensors matched")
	}
	s.TempC, s.Sensor = t, name
	for _, tp := range temps {
		if strings.Contains(strings.ToLower(tp.Name), "inlet") {
			s.InletC = tp.Celsius
			break
		}
	}

	// An unreadable GPU is fatal to the sweep rather than a warning, mirroring
	// the daemon's invariant: with GPU monitoring enabled, cooling on CPU data
	// alone is never the safe choice. Returning an error unwinds into the
	// deferred hand-back to BMC automatic control.
	if opt.gpu != nil {
		gtemps, err := opt.gpu.Temperatures(ctx)
		if err != nil {
			return sweepSample{}, fmt.Errorf("read gpu: %w", err)
		}
		g, ok := gpu.Max(gtemps)
		if !ok {
			return sweepSample{}, errors.New("gpu monitoring is enabled but no GPU temperatures were reported")
		}
		s.GPUC, s.HaveGPU = g, true
	}
	return s, nil
}

// abortReason reports why this sample should stop the sweep, or "" to continue.
func abortReason(s sweepSample, opt sweepOptions) string {
	if s.TempC >= opt.maxTemp {
		return fmt.Sprintf("%s reached %d°C (limit %d°C)", s.Sensor, s.TempC, opt.maxTemp)
	}
	if s.HaveGPU && s.GPUC >= opt.maxGPUTemp {
		return fmt.Sprintf("GPU reached %d°C (limit %d°C)", s.GPUC, opt.maxGPUTemp)
	}
	if s.MinRPM < opt.minRPM {
		return fmt.Sprintf("a fan fell to %d RPM (limit %d RPM)", s.MinRPM, opt.minRPM)
	}
	return ""
}

// parseSteps parses and validates the duty list. Steps must descend: the
// analysis walks them from most to least airflow looking for where control
// breaks down, and an unordered list would make that meaningless.
func parseSteps(s string) ([]int, error) {
	var out []int
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		v, err := strconv.Atoi(f)
		if err != nil {
			return nil, fmt.Errorf("bad step %q: not a number", f)
		}
		if v < 0 || v > 100 {
			return nil, fmt.Errorf("step %d out of range 0-100", v)
		}
		if len(out) > 0 && v >= out[len(out)-1] {
			return nil, fmt.Errorf("steps must descend: %d does not come after %d", v, out[len(out)-1])
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, errors.New("no steps given")
	}
	return out, nil
}

// daemonActive best-effort reports whether the fanctl unit is running. A
// missing systemctl, or any non-zero exit, is treated as "not active".
func daemonActive() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := runcmd.CombinedOutput(ctx, "systemctl", "is-active", "fanctl")
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "active"
}

// --- output -----------------------------------------------------------------

func render(w io.Writer, samples []sweepSample, format string) {
	withGPU := anyGPU(samples)
	switch format {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(samples)
	case "tsv":
		hdr := "duty\tavg_rpm\tmin_rpm\tmax_rpm\tspread_pct\ttemp_c\tinlet_c"
		if withGPU {
			hdr += "\tgpu_c"
		}
		_, _ = fmt.Fprintln(w, hdr)
		for _, s := range samples {
			_, _ = fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%d\t%d\t%d",
				dutyLabel(s), s.AvgRPM, s.MinRPM, s.MaxRPM, s.spreadPct(), s.TempC, s.InletC)
			if withGPU {
				_, _ = fmt.Fprintf(w, "\t%d", s.GPUC)
			}
			_, _ = fmt.Fprintln(w)
		}
	default:
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		if withGPU {
			_, _ = fmt.Fprintln(tw, "DUTY\tAVG RPM\tMIN\tMAX\tSPREAD\tTEMP\tGPU\tINLET\t")
		} else {
			_, _ = fmt.Fprintln(tw, "DUTY\tAVG RPM\tMIN\tMAX\tSPREAD\tTEMP\tINLET\t")
		}
		for _, s := range samples {
			note := ""
			if s.Aborted != "" {
				note = "  <- ABORT: " + s.Aborted
			}
			_, _ = fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d%%\t%d°C\t",
				dutyLabel(s), s.AvgRPM, s.MinRPM, s.MaxRPM, s.spreadPct(), s.TempC)
			if withGPU {
				_, _ = fmt.Fprintf(tw, "%d°C\t", s.GPUC)
			}
			_, _ = fmt.Fprintf(tw, "%d°C\t%s\n", s.InletC, note)
		}
		_ = tw.Flush()
	}
}

// anyGPU reports whether any sample carries a GPU reading, so the GPU column
// appears only on hosts that have one.
func anyGPU(samples []sweepSample) bool {
	for _, s := range samples {
		if s.HaveGPU {
			return true
		}
	}
	return false
}

func dutyLabel(s sweepSample) string {
	if s.Duty < 0 {
		return "baseline"
	}
	return strconv.Itoa(s.Duty) + "%"
}

// analyseSweep walks the commanded steps from most to least airflow and returns
// the lowest duty at which fan control was still well behaved, plus why the
// step below it was rejected.
//
// Walking downward and stopping at the first bad step matters: once fans bottom
// out, still-lower steps look well behaved again on their own numbers (every
// fan sits at the same floor, so the spread closes back up) even though duty no
// longer controls anything. Judging each step in isolation would pick one of
// those and recommend a floor below the point where control was already lost.
func analyseSweep(samples []sweepSample) (best sweepSample, reason string, ok bool) {
	ref := referenceSpread(samples)
	limit := ref + unevenSpreadRisePct
	if limit > unevenSpreadAbsPct {
		limit = unevenSpreadAbsPct
	}
	steepest := 0
	for _, s := range samples {
		if s.Duty < 0 {
			continue // baseline is not a commanded step
		}
		if s.Aborted != "" {
			return best, fmt.Sprintf("at %d%% the sweep aborted: %s", s.Duty, s.Aborted), ok
		}
		if sp := s.spreadPct(); sp > limit {
			return best, fmt.Sprintf("at %d%% the fans stopped tracking together (spread %d%%, was %d%% at the top of the sweep)",
				s.Duty, sp, ref), ok
		}
		// best is the last accepted step, so it is also the previous one.
		if ok {
			slope := (best.AvgRPM - s.AvgRPM) / max(best.Duty-s.Duty, 1)
			if slope < clampMinSlopeRPM || (steepest > 0 && slope < steepest*clampSlopeFracPct/100) {
				return best, fmt.Sprintf("at %d%% RPM stopped responding to duty (%d -> %d, %d RPM per point against %d earlier), so the firmware is clamping",
					s.Duty, best.AvgRPM, s.AvgRPM, slope, steepest), ok
			}
			if slope > steepest {
				steepest = slope
			}
		}
		best, ok = s, true
	}
	return best, "", ok
}

// referenceSpread is the spread the host shows before duty is wound down: the
// baseline reading if the sweep took one, else its first commanded step. Fans
// of different sizes and positions never turn at the same rate, so this is the
// zero point the later steps are judged against.
func referenceSpread(samples []sweepSample) int {
	for _, s := range samples {
		if s.Duty < 0 {
			return s.spreadPct()
		}
	}
	for _, s := range samples {
		if s.Duty >= 0 {
			return s.spreadPct()
		}
	}
	return 0
}

// quieterDB estimates the noise change from an RPM change. Fan noise power
// scales roughly with the fifth power of speed, so this is an estimate from
// RPM alone, not a measurement.
func quieterDB(from, to int) float64 {
	if from <= 0 || to <= 0 {
		return 0
	}
	return 50 * math.Log10(float64(to)/float64(from))
}

func renderSuggestion(w io.Writer, samples []sweepSample) {
	best, reason, ok := analyseSweep(samples)
	_, _ = fmt.Fprintln(w)
	if !ok {
		_, _ = fmt.Fprintln(w, "suggestion: no step was usable")
		if reason != "" {
			_, _ = fmt.Fprintln(w, "  "+reason)
		}
		return
	}

	// Compare against the highest commanded step, which is the status quo the
	// sweep started from.
	var ref sweepSample
	for _, s := range samples {
		if s.Duty >= 0 {
			ref = s
			break
		}
	}

	_, _ = fmt.Fprintf(w, "suggested floor: %d%% -- %d RPM", best.Duty, best.AvgRPM)
	if ref.Duty > best.Duty {
		_, _ = fmt.Fprintf(w, ", about %.0f dB quieter than %d%% (estimated from RPM)",
			-quieterDB(ref.AvgRPM, best.AvgRPM), ref.Duty)
	}
	_, _ = fmt.Fprintln(w)
	if reason != "" {
		_, _ = fmt.Fprintln(w, "  lower steps rejected: "+reason)
	}
	if best.HaveGPU {
		_, _ = fmt.Fprintf(w, "  at that floor: %d°C %s, %d°C GPU\n", best.TempC, best.Sensor, best.GPUC)
	}
	_, _ = fmt.Fprintf(w, "\n  curve:\n    - max_temp: <see below>\n      percent: %d\n", best.Duty)
	_, _ = fmt.Fprintf(w, `
  Set max_temp above the EQUILIBRIUM idle temperature, not the %d°C measured
  here: a settle period measures a transient, and a chassis takes several
  minutes to reach steady state. Hold this duty at idle for ~10 minutes, take
  the temperature it flattens at, and add a couple of degrees of margin.
`, best.TempC)

	if best.HaveGPU {
		// The trap worth naming: the commanded duty is the max of both curves,
		// and each curve is flat below its own first anchor, so a gpu.curve
		// still starting at (say) 55C -> 20%% pins idle at 20%% no matter what
		// the CPU curve's floor says.
		_, _ = fmt.Fprint(w, `
  This host has GPU monitoring enabled, so lower the bottom anchor of BOTH the
  main curve and gpu.curve. The daemon commands the higher of the two, and each
  is flat below its own first anchor -- a gpu.curve whose lowest anchor is still
  at 20% holds idle at 20% however low the main curve goes.

  A GPU idling this cool is not the constraint under load: the upper anchors and
  lookahead are what keep it off its throttle point. The floor only governs idle.
`)
	} else {
		_, _ = fmt.Fprint(w, `
  This suggestion is based on fan behaviour and the selected sensors only. It
  cannot see passively-cooled cards that depend on chassis airflow and expose
  no temperature of their own -- an HBA or a datacenter GPU may set a higher
  floor than the fans and CPUs alone imply.
`)
	}
}
