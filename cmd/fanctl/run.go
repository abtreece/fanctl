package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/abtreece/fanctl/internal/config"
	"github.com/abtreece/fanctl/internal/fan"
	"github.com/abtreece/fanctl/internal/gpu"
	"github.com/abtreece/fanctl/internal/ipmi"
)

// verifyMinDelta is the commanded duty change (percent points) large enough
// that the fans' RPM must visibly move by the next poll.
const verifyMinDelta = 15

// verifyRPMFrac is the fractional RPM change below which a large duty change
// is judged to have had no effect (BMC ignoring manual control).
const verifyRPMFrac = 0.03

// hotSlope is the temperature rise (°C/min) at or above which the hot poll
// cadence is used.
const hotSlope = 1.0

// newIPMIClient builds an ipmi.Client from the resolved config (in-band or
// out-of-band depending on connection.interface).
func newIPMIClient(cfg *config.Config) *ipmi.Client {
	return ipmi.New(ipmi.Options{
		Bin:       cfg.IPMITool,
		Interface: cfg.Connection.Interface,
		Host:      cfg.Connection.Host,
		Username:  cfg.Connection.Username,
		Password:  cfg.Connection.Password,
	}, ipmi.ExecRunner)
}

// runDaemon runs the control loop until SIGINT/SIGTERM, then restores BMC
// automatic control. SIGHUP and changes to the config file trigger a live
// reload of the curve, sensors, hysteresis, and poll interval. With once=true
// it performs a single iteration and exits.
func runDaemon(log *slog.Logger, cfg *config.Config, dryRun, once bool) error {
	client := newIPMIClient(cfg)
	var gpuReader *gpu.Reader
	if cfg.GPU.Enabled {
		gpuReader = gpu.New(cfg.GPU.Command, gpu.ExecRunner)
	}
	ctrl := &controller{log: log, client: client, gpu: gpuReader, dryRun: dryRun, now: time.Now}
	ctrl.setTunables(tunablesFrom(cfg))

	// Every BMC round-trip is bounded: a wedged ipmitool must never stall the
	// loop, because a stalled loop leaves the fans pinned at the last commanded
	// duty with no path back to BMC automatic control.
	stepTimeout := cfg.EffectiveStepTimeout()

	if once {
		// Single shot: report what it would do and leave control as set.
		stepCtx, cancel := context.WithTimeout(context.Background(), stepTimeout)
		defer cancel()
		return ctrl.step(stepCtx)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Info("fanctl starting",
		"poll_interval", cfg.PollInterval,
		"poll_interval_hot", cfg.PollIntervalHot,
		"step_timeout", stepTimeout,
		"hysteresis", cfg.Hysteresis,
		"deadband", cfg.Deadband,
		"lookahead_min", cfg.Lookahead,
		"bands", len(cfg.Curve),
		"gpu_bands", len(cfg.GPU.Curve),
		"connection", connDesc(cfg),
		"dry_run", dryRun,
	)

	// Always hand control back to the BMC when we exit, so a stopped daemon
	// never leaves the fans pinned.
	defer ctrl.handBackToAuto()

	// intervalCh wakes the loop after a reload so it can re-evaluate the
	// ticker cadence, which the loop owns.
	intervalCh := make(chan struct{}, 1)
	reload := func() {
		fresh := config.Default(cfg.ConfigFile)
		if err := config.LoadFile(cfg.ConfigFile, fresh); err != nil && !errors.Is(err, fs.ErrNotExist) {
			log.Warn("reload: load failed; keeping current config", "err", err)
			return
		}
		if err := config.Validate(fresh); err != nil {
			log.Warn("reload: invalid config; keeping current config", "err", err)
			return
		}
		// Connection and ipmitool binary are bound into the client at startup.
		// step_timeout is fixed too: the watchdog's grace period is derived from
		// it, so changing one without the other would desync them.
		if fresh.IPMITool != cfg.IPMITool || fresh.Connection != cfg.Connection || fresh.StepTimeout != cfg.StepTimeout {
			log.Warn("reload: connection/ipmitool/step_timeout changes require a restart and were not applied")
		}
		ctrl.setTunables(tunablesFrom(fresh))
		ctrl.recordReload()
		// Keep the connection/ipmitool the client was built with, and the
		// step_timeout the loop is running with, so the in-memory config never
		// disagrees with what is actually in effect.
		fresh.IPMITool = cfg.IPMITool
		fresh.Connection = cfg.Connection
		fresh.StepTimeout = cfg.StepTimeout
		*cfg = *fresh
		select {
		case intervalCh <- struct{}{}:
		default:
		}
		log.Info("config reloaded",
			"poll_interval", fresh.PollInterval,
			"poll_interval_hot", fresh.PollIntervalHot,
			"hysteresis", fresh.Hysteresis,
			"deadband", fresh.Deadband,
			"lookahead_min", fresh.Lookahead,
			"bands", len(fresh.Curve),
			"gpu_bands", len(fresh.GPU.Curve),
		)
	}

	// SIGHUP triggers a reload.
	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)
	defer signal.Stop(sighup)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-sighup:
				log.Info("SIGHUP received; reloading config")
				reload()
			}
		}
	}()

	// Watch the config file and reload on change. Watch failure is non-fatal:
	// the daemon keeps running and SIGHUP reload still works.
	if w, err := newConfigWatcher(cfg.ConfigFile, reload); err != nil {
		log.Warn("config file watch disabled", "path", cfg.ConfigFile, "err", err)
	} else {
		go w.run(ctx)
		defer func() { _ = w.close() }()
		log.Info("watching config file for changes", "path", cfg.ConfigFile)
	}

	if cfg.Metrics.Listen != "" {
		srv := startMetricsServer(log, cfg.Metrics.Listen, ctrl)
		defer func() {
			shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutCtx)
		}()
	}

	step := func() {
		ctrl.runStep(ctx, stepTimeout)
	}
	step()

	// Tell systemd we are up only after the first control step, so `systemctl
	// start` blocks until the fans are actually under management.
	if err := sdNotify("READY=1"); err != nil {
		log.Warn("systemd READY notification failed", "err", err)
	}

	// health carries the watchdog's liveness probe. It is unbuffered on purpose:
	// the send only completes when the loop below actually receives it, so a
	// wedged loop withholds the watchdog ping and systemd restarts us.
	health := make(chan struct{})
	if wd := watchdogInterval(); wd > 0 {
		grace := watchdogGrace(stepTimeout, wd)
		if grace < stepTimeout {
			log.Warn("WatchdogSec is small relative to step_timeout; a slow BMC may trip the watchdog",
				"watchdog_interval", wd, "step_timeout", stepTimeout, "grace", grace)
		}
		log.Info("systemd watchdog enabled", "interval", wd, "grace", grace)
		go runWatchdog(ctx, log, health, wd, grace)
	}

	current := ctrl.desiredInterval()
	ticker := time.NewTicker(current)
	defer ticker.Stop()
	retick := func() {
		if d := ctrl.desiredInterval(); d != current {
			current = d
			ticker.Reset(d)
			log.Debug("poll cadence changed", "interval", d)
		}
	}
	retick()
	for {
		select {
		case <-ctx.Done():
			log.Info("shutting down, restoring BMC automatic control")
			return nil
		case <-health:
			// Watchdog liveness probe; receiving it is the whole response.
		case <-intervalCh:
			retick()
		case <-ticker.C:
			step()
			retick()
		}
	}
}

// tunables groups everything a reload may swap while the loop runs.
type tunables struct {
	curve    fan.Curve
	gpuCurve fan.Curve
	gov      fan.Governor
	sensors  config.SensorConfig
	// lookahead is the predictive horizon in minutes.
	lookahead float64
	// pollIdle/pollHot are the slow and fast cadences; pollHot==0 disables
	// adaptive polling. hotDuty is the commanded duty that counts as hot.
	pollIdle time.Duration
	pollHot  time.Duration
	hotDuty  int
	// reassert re-issues manual control this long after the last IPMI write
	// even when the duty is unchanged. 0 disables.
	reassert time.Duration
}

func tunablesFrom(cfg *config.Config) tunables {
	return tunables{
		curve:     cfg.FanCurve(),
		gpuCurve:  cfg.GPUFanCurve(),
		gov:       cfg.Governor(),
		sensors:   cfg.Sensors,
		lookahead: cfg.Lookahead,
		pollIdle:  cfg.PollInterval,
		pollHot:   cfg.PollIntervalHot,
		hotDuty:   cfg.HotDuty,
		reassert:  cfg.ReassertInterval,
	}
}

// runStep performs one control iteration under a deadline, handing the fans
// back to BMC automatic control if it fails or times out.
func (c *controller) runStep(ctx context.Context, timeout time.Duration) {
	stepCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := c.step(stepCtx)
	if err == nil {
		return
	}
	// A killed ipmitool surfaces as "signal: killed" rather than the context
	// error, so ask the context itself whether the deadline was the cause.
	if errors.Is(stepCtx.Err(), context.DeadlineExceeded) {
		c.log.Warn("control step timed out; handing to BMC automatic control", "timeout", timeout)
	} else {
		c.log.Warn("control step failed; handing to BMC automatic control", "err", err)
	}
	c.fallbackAuto(ctx, timeout)
}

// controller holds the loop state. Tunables are mutex-guarded so a reload can
// swap them while the loop runs. state is the last commanded fan state; an
// IPMI command is issued on state change, on the periodic re-assert, or when
// RPM verification shows a command had no effect.
type controller struct {
	log    *slog.Logger
	client *ipmi.Client
	gpu    *gpu.Reader // nil when GPU monitoring is disabled
	dryRun bool
	now    func() time.Time

	mu        sync.Mutex
	tun       tunables
	state     fan.State
	cpuPred   fan.Predictor
	gpuPred   fan.Predictor
	lastApply time.Time
	// verification of the previous apply: expectPct is the commanded duty and
	// baseRPM the average RPM observed when it was issued.
	verifyPending bool
	verifyDelta   int
	baseRPM       int

	// Observed state, for the metrics endpoint.
	obs Snapshot
}

// Snapshot is a point-in-time view of the controller for metrics.
type Snapshot struct {
	HaveObs     bool
	TempC       int // hottest temperature driving the curve (CPU or GPU)
	HaveGPU     bool
	GPUTempC    int
	SlopeCPM    float64 // steepest temperature slope across sources, °C/min
	ComputedPct float64 // interpolated duty target before governor smoothing
	FanRPM      int
	Percent     int // -1 when handed to BMC automatic control
	BMCAuto     bool
	HotPoll     bool
	Steps       uint64
	StepErrors  uint64
	Reloads     uint64
	Reasserts   uint64
	VerifyFails uint64
}

// snapshot returns a copy of the current observed state.
func (c *controller) snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.obs
}

func (c *controller) recordReload() {
	c.mu.Lock()
	c.obs.Reloads++
	c.mu.Unlock()
}

func (c *controller) recordError() {
	c.mu.Lock()
	c.obs.Steps++
	c.obs.StepErrors++
	c.mu.Unlock()
}

// setTunables swaps the tunables and forces a fresh decision on the next
// step. Called at startup and on reload.
func (c *controller) setTunables(t tunables) {
	c.mu.Lock()
	c.tun = t
	c.state = fan.State{}
	c.cpuPred.Reset()
	c.gpuPred.Reset()
	c.verifyPending = false
	c.mu.Unlock()
}

// desiredInterval picks the poll cadence from the last observation: the hot
// cadence while duty is high or temperature is rising, otherwise the idle one.
func (c *controller) desiredInterval() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.tun
	if t.pollHot == 0 {
		return t.pollIdle
	}
	if c.obs.HaveObs && (c.obs.HotPoll) {
		return t.pollHot
	}
	return t.pollIdle
}

// step reads temperatures, computes the next state, and applies it when the
// state changes, the re-assert interval has elapsed, or verification failed.
func (c *controller) step(ctx context.Context) error {
	c.mu.Lock()
	tun, prev := c.tun, c.state
	c.mu.Unlock()
	now := c.now()

	temps, err := c.client.Temperatures(ctx)
	if err != nil {
		c.recordError()
		return err
	}
	cpuC, cpuName, cpuOK := selectMaxTemp(temps, tun.sensors)

	// GPU temperature (optional). If GPU monitoring is enabled but the read
	// fails, fail safe to BMC automatic control — never cool a GPU host on CPU
	// data alone, since a passively-cooled GPU depends on chassis airflow.
	gpuC, gpuOK, gpuFail := 0, false, false
	if c.gpu != nil {
		if gtemps, gerr := c.gpu.Temperatures(ctx); gerr != nil {
			gpuFail = true
			c.log.Warn("gpu temperature unreadable; handing to BMC automatic control", "err", gerr)
		} else {
			gpuC, gpuOK = gpu.Max(gtemps)
		}
	}

	// Per-source predictive boost: evaluate each curve at where its source is
	// heading, then command the maximum of the per-source duties.
	c.mu.Lock()
	var cpuEff, gpuEff, slope float64
	if cpuOK {
		c.cpuPred.Observe(now, float64(cpuC))
		cpuEff = float64(cpuC) + c.cpuPred.Boost(tun.lookahead)
		slope = c.cpuPred.SlopePerMin()
	}
	if gpuOK {
		c.gpuPred.Observe(now, float64(gpuC))
		gpuEff = float64(gpuC) + c.gpuPred.Boost(tun.lookahead)
		if s := c.gpuPred.SlopePerMin(); s > slope {
			slope = s
		}
	}
	c.mu.Unlock()

	hottest, haveTemp := 0, false
	if cpuOK {
		hottest, haveTemp = cpuC, true
	}
	if gpuOK && (!haveTemp || gpuC > hottest) {
		hottest, haveTemp = gpuC, true
	}

	var next fan.State
	var target float64
	var reason string
	switch {
	case gpuFail:
		next = fan.State{Auto: true, Init: true}
		reason = "gpu temperature unreadable"
	case !haveTemp:
		next = fan.State{Auto: true, Init: true}
		reason = "no matching temperature sensors"
	default:
		wantAuto, canReclaim := false, true
		if cpuOK {
			d, a := tun.curve.Duty(cpuEff)
			target, wantAuto = math.Max(target, d), wantAuto || a
			canReclaim = canReclaim && float64(cpuC) <= tun.curve.ReclaimBelow()
		}
		if gpuOK {
			d, a := tun.gpuCurve.Duty(gpuEff)
			target, wantAuto = math.Max(target, d), wantAuto || a
			canReclaim = canReclaim && float64(gpuC) <= tun.gpuCurve.ReclaimBelow()
		}
		next = tun.gov.Next(prev, target, wantAuto, canReclaim)
		reason = describeTemps(cpuName, cpuC, cpuOK, gpuC, gpuOK)
		if slope >= 0.5 {
			reason += fmt.Sprintf(" rising=%.1f°C/min", slope)
		}
	}

	// Best-effort fan RPM for metrics and verification; never fails the step.
	fanRPM := 0
	if fans, ferr := c.client.Fans(ctx); ferr == nil {
		fanRPM = ipmi.AverageRPM(fans)
	}

	// Verify the previous apply took effect: a large duty change must move the
	// observed RPM. A flat response means the BMC is ignoring manual control
	// (e.g. iDRAC reset back to automatic) — re-assert and count it.
	c.mu.Lock()
	verifyFailed, failedDelta := false, 0
	if c.verifyPending {
		// An over-temp handoff to BMC-auto invalidates the expectation (the
		// BMC ramps the fans itself); just drop the pending check.
		if !next.Auto && fanRPM > 0 && c.baseRPM > 0 &&
			math.Abs(float64(fanRPM-c.baseRPM)) < verifyRPMFrac*float64(c.baseRPM) {
			verifyFailed, failedDelta = true, c.verifyDelta
			c.obs.VerifyFails++
		}
		c.verifyPending = false
	}
	reassertDue := tun.reassert > 0 && !prev.Auto && prev.Init && now.Sub(c.lastApply) >= tun.reassert
	c.mu.Unlock()
	if verifyFailed {
		c.log.Warn("fan duty change had no RPM effect; re-asserting manual control",
			"commanded_delta_pct", failedDelta, "rpm", fanRPM)
	}

	// Compare what would be commanded, not the whole struct: the governor also
	// carries hold bookkeeping that changes while a sub-deadband decrease is
	// being held, and treating that as a change would write to the BMC on every
	// poll of exactly the wobble the deadband exists to absorb.
	changed := !next.SameCommand(prev)
	if changed || reassertDue || verifyFailed {
		if reassertDue && !changed && !verifyFailed {
			c.log.Debug("re-asserting manual fan control", "percent", next.Pct)
		}
		if err := c.applyState(ctx, next, reason); err != nil {
			c.recordError()
			return err
		}
		c.mu.Lock()
		c.lastApply = now
		if reassertDue && !changed {
			c.obs.Reasserts++
		}
		// Arm verification when a manual duty change is big enough that the
		// RPM must respond by the next poll.
		if changed && !next.Auto && prev.Init && !prev.Auto && fanRPM > 0 {
			if d := next.Pct - prev.Pct; d >= verifyMinDelta || d <= -verifyMinDelta {
				c.verifyPending, c.verifyDelta, c.baseRPM = true, d, fanRPM
			}
		}
		c.mu.Unlock()
	}

	c.mu.Lock()
	// Commit the state only if a concurrent reload hasn't reset it meanwhile.
	if c.state == prev {
		c.state = next
	}
	c.obs.HaveObs = true
	c.obs.Steps++
	if haveTemp {
		c.obs.TempC = hottest
	}
	c.obs.HaveGPU = gpuOK
	if gpuOK {
		c.obs.GPUTempC = gpuC
	}
	c.obs.SlopeCPM = slope
	c.obs.ComputedPct = target
	c.obs.FanRPM = fanRPM
	if next.Auto {
		c.obs.Percent = -1
		c.obs.BMCAuto = true
	} else {
		c.obs.Percent = next.Pct
		c.obs.BMCAuto = false
	}
	c.obs.HotPoll = next.Auto || next.Pct >= tun.hotDuty || slope >= hotSlope
	c.mu.Unlock()
	return nil
}

// describeTemps builds a log reason like "cpu(Temp)=46°C gpu=72°C".
func describeTemps(cpuName string, cpuC int, cpuOK bool, gpuC int, gpuOK bool) string {
	parts := make([]string, 0, 2)
	if cpuOK {
		parts = append(parts, fmt.Sprintf("cpu(%s)=%d°C", cpuName, cpuC))
	}
	if gpuOK {
		parts = append(parts, fmt.Sprintf("gpu=%d°C", gpuC))
	}
	return strings.Join(parts, " ")
}

// applyState issues the IPMI commands for a state. Auto hands control back to
// the BMC. It does no locking and performs the (slow) IPMI I/O.
func (c *controller) applyState(ctx context.Context, s fan.State, reason string) error {
	if s.Auto {
		c.log.Info("fans -> BMC automatic", "reason", reason)
		if c.dryRun {
			return nil
		}
		return c.client.SetAuto(ctx)
	}
	c.log.Info("fans -> manual", "percent", s.Pct, "reason", reason)
	if c.dryRun {
		return nil
	}
	if err := c.client.SetManual(ctx); err != nil {
		return err
	}
	return c.client.SetPercent(ctx, s.Pct)
}

// fallbackAuto forces BMC automatic control after an error, bypassing
// change-detection so the safe state is always re-asserted. It takes its own
// deadline: the step may well have failed because the BMC is unresponsive, and
// the recovery path must not inherit that hang.
func (c *controller) fallbackAuto(ctx context.Context, timeout time.Duration) {
	if c.dryRun {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := c.client.SetAuto(ctx); err != nil {
		c.log.Warn("fallback set auto failed", "err", err)
		return
	}
	c.mu.Lock()
	c.state = fan.State{Auto: true, Init: true}
	c.verifyPending = false
	c.mu.Unlock()
}

// handBackToAuto restores BMC control on shutdown using a fresh context, since
// the loop context is already cancelled by then.
func (c *controller) handBackToAuto() {
	if c.dryRun {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.client.SetAuto(ctx); err != nil {
		c.log.Warn("restore auto on shutdown failed", "err", err)
	}
}

// connDesc is a short description of the BMC connection for logs.
func connDesc(cfg *config.Config) string {
	if cfg.Connection.Interface == "" || cfg.Connection.Interface == "open" {
		return "in-band"
	}
	return cfg.Connection.Interface + " " + cfg.Connection.Host
}

// selectMaxTemp returns the hottest selected sensor's temperature and name.
func selectMaxTemp(temps []ipmi.Temp, sel config.SensorConfig) (int, string, bool) {
	best := -1
	var name string
	for _, t := range temps {
		if !sel.Selects(t.ID, t.Name) {
			continue
		}
		if t.Celsius > best {
			best = t.Celsius
			name = t.Name
		}
	}
	if best < 0 {
		return 0, "", false
	}
	return best, name, true
}
