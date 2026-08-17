package fan

import (
	"math"
	"testing"
)

func testCurve() Curve {
	return Curve{
		Bands: []Band{
			{MaxTemp: 45, Percent: 10},
			{MaxTemp: 55, Percent: 20},
			{MaxTemp: 65, Percent: 30},
			{MaxTemp: 72, Percent: 45},
		},
		Hysteresis: 4,
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		curve   Curve
		wantErr bool
	}{
		{"good", testCurve(), false},
		{"empty", Curve{}, true},
		{"non-ascending", Curve{Bands: []Band{{45, 10}, {45, 20}}}, true},
		{"percent too high", Curve{Bands: []Band{{45, 101}}}, true},
		{"negative hysteresis", Curve{Bands: []Band{{45, 10}}, Hysteresis: -1}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.curve.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestDutyInterpolates(t *testing.T) {
	c := testCurve()
	tests := []struct {
		temp     float64
		want     float64
		wantAuto bool
	}{
		{20, 10, false},     // below first anchor -> floor
		{45, 10, false},     // at first anchor
		{50, 15, false},     // midway 45..55 -> midway 10..20
		{55, 20, false},     // at anchor
		{60, 25, false},     // midway 55..65
		{68.5, 37.5, false}, // halfway 65..72 -> halfway 30..45
		{72, 45, false},     // top anchor still manual
		{72.1, 0, true},     // above top anchor -> auto
	}
	for _, tt := range tests {
		pct, auto := c.Duty(tt.temp)
		if auto != tt.wantAuto || math.Abs(pct-tt.want) > 0.01 {
			t.Errorf("Duty(%.1f) = %.2f, auto=%v; want %.2f, auto=%v", tt.temp, pct, auto, tt.want, tt.wantAuto)
		}
	}
}

func TestReclaimBelow(t *testing.T) {
	if got := testCurve().ReclaimBelow(); got != 68 {
		t.Fatalf("ReclaimBelow() = %v, want 68 (72 - hysteresis 4)", got)
	}
}

// gov mirrors the governor config.Governor() builds for the daemon.
func gov() Governor { return Governor{Deadband: 3, MaxStepDown: 10, DeadbandHoldPolls: 3} }

func TestGovernorSeedsFirstDecision(t *testing.T) {
	s := gov().Next(State{}, 37.5, false, true)
	if !s.Init || s.Auto || s.Pct != 38 {
		t.Fatalf("first decision = %+v; want manual 38, init", s)
	}
	s = gov().Next(State{}, 0, true, false)
	if !s.Init || !s.Auto {
		t.Fatalf("first decision over-temp = %+v; want auto", s)
	}
}

func TestGovernorRaisesImmediately(t *testing.T) {
	prev := State{Pct: 20, Init: true}
	if s := gov().Next(prev, 70, false, true); s.Pct != 70 {
		t.Fatalf("raise 20->70 = %+v; want immediate 70", s)
	}
	// Even a 1-point raise is not deadband-gated.
	if s := gov().Next(prev, 21, false, true); s.Pct != 21 {
		t.Fatalf("raise 20->21 = %+v; want 21", s)
	}
}

func TestGovernorDeadbandHoldsSmallDrops(t *testing.T) {
	prev := State{Pct: 40, Init: true}
	if s := gov().Next(prev, 38, false, true); s.Pct != 40 {
		t.Fatalf("drop within deadband = %+v; want hold at 40", s)
	}
	if s := gov().Next(prev, 37, false, true); s.Pct != 37 {
		t.Fatalf("drop past deadband = %+v; want 37", s)
	}
}

// The regression this exists for: below the curve's lowest anchor the curve is
// flat, so the target is pinned at the floor and the gap to a duty sitting a
// point or two above it can never grow to Deadband. Holding forever stranded a
// tuned 6% floor at 7%, giving back much of the noise it was tuned for.
func TestGovernorReachesFloorAfterSustainedHold(t *testing.T) {
	g := gov()
	s := State{Pct: 7, Init: true}
	const floor = 6.0

	for i := 0; i < g.DeadbandHoldPolls-1; i++ {
		s = g.Next(s, floor, false, true)
		if s.Pct != 7 {
			t.Fatalf("poll %d: pct = %d, want the decrease still held at 7", i+1, s.Pct)
		}
	}
	s = g.Next(s, floor, false, true)
	if s.Pct != 6 {
		t.Fatalf("after %d holds: pct = %d, want the floor at 6", g.DeadbandHoldPolls, s.Pct)
	}
	// And it stays there rather than oscillating.
	if s = g.Next(s, floor, false, true); s.Pct != 6 {
		t.Fatalf("once on the floor: pct = %d, want a stable 6", s.Pct)
	}
	if s.Hold != 0 {
		t.Errorf("hold = %d after settling, want it cleared", s.Hold)
	}
}

// A target genuinely wobbling across a band boundary must still be absorbed:
// the hold only matures when the same decrease persists.
func TestGovernorHoldResetsWhenTargetOscillates(t *testing.T) {
	g := gov()
	s := State{Pct: 7, Init: true}
	for i := 0; i < 10; i++ {
		target := 6.0
		if i%2 == 1 {
			target = 7.0
		}
		s = g.Next(s, target, false, true)
		if s.Pct != 7 {
			t.Fatalf("poll %d: pct = %d, want 7 held against an oscillating target", i+1, s.Pct)
		}
	}
}

func TestGovernorZeroHoldPollsHoldsForever(t *testing.T) {
	g := Governor{Deadband: 3, MaxStepDown: 10} // DeadbandHoldPolls unset
	s := State{Pct: 7, Init: true}
	for i := 0; i < 20; i++ {
		s = g.Next(s, 6, false, true)
	}
	if s.Pct != 7 {
		t.Fatalf("pct = %d; with DeadbandHoldPolls unset the decrease must never apply", s.Pct)
	}
}

// Hold is bookkeeping. Callers deciding whether to write to the BMC must not
// see a held poll as a change, or the deadband produces the churn it prevents.
func TestStateSameCommandIgnoresHold(t *testing.T) {
	a := State{Pct: 7, Init: true}
	b := State{Pct: 7, Init: true, Hold: 2}
	if a == b {
		t.Fatal("test is meaningless if the structs are already equal")
	}
	if !a.SameCommand(b) {
		t.Error("states differing only in Hold must command the same thing")
	}
	if a.SameCommand(State{Pct: 8, Init: true}) {
		t.Error("a different duty is a different command")
	}
	if a.SameCommand(State{Auto: true, Init: true}) {
		t.Error("auto is a different command")
	}
}

// A raise must clear the hold, so a later decrease starts its count fresh
// rather than inheriting credit from before the temperature rose.
func TestGovernorRaiseClearsHold(t *testing.T) {
	g := gov()
	s := g.Next(State{Pct: 7, Init: true}, 6, false, true)
	if s.Hold != 1 {
		t.Fatalf("hold = %d, want 1 after one held decrease", s.Hold)
	}
	s = g.Next(s, 12, false, true)
	if s.Pct != 12 || s.Hold != 0 {
		t.Fatalf("after raise = %+v, want pct 12 and hold cleared", s)
	}
}

func TestGovernorRateLimitsStepDown(t *testing.T) {
	prev := State{Pct: 100, Init: true}
	if s := gov().Next(prev, 20, false, true); s.Pct != 90 {
		t.Fatalf("large drop = %+v; want rate-limited to 90", s)
	}
}

func TestGovernorAutoHandoffAndReclaim(t *testing.T) {
	prev := State{Pct: 70, Init: true}
	s := gov().Next(prev, 0, true, false)
	if !s.Auto {
		t.Fatalf("over-temp = %+v; want auto", s)
	}
	// Still hot: stay auto even though a duty target exists.
	s2 := gov().Next(s, 100, false, false)
	if !s2.Auto {
		t.Fatalf("auto without reclaim margin = %+v; want stay auto", s2)
	}
	// Cooled past hysteresis: reclaim manual at the target duty.
	s3 := gov().Next(s2, 45, false, true)
	if s3.Auto || s3.Pct != 45 {
		t.Fatalf("reclaim = %+v; want manual 45", s3)
	}
}
