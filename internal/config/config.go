// Package config defines fanctl's configuration: the IPMI binary, poll cadence,
// temperature-sensor selection, and the fan curve. Defaults target a Dell
// PowerEdge 12G/13G host but every field is overridable from the YAML file.
package config

import (
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/abtreece/fanctl/internal/fan"
)

// DefaultPath is where the systemd unit and packaging place the config file.
const DefaultPath = "/etc/fanctl/config.yaml"

// Config is the fully-resolved configuration the daemon runs from.
type Config struct {
	ConfigFile string

	// IPMITool is the ipmitool binary name or path.
	IPMITool string
	// PollInterval is how often the controller re-reads temps and adjusts fans.
	PollInterval time.Duration
	// PollIntervalHot, when set, is the faster poll cadence used while the
	// system is hot or temperatures are rising. Zero disables adaptive polling.
	PollIntervalHot time.Duration
	// HotDuty is the commanded duty percent at or above which the hot poll
	// cadence is used.
	HotDuty int
	// StepTimeout bounds the BMC I/O of a single control iteration. A wedged
	// ipmitool must never stall the loop indefinitely, since a stalled loop
	// leaves the fans pinned at the last commanded duty.
	StepTimeout time.Duration
	// Hysteresis is the downward °C margin before reclaiming manual control
	// from BMC automatic mode.
	Hysteresis int
	// Deadband is the minimum percent-point decrease before duty is lowered;
	// increases always apply immediately.
	Deadband int
	// Lookahead is the predictive horizon in minutes: the curve is evaluated
	// at temp + slope*lookahead so fast rises get airflow early. 0 disables.
	Lookahead float64
	// ReassertInterval re-issues the manual-mode commands periodically even
	// when the duty is unchanged, recovering from an iDRAC that silently
	// reverted to automatic control. 0 disables.
	ReassertInterval time.Duration
	// Sensors selects which temperature SDR records drive the curve.
	Sensors SensorConfig
	// Curve is the ascending temperature->percent mapping.
	Curve []fan.Band
	// Connection selects in-band (default) or out-of-band BMC access.
	Connection ConnectionConfig
	// Metrics optionally exposes a Prometheus endpoint.
	Metrics MetricsConfig
	// GPU optionally factors NVIDIA GPU temperature into the curve.
	GPU GPUConfig
}

// GPUConfig enables factoring NVIDIA GPU temperature (via nvidia-smi) into the
// curve, for hosts with passively-cooled datacenter GPUs that depend on chassis
// airflow. When Enabled and the GPU temperature cannot be read, the controller
// fails safe to BMC automatic control rather than cooling on CPU data alone.
type GPUConfig struct {
	Enabled bool
	Command string // nvidia-smi binary; defaults to "nvidia-smi"
	// Curve, when non-empty, is a GPU-specific temperature->percent mapping.
	// The controller computes a duty per source and commands the maximum, so
	// a passively-cooled GPU and the CPUs each get the airflow they need
	// without one curve over-cooling for the other. Empty falls back to the
	// shared curve.
	Curve []fan.Band
}

// MetricsConfig configures the optional Prometheus metrics endpoint. An empty
// Listen disables it.
type MetricsConfig struct {
	Listen string // host:port, e.g. ":9466"; empty disables metrics
}

// ConnectionConfig selects how ipmitool reaches the BMC. The default empty/
// "open" interface is in-band via /dev/ipmi0. "lanplus" is out-of-band over the
// network and requires Host (Username/Password are typical). Put credentials in
// the file via env expansion, e.g. password: ${IPMI_PASSWORD}.
type ConnectionConfig struct {
	Interface string
	Host      string
	Username  string
	Password  string
}

// SensorConfig selects the temperature sensors whose hottest reading drives the
// curve. IDs, when non-empty, take precedence over name matching; otherwise a
// sensor is selected when its name contains any NameMatch substring and none of
// the NameExclude substrings (all case-insensitive). The Dell default — match
// "Temp", exclude "Inlet"/"Exhaust" — picks the CPU sensors across generations.
type SensorConfig struct {
	IDs         []string
	NameMatch   []string
	NameExclude []string
}

// Selects reports whether a sensor with the given id and name is selected.
func (s SensorConfig) Selects(id, name string) bool {
	if len(s.IDs) > 0 {
		for _, want := range s.IDs {
			if strings.EqualFold(strings.TrimSpace(want), strings.TrimSpace(id)) {
				return true
			}
		}
		return false
	}
	lname := strings.ToLower(name)
	for _, ex := range s.NameExclude {
		if ex != "" && strings.Contains(lname, strings.ToLower(ex)) {
			return false
		}
	}
	if len(s.NameMatch) == 0 {
		return true
	}
	for _, m := range s.NameMatch {
		if m != "" && strings.Contains(lname, strings.ToLower(m)) {
			return true
		}
	}
	return false
}

// FanCurve assembles the shared (CPU) fan.Curve from the config.
func (c *Config) FanCurve() fan.Curve {
	return fan.Curve{Bands: c.Curve, Hysteresis: c.Hysteresis}
}

// GPUFanCurve returns the GPU-specific curve, falling back to the shared
// curve when none is configured.
func (c *Config) GPUFanCurve() fan.Curve {
	if len(c.GPU.Curve) == 0 {
		return c.FanCurve()
	}
	return fan.Curve{Bands: c.GPU.Curve, Hysteresis: c.Hysteresis}
}

// Governor assembles the fan.Governor from the config.
func (c *Config) Governor() fan.Governor {
	return fan.Governor{Deadband: c.Deadband, MaxStepDown: maxStepDown, DeadbandHoldPolls: deadbandHoldPolls}
}

// maxStepDown caps how many percent-points the duty may fall per poll so fans
// wind down smoothly after load drops; not currently configurable.
const maxStepDown = 10

// deadbandHoldPolls is how many consecutive polls a sub-deadband move must
// persist before being applied, in either direction, so the duty can settle
// onto the curve's floor instead of being stranded above it while 1-point
// jitter is still absorbed rather than cycled on; not currently configurable.
const deadbandHoldPolls = 3

// EffectiveStepTimeout is the deadline applied to one control iteration. It is
// capped at the shortest poll cadence in play — the hot one when adaptive
// polling is enabled — so a slow step can never run past the next tick.
func (c *Config) EffectiveStepTimeout() time.Duration {
	shortest := c.PollInterval
	if c.PollIntervalHot > 0 && c.PollIntervalHot < shortest {
		shortest = c.PollIntervalHot
	}
	if c.StepTimeout > shortest {
		return shortest
	}
	return c.StepTimeout
}

// Default returns a Config seeded with built-in defaults for the given path.
func Default(path string) *Config {
	return &Config{
		ConfigFile:       path,
		IPMITool:         "ipmitool",
		PollInterval:     30 * time.Second,
		PollIntervalHot:  0,
		HotDuty:          50,
		StepTimeout:      15 * time.Second,
		Hysteresis:       4,
		Deadband:         3,
		Lookahead:        1.5,
		ReassertInterval: 5 * time.Minute,
		Sensors: SensorConfig{
			NameMatch:   []string{"Temp"},
			NameExclude: []string{"Inlet", "Exhaust"},
		},
		Connection: ConnectionConfig{Interface: "open"},
		GPU:        GPUConfig{Command: "nvidia-smi"},
		Curve: []fan.Band{
			{MaxTemp: 50, Percent: 10},
			{MaxTemp: 60, Percent: 20},
			{MaxTemp: 68, Percent: 30},
			{MaxTemp: 75, Percent: 45},
		},
	}
}

// Validate checks the resolved config is internally consistent.
func Validate(cfg *Config) error {
	if cfg.IPMITool == "" {
		return fmt.Errorf("ipmitool binary must be set")
	}
	if cfg.PollInterval < time.Second {
		return fmt.Errorf("poll_interval must be >= 1s; got %s", cfg.PollInterval)
	}
	if cfg.StepTimeout <= 0 {
		return fmt.Errorf("step_timeout must be > 0; got %s", cfg.StepTimeout)
	}
	if len(cfg.Sensors.IDs) == 0 && len(cfg.Sensors.NameMatch) == 0 {
		return fmt.Errorf("sensors: set at least one of ids or name_match")
	}
	if err := cfg.FanCurve().Validate(); err != nil {
		return fmt.Errorf("curve: %w", err)
	}
	if len(cfg.GPU.Curve) > 0 {
		if err := cfg.GPUFanCurve().Validate(); err != nil {
			return fmt.Errorf("gpu.curve: %w", err)
		}
	}
	if cfg.PollIntervalHot != 0 && cfg.PollIntervalHot < time.Second {
		return fmt.Errorf("poll_interval_hot must be >= 1s or 0; got %s", cfg.PollIntervalHot)
	}
	if cfg.PollIntervalHot > cfg.PollInterval {
		return fmt.Errorf("poll_interval_hot (%s) must not exceed poll_interval (%s)", cfg.PollIntervalHot, cfg.PollInterval)
	}
	if cfg.Deadband < 0 || cfg.Deadband > 50 {
		return fmt.Errorf("deadband %d out of range 0-50", cfg.Deadband)
	}
	if cfg.Lookahead < 0 || cfg.Lookahead > 10 {
		return fmt.Errorf("lookahead %g out of range 0-10 minutes", cfg.Lookahead)
	}
	if cfg.HotDuty < 0 || cfg.HotDuty > 100 {
		return fmt.Errorf("hot_duty %d out of range 0-100", cfg.HotDuty)
	}
	if cfg.ReassertInterval < 0 {
		return fmt.Errorf("reassert_interval must be >= 0")
	}
	switch cfg.Connection.Interface {
	case "", "open":
	case "lanplus":
		if cfg.Connection.Host == "" {
			return fmt.Errorf("connection.host is required when interface is lanplus")
		}
	default:
		return fmt.Errorf("connection.interface %q is not supported (use open or lanplus)", cfg.Connection.Interface)
	}
	if cfg.Metrics.Listen != "" {
		if _, _, err := net.SplitHostPort(cfg.Metrics.Listen); err != nil {
			return fmt.Errorf("metrics.listen %q is not a valid host:port: %w", cfg.Metrics.Listen, err)
		}
	}
	return nil
}
