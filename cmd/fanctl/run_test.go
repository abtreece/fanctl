package main

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/abtreece/fanctl/internal/config"
	"github.com/abtreece/fanctl/internal/fan"
	"github.com/abtreece/fanctl/internal/ipmi"
)

func TestSelectMaxTemp(t *testing.T) {
	temps := []ipmi.Temp{
		{ID: "04h", Name: "Inlet Temp", Celsius: 22},
		{ID: "0Eh", Name: "Temp", Celsius: 36},
		{ID: "0Fh", Name: "Temp", Celsius: 39},
		{ID: "01h", Name: "Exhaust Temp", Celsius: 30},
	}
	sel := config.SensorConfig{NameMatch: []string{"Temp"}, NameExclude: []string{"Inlet", "Exhaust"}}
	got, name, ok := selectMaxTemp(temps, sel)
	if !ok || got != 39 || name != "Temp" {
		t.Fatalf("selectMaxTemp = %d, %q, %v; want 39, \"Temp\", true", got, name, ok)
	}
}

func TestSelectMaxTempByID(t *testing.T) {
	temps := []ipmi.Temp{
		{ID: "0Eh", Name: "Temp", Celsius: 36},
		{ID: "0Fh", Name: "Temp", Celsius: 39},
	}
	sel := config.SensorConfig{IDs: []string{"0Eh"}}
	got, _, ok := selectMaxTemp(temps, sel)
	if !ok || got != 36 {
		t.Fatalf("selectMaxTemp by id = %d, %v; want 36, true", got, ok)
	}
}

func TestSelectMaxTempNoMatch(t *testing.T) {
	temps := []ipmi.Temp{{ID: "04h", Name: "Inlet Temp", Celsius: 22}}
	sel := config.SensorConfig{NameMatch: []string{"Temp"}, NameExclude: []string{"Inlet"}}
	if _, _, ok := selectMaxTemp(temps, sel); ok {
		t.Fatal("expected no match")
	}
}

// recordingRunner counts how many raw set commands were issued.
type recordingRunner struct {
	temps string
	raws  int
}

func (r *recordingRunner) run(_ context.Context, _ string, args ...string) ([]byte, error) {
	if len(args) >= 2 && args[0] == "sdr" {
		return []byte(r.temps), nil
	}
	if len(args) >= 1 && args[0] == "raw" {
		r.raws++
	}
	return nil, nil
}

func newTestController(rr *recordingRunner) *controller {
	return &controller{
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		client:  ipmi.New(ipmi.Options{Bin: "ipmitool"}, rr.run),
		curve:   fan.Curve{Bands: []fan.Band{{MaxTemp: 50, Percent: 10}, {MaxTemp: 60, Percent: 20}, {MaxTemp: 68, Percent: 30}, {MaxTemp: 75, Percent: 45}}, Hysteresis: 4},
		sensors: config.SensorConfig{NameMatch: []string{"Temp"}, NameExclude: []string{"Inlet"}},
		level:   fan.Initial(),
	}
}

func TestControllerOnlyActsOnChange(t *testing.T) {
	rr := &recordingRunner{temps: "Temp | 0Eh | ok | 3.1 | 40 degrees C"}
	c := newTestController(rr)

	// First step: 40°C -> band 0 (10%). Expect manual + setpercent = 2 raws.
	if err := c.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rr.raws != 2 {
		t.Fatalf("first step issued %d raw commands, want 2", rr.raws)
	}
	// Second step at the same temperature must issue nothing further.
	if err := c.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rr.raws != 2 {
		t.Fatalf("second step issued more raws (%d); want no change", rr.raws)
	}
}

func TestControllerDryRunIssuesNoCommands(t *testing.T) {
	rr := &recordingRunner{temps: "Temp | 0Eh | ok | 3.1 | 40 degrees C"}
	c := newTestController(rr)
	c.dryRun = true
	if err := c.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rr.raws != 0 {
		t.Fatalf("dry-run issued %d raw commands, want 0", rr.raws)
	}
}
