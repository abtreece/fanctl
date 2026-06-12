package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadFileOverlaysDefaults(t *testing.T) {
	path := writeTemp(t, `
poll_interval: 15s
hysteresis: 6
sensors:
  ids: ["0Eh", "0Fh"]
curve:
  - { max_temp: 40, percent: 5 }
  - { max_temp: 70, percent: 50 }
connection:
  interface: lanplus
  host: 10.0.0.5
  username: admin
  password: ${TEST_IPMI_PW}
`)
	t.Setenv("TEST_IPMI_PW", "s3cret")

	cfg := Default(path)
	if err := LoadFile(path, cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.PollInterval != 15*time.Second {
		t.Errorf("poll_interval = %s, want 15s", cfg.PollInterval)
	}
	if cfg.Hysteresis != 6 {
		t.Errorf("hysteresis = %d, want 6", cfg.Hysteresis)
	}
	if len(cfg.Sensors.IDs) != 2 || cfg.Sensors.IDs[0] != "0Eh" {
		t.Errorf("sensors.ids = %v", cfg.Sensors.IDs)
	}
	if len(cfg.Curve) != 2 || cfg.Curve[1].Percent != 50 {
		t.Errorf("curve = %+v", cfg.Curve)
	}
	if cfg.Connection.Interface != "lanplus" || cfg.Connection.Host != "10.0.0.5" {
		t.Errorf("connection = %+v", cfg.Connection)
	}
	if cfg.Connection.Password != "s3cret" {
		t.Errorf("password env expansion failed: %q", cfg.Connection.Password)
	}
}

func TestLoadFileMissingKeepsDefaults(t *testing.T) {
	path := writeTemp(t, "hysteresis: 3\n")
	cfg := Default(path)
	if err := LoadFile(path, cfg); err != nil {
		t.Fatal(err)
	}
	// Untouched fields keep defaults.
	if cfg.PollInterval != 30*time.Second {
		t.Errorf("poll_interval = %s, want default 30s", cfg.PollInterval)
	}
	if len(cfg.Curve) != 4 {
		t.Errorf("curve len = %d, want default 4", len(cfg.Curve))
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"default ok", func(*Config) {}, false},
		{"lanplus needs host", func(c *Config) { c.Connection.Interface = "lanplus" }, true},
		{"lanplus with host ok", func(c *Config) { c.Connection.Interface = "lanplus"; c.Connection.Host = "h" }, false},
		{"bad interface", func(c *Config) { c.Connection.Interface = "serial" }, true},
		{"sub-second poll", func(c *Config) { c.PollInterval = time.Millisecond }, true},
		{"no sensor selector", func(c *Config) { c.Sensors.NameMatch = nil; c.Sensors.IDs = nil }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default("x")
			tt.mutate(cfg)
			err := Validate(cfg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
