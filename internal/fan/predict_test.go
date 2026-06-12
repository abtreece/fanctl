package fan

import (
	"math"
	"testing"
	"time"
)

func at(sec int) time.Time {
	return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(sec) * time.Second)
}

func TestSlopeNeedsHistory(t *testing.T) {
	var p Predictor
	if got := p.SlopePerMin(); got != 0 {
		t.Fatalf("empty slope = %v, want 0", got)
	}
	p.Observe(at(0), 50)
	if got := p.SlopePerMin(); got != 0 {
		t.Fatalf("single-sample slope = %v, want 0", got)
	}
	p.Observe(at(5), 60)
	if got := p.SlopePerMin(); got != 0 {
		t.Fatalf("short-span slope = %v, want 0 (span < min)", got)
	}
}

func TestSlopeAndBoost(t *testing.T) {
	var p Predictor
	// 60 -> 66 over 60s = 6°C/min.
	p.Observe(at(0), 60)
	p.Observe(at(30), 63)
	p.Observe(at(60), 66)
	if got := p.SlopePerMin(); math.Abs(got-6) > 0.01 {
		t.Fatalf("slope = %v, want 6", got)
	}
	// Boost = slope * lookahead, capped.
	if got := p.Boost(1.0); math.Abs(got-6) > 0.01 {
		t.Fatalf("boost(1.0) = %v, want 6", got)
	}
	if got := p.Boost(2.0); got != boostCap {
		t.Fatalf("boost(2.0) = %v, want cap %v", got, boostCap)
	}
	if got := p.Boost(0); got != 0 {
		t.Fatalf("boost(0) = %v, want 0 (disabled)", got)
	}
}

func TestBoostIgnoresFallingTemps(t *testing.T) {
	var p Predictor
	p.Observe(at(0), 70)
	p.Observe(at(60), 60)
	if got := p.Boost(1.5); got != 0 {
		t.Fatalf("falling boost = %v, want 0", got)
	}
}

func TestObserveDropsOldSamples(t *testing.T) {
	var p Predictor
	p.Observe(at(0), 90) // will age out of the window
	p.Observe(at(120), 50)
	p.Observe(at(180), 53)
	// Slope must be computed from the recent rise, not the stale spike.
	if got := p.SlopePerMin(); math.Abs(got-3) > 0.01 {
		t.Fatalf("slope after expiry = %v, want 3", got)
	}
}

func TestReset(t *testing.T) {
	var p Predictor
	p.Observe(at(0), 50)
	p.Observe(at(60), 70)
	p.Reset()
	if got := p.SlopePerMin(); got != 0 {
		t.Fatalf("slope after reset = %v, want 0", got)
	}
}
