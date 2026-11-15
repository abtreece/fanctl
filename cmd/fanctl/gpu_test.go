package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/abtreece/fanctl/internal/config"
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
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := &controller{
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		client: ipmi.New(ipmi.Options{Bin: "ipmitool"}, rr.ipmi),
		now:    func() time.Time { return clock },
	}
	c.setTunables(gpuTunables(rr))
	return c
}

// gpuTunables is testTunables with GPU monitoring enabled against rr.
func gpuTunables(rr *gpuRunner) tunables {
	tun := testTunables()
	tun.gpu = gpu.New("nvidia-smi", rr.nvidia)
	return tun
}

// Cool CPU but hot GPU must drive the curve off the GPU temperature.
func TestGPUDrivesCurveWhenHotter(t *testing.T) {
	rr := &gpuRunner{cpuTemps: "Temp | 0Eh | ok | 3.1 | 40 degrees C", gpuOut: "72\n"}
	c := newGPUController(rr)
	if err := c.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 40°C CPU alone -> 10%; GPU 72°C interpolates 68..75 -> ~39%.
	if s := c.snapshot(); s.Percent != 39 {
		t.Fatalf("commanded %d%%, want 39%% (GPU at 72°C should drive the curve)", s.Percent)
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

// Pulling the card and reloading with gpu.enabled: false must stop the GPU
// fail-safe and resume manual control. Before the reader moved into tunables
// it was bound at startup, so a reload left the daemon polling a missing card
// and parked in BMC auto until restart.
func TestReloadDisablingGPUResumesManualControl(t *testing.T) {
	rr := &gpuRunner{cpuTemps: "Temp | 0Eh | ok | 3.1 | 40 degrees C", gpuErr: errors.New("exit status 9")}
	c := newGPUController(rr)
	if err := c.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s := c.snapshot(); !s.BMCAuto {
		t.Fatalf("precondition: unreadable GPU should fail safe to BMC auto; got Percent=%d", s.Percent)
	}

	c.setTunables(testTunables()) // reload with gpu.enabled: false
	if err := c.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s := c.snapshot(); s.BMCAuto || s.Percent != 10 || s.HaveGPU {
		t.Fatalf("after disabling GPU: BMCAuto=%v Percent=%d HaveGPU=%v, want manual 10%% off CPU only",
			s.BMCAuto, s.Percent, s.HaveGPU)
	}
}

// The dangerous direction: enabling GPU monitoring by reload must start
// watching the card at once, not leave it unwatched until the next restart.
func TestReloadEnablingGPUWatchesCard(t *testing.T) {
	rr := &gpuRunner{cpuTemps: "Temp | 0Eh | ok | 3.1 | 40 degrees C", gpuOut: "72\n"}
	c := newGPUController(rr)
	c.setTunables(testTunables()) // started with gpu.enabled: false
	if err := c.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s := c.snapshot(); s.Percent != 10 {
		t.Fatalf("precondition: CPU-only at 40°C should command 10%%; got %d%%", s.Percent)
	}

	c.setTunables(gpuTunables(rr)) // reload with gpu.enabled: true
	if err := c.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s := c.snapshot(); s.Percent != 39 || !s.HaveGPU {
		t.Fatalf("after enabling GPU: Percent=%d HaveGPU=%v, want 39%% driven by the 72°C GPU", s.Percent, s.HaveGPU)
	}
}

func TestTunablesFromBuildsGPUReaderOnlyWhenEnabled(t *testing.T) {
	cfg := config.Default("")
	cfg.GPU.Enabled = false
	if tunablesFrom(cfg).gpu != nil {
		t.Fatal("gpu.enabled: false must leave the GPU reader nil")
	}
	cfg.GPU.Enabled = true
	if tunablesFrom(cfg).gpu == nil {
		t.Fatal("gpu.enabled: true must build a GPU reader")
	}
}
