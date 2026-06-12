package fan

import "testing"

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

func TestLevelFreshSelection(t *testing.T) {
	c := testCurve()
	tests := []struct {
		temp int
		want int
	}{
		{20, 0}, {45, 0}, {46, 1}, {55, 1}, {65, 2}, {72, 3}, {80, AutoLevel},
	}
	for _, tt := range tests {
		if got := c.Level(Initial(), tt.temp); got != tt.want {
			t.Errorf("Level(Initial(), %d) = %d, want %d", tt.temp, got, tt.want)
		}
	}
}

func TestLevelRaisesImmediately(t *testing.T) {
	c := testCurve()
	// From band 0, a jump straight to band 3 should not be rate-limited.
	if got := c.Level(0, 70); got != 3 {
		t.Errorf("Level(0, 70) = %d, want 3 (immediate raise)", got)
	}
	// Above the top band hands off to auto immediately.
	if got := c.Level(0, 90); got != AutoLevel {
		t.Errorf("Level(0, 90) = %d, want AutoLevel", got)
	}
}

func TestLevelLowersWithHysteresis(t *testing.T) {
	c := testCurve()
	// At band 1 (ceiling 55), the band below has ceiling 45; hysteresis 4 means
	// we only drop to band 0 once temp < 41.
	if got := c.Level(1, 44); got != 1 {
		t.Errorf("Level(1, 44) = %d, want 1 (within hysteresis, hold)", got)
	}
	if got := c.Level(1, 41); got != 1 {
		t.Errorf("Level(1, 41) = %d, want 1 (at boundary, hold)", got)
	}
	if got := c.Level(1, 40); got != 0 {
		t.Errorf("Level(1, 40) = %d, want 0 (past hysteresis, drop)", got)
	}
}

func TestLevelLowersOneStepAtATime(t *testing.T) {
	c := testCurve()
	// From band 3 with a low temp, we should step down one band per call, not
	// jump straight to band 0.
	if got := c.Level(3, 20); got != 2 {
		t.Errorf("Level(3, 20) = %d, want 2 (single step down)", got)
	}
}

func TestLevelLeavesAutoWithHysteresis(t *testing.T) {
	c := testCurve()
	// Top band ceiling is 72, hysteresis 4 -> stay in auto until temp <= 68.
	if got := c.Level(AutoLevel, 70); got != AutoLevel {
		t.Errorf("Level(AutoLevel, 70) = %d, want AutoLevel (still hot)", got)
	}
	if got := c.Level(AutoLevel, 68); got != 3 {
		t.Errorf("Level(AutoLevel, 68) = %d, want 3 (reclaim manual)", got)
	}
}

func TestPercent(t *testing.T) {
	c := testCurve()
	if p, ok := c.Percent(2); !ok || p != 30 {
		t.Errorf("Percent(2) = %d, %v; want 30, true", p, ok)
	}
	if _, ok := c.Percent(AutoLevel); ok {
		t.Errorf("Percent(AutoLevel) ok = true; want false")
	}
}
