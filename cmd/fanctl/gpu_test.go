package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/abtreece/fanctl/internal/config"
	"github.com/abtreece/fanctl/internal/fan"
	"github.com/abtreece/fanctl/internal/gpu"
	"github.com/abtreece/fanctl/internal/ipmi"
)

// gpuRunner serves IPMI temps for the "sdr" call and a GPU temp for nvidia-smi.
type gpuRunner struct {
	cpuTemps string
	gpuOut   string
	gpuErr   error
}

func (r *gpuRunner) ipmi(_ context.Context, _ string, args ...string) ([]byte, error) {
	if len(args) >= 1 && args[0] == "sdr" {
		return []byte(r.cpuTemps), nil
	}
	return nil, nil // raw/other commands succeed
}

func (r *gpuRunner) nvidia(_ context.Context, _ string, _ ...string) ([]byte, error) {
	return []byte(r.gpuOut), r.gpuErr
}

func newGPUController(rr *gpuRunner) *controller {
	c := &controller{
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		client: ipmi.New(ipmi.Options{Bin: "ipmitool"}, rr.ipmi),
		gpu:    gpu.New("nvidia-smi", rr.nvidia),
	}
	c.setTunables(
		fan.Curve{Bands: []fan.Band{{MaxTemp: 50, Percent: 10}, {MaxTemp: 60, Percent: 20}, {MaxTemp: 68, Percent: 30}, {MaxTemp: 75, Percent: 45}}, Hysteresis: 4},
		config.SensorConfig{NameMatch: []string{"Temp"}, NameExclude: []string{"Inlet"}},
	)
	return c
}

// Cool CPU but hot GPU must drive the curve off the GPU temperature.
func TestGPUDrivesCurveWhenHotter(t *testing.T) {
	rr := &gpuRunner{cpuTemps: "Temp | 0Eh | ok | 3.1 | 40 degrees C", gpuOut: "72\n"}
	c := newGPUController(rr)
	if err := c.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 40°C CPU alone -> band 0 (10%); GPU 72°C -> band 3 (45%).
	if s := c.snapshot(); s.Percent != 45 {
		t.Fatalf("commanded %d%%, want 45%% (GPU at 72°C should drive the curve)", s.Percent)
	}
}

// GPU enabled but unreadable must fail safe to BMC automatic control.
func TestGPUUnreadableFailsSafeToAuto(t *testing.T) {
	rr := &gpuRunner{cpuTemps: "Temp | 0Eh | ok | 3.1 | 40 degrees C", gpuErr: errors.New("nvidia-smi not found")}
	c := newGPUController(rr)
	if err := c.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := c.snapshot()
	if !s.BMCAuto || s.Percent != -1 {
		t.Fatalf("expected fail-safe to BMC auto; got BMCAuto=%v Percent=%d", s.BMCAuto, s.Percent)
	}
}

func TestGPUSnapshotRecordsTemp(t *testing.T) {
	rr := &gpuRunner{cpuTemps: "Temp | 0Eh | ok | 3.1 | 40 degrees C", gpuOut: "55\n"}
	c := newGPUController(rr)
	if err := c.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := c.snapshot()
	if !s.HaveGPU || s.GPUTempC != 55 {
		t.Fatalf("snapshot GPU temp = %d (have=%v), want 55", s.GPUTempC, s.HaveGPU)
	}
}
