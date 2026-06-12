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
	// Hysteresis is the downward °C margin before stepping to a cooler band.
	Hysteresis int
	// Sensors selects which temperature SDR records drive the curve.
	Sensors SensorConfig
	// Curve is the ascending temperature->percent mapping.
	Curve []fan.Band
	// Connection selects in-band (default) or out-of-band BMC access.
	Connection ConnectionConfig
	// Metrics optionally exposes a Prometheus endpoint.
	Metrics MetricsConfig
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

// Curve assembles a fan.Curve from the config.
func (c *Config) FanCurve() fan.Curve {
	return fan.Curve{Bands: c.Curve, Hysteresis: c.Hysteresis}
}

// Default returns a Config seeded with built-in defaults for the given path.
func Default(path string) *Config {
	return &Config{
		ConfigFile:   path,
		IPMITool:     "ipmitool",
		PollInterval: 30 * time.Second,
		Hysteresis:   4,
		Sensors: SensorConfig{
			NameMatch:   []string{"Temp"},
			NameExclude: []string{"Inlet", "Exhaust"},
		},
		Connection: ConnectionConfig{Interface: "open"},
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
	if len(cfg.Sensors.IDs) == 0 && len(cfg.Sensors.NameMatch) == 0 {
		return fmt.Errorf("sensors: set at least one of ids or name_match")
	}
	if err := cfg.FanCurve().Validate(); err != nil {
		return fmt.Errorf("curve: %w", err)
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
