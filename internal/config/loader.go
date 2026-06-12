package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/abtreece/fanctl/internal/fan"
)

// rawFile mirrors the YAML file structure. It is unmarshalled and then merged
// into a Config seeded with defaults, so any field omitted from the file keeps
// its default value.
type rawFile struct {
	IPMITool     string `yaml:"ipmitool"`
	PollInterval string `yaml:"poll_interval"`
	Hysteresis   *int   `yaml:"hysteresis"`
	Sensors      struct {
		IDs         []string `yaml:"ids"`
		NameMatch   []string `yaml:"name_match"`
		NameExclude []string `yaml:"name_exclude"`
	} `yaml:"sensors"`
	Curve []struct {
		MaxTemp int `yaml:"max_temp"`
		Percent int `yaml:"percent"`
	} `yaml:"curve"`
	Connection struct {
		Interface string `yaml:"interface"`
		Host      string `yaml:"host"`
		Username  string `yaml:"username"`
		Password  string `yaml:"password"`
	} `yaml:"connection"`
	Metrics struct {
		Listen string `yaml:"listen"`
	} `yaml:"metrics"`
	GPU struct {
		Enabled *bool  `yaml:"enabled"`
		Command string `yaml:"command"`
	} `yaml:"gpu"`
}

// LoadFile reads a YAML config file and merges its values onto cfg (which the
// caller seeds with Default). Environment variables in the file are expanded.
func LoadFile(path string, cfg *Config) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config file: %w", err)
	}

	var raw rawFile
	if err := yaml.Unmarshal([]byte(os.ExpandEnv(string(data))), &raw); err != nil {
		return fmt.Errorf("parse config file: %w", err)
	}

	if raw.IPMITool != "" {
		cfg.IPMITool = raw.IPMITool
	}
	if raw.PollInterval != "" {
		d, err := time.ParseDuration(raw.PollInterval)
		if err != nil {
			return fmt.Errorf("poll_interval: %w", err)
		}
		cfg.PollInterval = d
	}
	if raw.Hysteresis != nil {
		cfg.Hysteresis = *raw.Hysteresis
	}
	if len(raw.Sensors.IDs) > 0 {
		cfg.Sensors.IDs = raw.Sensors.IDs
	}
	if len(raw.Sensors.NameMatch) > 0 {
		cfg.Sensors.NameMatch = raw.Sensors.NameMatch
	}
	if len(raw.Sensors.NameExclude) > 0 {
		cfg.Sensors.NameExclude = raw.Sensors.NameExclude
	}
	if len(raw.Curve) > 0 {
		bands := make([]fan.Band, len(raw.Curve))
		for i, b := range raw.Curve {
			bands[i] = fan.Band{MaxTemp: b.MaxTemp, Percent: b.Percent}
		}
		cfg.Curve = bands
	}

	if raw.Connection.Interface != "" {
		cfg.Connection.Interface = raw.Connection.Interface
	}
	if raw.Connection.Host != "" {
		cfg.Connection.Host = raw.Connection.Host
	}
	if raw.Connection.Username != "" {
		cfg.Connection.Username = raw.Connection.Username
	}
	if raw.Connection.Password != "" {
		cfg.Connection.Password = raw.Connection.Password
	}

	if raw.Metrics.Listen != "" {
		cfg.Metrics.Listen = raw.Metrics.Listen
	}

	if raw.GPU.Enabled != nil {
		cfg.GPU.Enabled = *raw.GPU.Enabled
	}
	if raw.GPU.Command != "" {
		cfg.GPU.Command = raw.GPU.Command
	}

	return nil
}
