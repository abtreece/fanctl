package main

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abtreece/fanctl/internal/ipmi"
)

const setAutoRaw = "0x30 0x30 0x01 0x01"

// controllerWithRunner builds a controller around an arbitrary ipmi.Runner.
func controllerWithRunner(run ipmi.Runner) *controller {
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := &controller{
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		client: ipmi.New(ipmi.Options{Bin: "ipmitool"}, run),
		now:    func() time.Time { return clock },
	}
	c.setTunables(testTunables())
	return c
}

// wedgedRunner simulates an unresponsive BMC: SDR reads block until their
// context is cancelled, the way a hung ipmitool does. Raw commands still
// succeed so the fallback path can be observed.
type wedgedRunner struct {
	mu   sync.Mutex
	raws []string
}

func (r *wedgedRunner) run(ctx context.Context, _ string, args ...string) ([]byte, error) {
	if len(args) > 0 && args[0] == "raw" {
		r.mu.Lock()
		r.raws = append(r.raws, strings.Join(args[1:], " "))
		r.mu.Unlock()
		return nil, nil
	}
	<-ctx.Done() // wedge until the deadline fires
	return nil, ctx.Err()
}

func (r *wedgedRunner) rawCalls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.raws...)
}

// A hung ipmitool must not stall the control loop: the step has to time out and
// hand the fans back to the BMC, because a stalled loop leaves them pinned.
func TestRunStepTimesOutAndFallsBackToAuto(t *testing.T) {
	rr := &wedgedRunner{}
	c := controllerWithRunner(rr.run)

	start := time.Now()
	c.runStep(context.Background(), 50*time.Millisecond)
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("runStep blocked for %s against a wedged BMC; it must respect its deadline", elapsed)
	}
	raws := rr.rawCalls()
	if len(raws) != 1 || raws[0] != setAutoRaw {
		t.Fatalf("after a timed-out step, want exactly one set-auto (%q), got %v", setAutoRaw, raws)
	}
}

// The recovery path must be bounded too: it typically runs precisely because
// the BMC is unresponsive, so it cannot inherit an unbounded wait.
func TestRunStepReturnsWhenEvenFallbackHangs(t *testing.T) {
	run := func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	c := controllerWithRunner(run)

	done := make(chan struct{})
	go func() {
		c.runStep(context.Background(), 50*time.Millisecond)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runStep never returned with a fully unresponsive BMC")
	}
}

// A step error still counts toward the metrics counters, so a BMC that has gone
// away is visible to alerting rather than silently retried forever.
func TestRunStepRecordsErrors(t *testing.T) {
	rr := &wedgedRunner{}
	c := controllerWithRunner(rr.run)
	c.runStep(context.Background(), 50*time.Millisecond)

	if got := c.snapshot().StepErrors; got != 1 {
		t.Fatalf("StepErrors = %d, want 1", got)
	}
}

// A healthy step must not pay the timeout cost or touch the fallback path.
func TestRunStepFastPathUnaffected(t *testing.T) {
	rr := &recordingRunner{temps: "Temp | 0Eh | ok | 3.1 | 40 degrees C"}
	c := controllerWithRunner(rr.run)

	c.runStep(context.Background(), 5*time.Second)

	// Band 0 at 40°C: set-manual + set-percent, and no set-auto fallback.
	if rr.raws != 2 {
		t.Fatalf("healthy step issued %d raw commands, want 2", rr.raws)
	}
	if got := c.snapshot().StepErrors; got != 0 {
		t.Fatalf("healthy step recorded %d errors, want 0", got)
	}
}
