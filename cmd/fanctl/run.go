package main

import (
	"context"
	"fmt"
	"log/slog"
	"os/signal"
	"syscall"
	"time"

	"github.com/abtreece/fanctl/internal/config"
	"github.com/abtreece/fanctl/internal/fan"
	"github.com/abtreece/fanctl/internal/ipmi"
)

// runDaemon runs the control loop until SIGINT/SIGTERM, then restores BMC
// automatic control. With once=true it performs a single iteration and exits.
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

func runDaemon(log *slog.Logger, cfg *config.Config, dryRun, once bool) error {
	client := newIPMIClient(cfg)
	ctrl := &controller{
		log:     log,
		client:  client,
		curve:   cfg.FanCurve(),
		sensors: cfg.Sensors,
		dryRun:  dryRun,
		level:   fan.Initial(),
	}

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
		"dry_run", dryRun,
	)

	// Always hand control back to the BMC when we exit, so a stopped daemon
	// never leaves the fans pinned.
	defer ctrl.handBackToAuto()

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
		case <-ticker.C:
			if err := ctrl.step(ctx); err != nil {
				log.Warn("control step failed; handing to BMC automatic control", "err", err)
				ctrl.fallbackAuto(ctx)
			}
		}
	}
}

// controller holds the loop state: the last applied level so we only issue an
// IPMI command when the level actually changes.
type controller struct {
	log     *slog.Logger
	client  *ipmi.Client
	curve   fan.Curve
	sensors config.SensorConfig
	dryRun  bool
	level   int
}

// step reads temperatures, computes the next level, and applies it if changed.
func (c *controller) step(ctx context.Context) error {
	temps, err := c.client.Temperatures(ctx)
	if err != nil {
		return err
	}
	t, name, ok := selectMaxTemp(temps, c.sensors)
	if !ok {
		// No usable temperature reading: fail safe to BMC automatic control.
		c.apply(ctx, fan.AutoLevel, 0, "no matching temperature sensors")
		return nil
	}
	next := c.curve.Level(c.level, t)
	pct, _ := c.curve.Percent(next)
	c.apply(ctx, next, pct, fmt.Sprintf("%s=%d°C", name, t))
	return nil
}

// apply issues the IPMI command for level, but only when it differs from the
// last applied level. AutoLevel hands control back to the BMC.
func (c *controller) apply(ctx context.Context, level, pct int, reason string) {
	if level == c.level {
		return
	}
	if level == fan.AutoLevel {
		c.log.Info("fans -> BMC automatic", "reason", reason)
		if !c.dryRun {
			if err := c.client.SetAuto(ctx); err != nil {
				c.log.Warn("set auto failed", "err", err)
				return
			}
		}
		c.level = level
		return
	}
	c.log.Info("fans -> manual", "percent", pct, "reason", reason)
	if !c.dryRun {
		if err := c.client.SetManual(ctx); err != nil {
			c.log.Warn("set manual failed", "err", err)
			return
		}
		if err := c.client.SetPercent(ctx, pct); err != nil {
			c.log.Warn("set percent failed", "percent", pct, "err", err)
			return
		}
	}
	c.level = level
}

// fallbackAuto forces BMC automatic control after an error, bypassing the
// change-detection in apply so the safe state is always re-asserted.
func (c *controller) fallbackAuto(ctx context.Context) {
	if c.dryRun {
		return
	}
	if err := c.client.SetAuto(ctx); err != nil {
		c.log.Warn("fallback set auto failed", "err", err)
		return
	}
	c.level = fan.AutoLevel
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
