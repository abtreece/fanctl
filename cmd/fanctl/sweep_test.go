package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/abtreece/fanctl/internal/config"
	"github.com/abtreece/fanctl/internal/gpu"
	"github.com/abtreece/fanctl/internal/ipmi"
)

// gpuStub reports a GPU temperature as a function of the commanded duty, or an
// error to exercise the fail-safe path.
type gpuStub struct {
	tempFor   func(duty int) int
	err       error
	failAfter int // once >0, reads past this many calls return err
	calls     int
	duty      *int // points at the controller's current duty
}

func (g *gpuStub) Temperatures(context.Context) ([]gpu.Temp, error) {
	g.calls++
	if g.err != nil && g.calls > g.failAfter {
		return nil, g.err
	}
	return []gpu.Temp{{Index: 0, Celsius: g.tempFor(*g.duty)}}, nil
}

// sweepStub returns a fan set chosen by the last commanded duty, so a test can
// describe a host's duty->RPM behaviour as a table.
type sweepStub struct {
	rpmFor  func(duty int) []int // duty -> per-fan RPM
	tempFor func(duty int) int   // duty -> selected sensor °C
	duty    int
	autoSet bool
	manual  int
	setPcts []int
}

func (s *sweepStub) Fans(context.Context) ([]ipmi.Fan, error) {
	var out []ipmi.Fan
	for i, r := range s.rpmFor(s.duty) {
		out = append(out, ipmi.Fan{ID: "3" + string(rune('0'+i)) + "h", Name: "Fan" + string(rune('1'+i)), RPM: r})
	}
	return out, nil
}

func (s *sweepStub) Temperatures(context.Context) ([]ipmi.Temp, error) {
	t := 40
	if s.tempFor != nil {
		t = s.tempFor(s.duty)
	}
	return []ipmi.Temp{
		{ID: "04h", Name: "Inlet Temp", Celsius: 24},
		{ID: "0Eh", Name: "Temp", Celsius: t},
		{ID: "0Fh", Name: "Temp", Celsius: t - 3},
	}, nil
}

func (s *sweepStub) SetManual(context.Context) error { s.manual++; return nil }
func (s *sweepStub) SetPercent(_ context.Context, p int) error {
	s.duty = p
	s.setPcts = append(s.setPcts, p)
	return nil
}
func (s *sweepStub) SetAuto(context.Context) error { s.autoSet = true; return nil }

func testSensors() config.SensorConfig {
	return config.SensorConfig{NameMatch: []string{"Temp"}, NameExclude: []string{"Inlet", "Exhaust"}}
}

func baseOptions(duties []int) sweepOptions {
	return sweepOptions{
		duties:     duties,
		settle:     time.Millisecond,
		maxTemp:    60,
		maxGPUTemp: 75,
		minRPM:     900,
		restore:    true,
		format:     "table",
		sensors:    testSensors(),
	}
}

// razorRPM reproduces the measured R420 response: RPM tracks duty down to 0
// with no clamp, but below 6% some fans bottom out while others keep tracking.
func razorRPM(duty int) []int {
	switch {
	case duty >= 10:
		return []int{3360, 2880, 3120, 2880, 3120, 2880}
	case duty >= 8:
		return []int{3000, 2640, 2760, 2640, 2760, 2640}
	case duty >= 6:
		return []int{2520, 2280, 2400, 2280, 2400, 2400}
	case duty >= 4:
		return []int{2280, 1560, 2160, 1560, 2280, 2160}
	case duty >= 2:
		return []int{1920, 1560, 1800, 1560, 1920, 1800}
	default:
		return []int{1680, 1440, 1560, 1440, 1680, 1560}
	}
}

func TestSweepVisitsEveryStepAndRestores(t *testing.T) {
	st := &sweepStub{rpmFor: razorRPM}
	var out bytes.Buffer
	code := sweep(&out, &out, st, baseOptions([]int{10, 8, 6}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, out.String())
	}
	if got, want := len(st.setPcts), 3; got != want {
		t.Errorf("SetPercent calls = %d, want %d", got, want)
	}
	if st.manual == 0 {
		t.Error("SetManual was never called")
	}
	if !st.autoSet {
		t.Error("BMC automatic control was not restored")
	}
	// Baseline plus one row per step.
	for _, want := range []string{"baseline", "10%", "8%", "6%"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing row %q\n%s", want, out.String())
		}
	}
}

func TestSweepRestoresAutoAfterAbort(t *testing.T) {
	st := &sweepStub{
		rpmFor:  razorRPM,
		tempFor: func(duty int) int { return 70 - duty }, // hot enough to trip at low duty
	}
	var out bytes.Buffer
	opt := baseOptions([]int{10, 8, 6, 4, 2, 0})
	opt.maxTemp = 64
	code := sweep(&out, &out, st, opt)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 on abort\n%s", code, out.String())
	}
	if !st.autoSet {
		t.Error("BMC automatic control must be restored even when the sweep aborts")
	}
	if !strings.Contains(out.String(), "ABORT") {
		t.Errorf("output should report the abort\n%s", out.String())
	}
	// It must stop at the offending step rather than pressing on.
	if last := st.setPcts[len(st.setPcts)-1]; last == 0 {
		t.Errorf("sweep continued to %d%% after aborting", last)
	}
}

func TestSweepAbortsOnLowRPM(t *testing.T) {
	st := &sweepStub{rpmFor: func(int) []int { return []int{800, 900, 850} }}
	var out bytes.Buffer
	code := sweep(&out, &out, st, baseOptions([]int{10}))
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(out.String(), "ABORT") {
		t.Errorf("expected an RPM abort\n%s", out.String())
	}
}

func TestSweepSkipsRestoreWhenDisabled(t *testing.T) {
	st := &sweepStub{rpmFor: razorRPM}
	var out bytes.Buffer
	opt := baseOptions([]int{10})
	opt.restore = false
	if code := sweep(&out, &out, st, opt); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if st.autoSet {
		t.Error("SetAuto called despite -restore-auto=false")
	}
}

// --- GPU hosts --------------------------------------------------------------

// r430RPM reproduces the measured R430 response: ~150 RPM per duty point.
func r430RPM(duty int) []int {
	base := 1608 + duty*150
	return []int{base + 120, base - 120, base, base - 60, base + 60, base}
}

func TestSweepReportsGPUTemperature(t *testing.T) {
	st := &sweepStub{rpmFor: r430RPM}
	// Measured R430 idle: 45°C at 10%, 49°C at 6%.
	gs := &gpuStub{duty: &st.duty, tempFor: func(duty int) int { return 61 - duty*2 + duty/5 }}
	var out bytes.Buffer
	opt := baseOptions([]int{10, 8, 6})
	opt.gpu = gs
	if code := sweep(&out, &out, st, opt); code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "GPU") {
		t.Errorf("table should carry a GPU column on a GPU host\n%s", out.String())
	}
}

// The daemon's invariant is that GPU-enabled plus an unreadable GPU means BMC
// auto, never CPU-only cooling. The sweep must not press on either.
func TestSweepAbortsAndRestoresWhenGPUUnreadable(t *testing.T) {
	st := &sweepStub{rpmFor: r430RPM}
	// Baseline and the first two steps read fine; the GPU goes away at 6%.
	gs := &gpuStub{
		duty:      &st.duty,
		tempFor:   func(int) int { return 50 },
		err:       errors.New("nvidia-smi: no devices"),
		failAfter: 3,
	}
	var out bytes.Buffer
	opt := baseOptions([]int{10, 8, 6, 4})
	opt.gpu = gs
	if code := sweep(&out, &out, st, opt); code == 0 {
		t.Fatalf("sweep must fail when the GPU is unreadable\n%s", out.String())
	}
	if !st.autoSet {
		t.Error("BMC automatic control must be restored when the GPU read fails")
	}
	if last := st.setPcts[len(st.setPcts)-1]; last != 6 {
		t.Errorf("sweep continued past the failed GPU read to %d%%: %v", last, st.setPcts)
	}
}

func TestSweepAbortsOnHotGPUWhileCPUIsCool(t *testing.T) {
	st := &sweepStub{rpmFor: r430RPM}
	// CPU stays cool throughout; only the GPU crosses the limit.
	gs := &gpuStub{duty: &st.duty, tempFor: func(duty int) int {
		if duty <= 6 {
			return 80
		}
		return 50
	}}
	var out bytes.Buffer
	opt := baseOptions([]int{10, 8, 6, 4})
	opt.gpu = gs
	if code := sweep(&out, &out, st, opt); code != 1 {
		t.Fatalf("exit code = %d, want 1\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "GPU reached") {
		t.Errorf("abort should name the GPU as the cause\n%s", out.String())
	}
	if last := st.setPcts[len(st.setPcts)-1]; last != 6 {
		t.Errorf("sweep stopped at %d%%, want 6%% (the first GPU breach)", last)
	}
}

func TestSuggestionOnGPUHostWarnsAboutBothCurveFloors(t *testing.T) {
	samples := []sweepSample{
		{Duty: 10, AvgRPM: 3120, MinRPM: 3000, MaxRPM: 3240, TempC: 38, GPUC: 45, HaveGPU: true},
		{Duty: 6, AvgRPM: 2520, MinRPM: 2400, MaxRPM: 2640, TempC: 41, GPUC: 49, HaveGPU: true},
		{Duty: 4, AvgRPM: 2208, MinRPM: 1400, MaxRPM: 2900, TempC: 42, GPUC: 51, HaveGPU: true},
	}
	var out bytes.Buffer
	renderSuggestion(&out, samples)
	got := out.String()
	for _, want := range []string{"6%", "gpu.curve", "BOTH", "49°C GPU"} {
		if !strings.Contains(got, want) {
			t.Errorf("GPU-host suggestion missing %q\n%s", want, got)
		}
	}
	// The generic "cannot see passively-cooled cards" caveat is wrong here --
	// this sweep could see the card.
	if strings.Contains(got, "cannot see passively-cooled") {
		t.Errorf("GPU-host suggestion should not claim GPU blindness\n%s", got)
	}
}

// --- analysis ---------------------------------------------------------------

func sample(duty, avg, min, max int) sweepSample {
	return sweepSample{Duty: duty, AvgRPM: avg, MinRPM: min, MaxRPM: max, TempC: 43}
}

func TestAnalyseStopsWhereFansStopTracking(t *testing.T) {
	// The measured R420 numbers: 4% splits 1560-2280, so 6% is the floor.
	samples := []sweepSample{
		{Duty: -1, AvgRPM: 3040, MinRPM: 2880, MaxRPM: 3360},
		sample(10, 3050, 2880, 3360),
		sample(8, 2740, 2640, 3000),
		sample(6, 2420, 2280, 2520),
		sample(4, 2030, 1560, 2280),
		sample(2, 1820, 1560, 1920),
		sample(0, 1560, 1440, 1680),
	}
	best, reason, ok := analyseSweep(samples)
	if !ok {
		t.Fatal("expected a usable step")
	}
	if best.Duty != 6 {
		t.Errorf("suggested floor = %d%%, want 6%%", best.Duty)
	}
	if !strings.Contains(reason, "4%") || !strings.Contains(reason, "tracking") {
		t.Errorf("reason = %q, want it to name the 4%% tracking failure", reason)
	}
}

// Once fans bottom out, lower steps look tidy again on their own numbers. The
// walk must not skip past the breakdown and recommend one of them.
func TestAnalyseDoesNotRecoverBelowBreakdown(t *testing.T) {
	samples := []sweepSample{
		sample(10, 3000, 2900, 3100),
		sample(6, 2000, 1200, 2800), // spread blows out here
		sample(2, 1000, 980, 1020),  // tidy, but control is already lost
	}
	best, _, ok := analyseSweep(samples)
	if !ok {
		t.Fatal("expected a usable step")
	}
	if best.Duty != 10 {
		t.Errorf("suggested floor = %d%%, want 10%% (must not skip past the breakdown)", best.Duty)
	}
}

func TestAnalyseDetectsFirmwareClamp(t *testing.T) {
	samples := []sweepSample{
		sample(30, 4000, 3900, 4100),
		sample(20, 3000, 2900, 3100),
		sample(10, 2950, 2900, 3000), // RPM stopped responding
		sample(0, 2940, 2900, 2980),
	}
	best, reason, ok := analyseSweep(samples)
	if !ok {
		t.Fatal("expected a usable step")
	}
	if best.Duty != 20 {
		t.Errorf("suggested floor = %d%%, want 20%%", best.Duty)
	}
	if !strings.Contains(reason, "clamping") {
		t.Errorf("reason = %q, want it to report a firmware clamp", reason)
	}
}

func TestAnalyseReportsNothingUsableWhenFirstStepFails(t *testing.T) {
	samples := []sweepSample{
		{Duty: 10, AvgRPM: 2000, MinRPM: 500, MaxRPM: 3500},
	}
	if _, _, ok := analyseSweep(samples); ok {
		t.Error("a first step that already fails must not be suggested as a floor")
	}
}

func TestSuggestionWarnsAboutEquilibriumAndHiddenCards(t *testing.T) {
	samples := []sweepSample{
		sample(10, 3050, 2880, 3360),
		sample(6, 2420, 2280, 2520),
		sample(4, 2030, 1560, 2280),
	}
	var out bytes.Buffer
	renderSuggestion(&out, samples)
	got := out.String()
	for _, want := range []string{"6%", "EQUILIBRIUM", "passively-cooled"} {
		if !strings.Contains(got, want) {
			t.Errorf("suggestion missing %q\n%s", want, got)
		}
	}
}

// --- flags ------------------------------------------------------------------

func TestParseStepsRejectsNonDescending(t *testing.T) {
	for _, in := range []string{"10,20", "10,10", "5,4,6"} {
		if _, err := parseSteps(in); err == nil {
			t.Errorf("parseSteps(%q) should fail: steps must descend", in)
		}
	}
}

func TestParseStepsRejectsOutOfRangeAndEmpty(t *testing.T) {
	for _, in := range []string{"101", "-1", "", "abc"} {
		if _, err := parseSteps(in); err == nil {
			t.Errorf("parseSteps(%q) should fail", in)
		}
	}
}

func TestParseStepsAcceptsDescendingList(t *testing.T) {
	got, err := parseSteps(" 10, 8 ,6,0 ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []int{10, 8, 6, 0}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestSpreadPct(t *testing.T) {
	if got := sample(6, 2420, 2280, 2520).spreadPct(); got != 9 {
		t.Errorf("spreadPct = %d, want 9", got)
	}
	if got := (sweepSample{}).spreadPct(); got != 0 {
		t.Errorf("spreadPct on empty sample = %d, want 0", got)
	}
}
