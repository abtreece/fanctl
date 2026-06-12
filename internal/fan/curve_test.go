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

func gov() Governor { return Governor{Deadband: 3, MaxStepDown: 10} }

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
