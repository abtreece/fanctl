package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
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
	ctrl := &controller{log: log, client: client, gpu: gpuReader, dryRun: dryRun}
	ctrl.setTunables(cfg.FanCurve(), cfg.Sensors)

	if once {
		// Single shot: report what it would do and leave control as set.
		return ctrl.step(context.Background())
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Info("fanctl starting",
		"poll_interval", cfg.PollInterval,
		"hysteresis", cfg.Hysteresis,
		"bands", len(cfg.Curve),
		"connection", connDesc(cfg),
		"dry_run", dryRun,
	)

	// Always hand control back to the BMC when we exit, so a stopped daemon
	// never leaves the fans pinned.
	defer ctrl.handBackToAuto()

	// intervalCh carries a new poll interval from a reload to the loop, which
	// owns the ticker.
	intervalCh := make(chan time.Duration, 1)
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
		// Connection and ipmitool binary are bound into the client at startup;
		// changing them needs a restart.
		if fresh.IPMITool != cfg.IPMITool || fresh.Connection != cfg.Connection {
			log.Warn("reload: connection/ipmitool changes require a restart and were not applied")
		}
		ctrl.setTunables(fresh.FanCurve(), fresh.Sensors)
		ctrl.recordReload()
		if fresh.PollInterval != cfg.PollInterval {
			select {
			case intervalCh <- fresh.PollInterval:
			default:
			}
		}
		cfg.PollInterval = fresh.PollInterval
		cfg.Hysteresis = fresh.Hysteresis
		cfg.Curve = fresh.Curve
		cfg.Sensors = fresh.Sensors
		log.Info("config reloaded", "poll_interval", fresh.PollInterval, "hysteresis", fresh.Hysteresis, "bands", len(fresh.Curve))
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

	if err := ctrl.step(ctx); err != nil {
		log.Warn("control step failed; handing to BMC automatic control", "err", err)
		ctrl.fallbackAuto(ctx)
	}

	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info("shutting down, restoring BMC automatic control")
			return nil
		case d := <-intervalCh:
			ticker.Reset(d)
		case <-ticker.C:
			if err := ctrl.step(ctx); err != nil {
				log.Warn("control step failed; handing to BMC automatic control", "err", err)
				ctrl.fallbackAuto(ctx)
			}
		}
	}
}

// controller holds the loop state. Tunables (curve, sensors) are mutex-guarded
// so a reload can swap them while the loop runs. level is the last applied fan
// level, so we only issue an IPMI command when it changes.
type controller struct {
	log    *slog.Logger
	client *ipmi.Client
	gpu    *gpu.Reader // nil when GPU monitoring is disabled
	dryRun bool

	mu      sync.Mutex
	curve   fan.Curve
	sensors config.SensorConfig
	level   int

	// Observed state, for the metrics endpoint.
	obs Snapshot
}

// Snapshot is a point-in-time view of the controller for metrics.
type Snapshot struct {
	HaveObs    bool
	TempC      int // hottest temperature driving the curve (CPU or GPU)
	HaveGPU    bool
	GPUTempC   int
	FanRPM     int
	Percent    int // -1 when handed to BMC automatic control
	BMCAuto    bool
	Steps      uint64
	StepErrors uint64
	Reloads    uint64
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

// setTunables swaps the curve and sensor selection and forces a fresh band
// selection on the next step. Called at startup and on reload.
func (c *controller) setTunables(curve fan.Curve, sensors config.SensorConfig) {
	c.mu.Lock()
	c.curve = curve
	c.sensors = sensors
	c.level = fan.Initial()
	c.mu.Unlock()
}

// step reads temperatures, computes the next level, and applies it if changed.
func (c *controller) step(ctx context.Context) error {
	c.mu.Lock()
	curve, sensors, prev := c.curve, c.sensors, c.level
	c.mu.Unlock()

	temps, err := c.client.Temperatures(ctx)
	if err != nil {
		c.recordError()
		return err
	}
	cpuC, cpuName, cpuOK := selectMaxTemp(temps, sensors)

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

	// Drive the curve off the hottest of CPU and GPU.
	hottest, haveTemp := 0, false
	if cpuOK {
		hottest, haveTemp = cpuC, true
	}
	if gpuOK && (!haveTemp || gpuC > hottest) {
		hottest, haveTemp = gpuC, true
	}

	var next, pct int
	var reason string
	switch {
	case gpuFail:
		next = fan.AutoLevel
		reason = "gpu temperature unreadable"
	case !haveTemp:
		next = fan.AutoLevel
		reason = "no matching temperature sensors"
	default:
		next = curve.Level(prev, hottest)
		pct, _ = curve.Percent(next)
		reason = describeTemps(cpuName, cpuC, cpuOK, gpuC, gpuOK)
	}

	// Best-effort fan RPM for metrics; never fails the step.
	fanRPM := 0
	if fans, ferr := c.client.Fans(ctx); ferr == nil {
		fanRPM = ipmi.AverageRPM(fans)
	}

	if next != prev {
		if err := c.applyLevel(ctx, next, pct, reason); err != nil {
			c.recordError()
			return err
		}
	}

	c.mu.Lock()
	// Commit the level only if a concurrent reload hasn't reset it meanwhile.
	if c.level == prev {
		c.level = next
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
	c.obs.FanRPM = fanRPM
	if next == fan.AutoLevel {
		c.obs.Percent = -1
		c.obs.BMCAuto = true
	} else {
		c.obs.Percent = pct
		c.obs.BMCAuto = false
	}
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

// applyLevel issues the IPMI commands for a level. AutoLevel hands control back
// to the BMC. It does no locking and performs the (slow) IPMI I/O.
func (c *controller) applyLevel(ctx context.Context, level, pct int, reason string) error {
	if level == fan.AutoLevel {
		c.log.Info("fans -> BMC automatic", "reason", reason)
		if c.dryRun {
			return nil
		}
		return c.client.SetAuto(ctx)
	}
	c.log.Info("fans -> manual", "percent", pct, "reason", reason)
	if c.dryRun {
		return nil
	}
	if err := c.client.SetManual(ctx); err != nil {
		return err
	}
	return c.client.SetPercent(ctx, pct)
}

// fallbackAuto forces BMC automatic control after an error, bypassing
// change-detection so the safe state is always re-asserted.
func (c *controller) fallbackAuto(ctx context.Context) {
	if c.dryRun {
		return
	}
	if err := c.client.SetAuto(ctx); err != nil {
		c.log.Warn("fallback set auto failed", "err", err)
		return
	}
	c.mu.Lock()
	c.level = fan.AutoLevel
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
