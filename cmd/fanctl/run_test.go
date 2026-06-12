package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

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

// recordingRunner serves canned temperature/fan SDR output and counts raw set
// commands.
type recordingRunner struct {
	temps string
	fans  string
	raws  int
}

func (r *recordingRunner) run(_ context.Context, _ string, args ...string) ([]byte, error) {
	if len(args) >= 3 && args[0] == "sdr" && args[2] == "fan" {
		return []byte(r.fans), nil
	}
	if len(args) >= 2 && args[0] == "sdr" {
		return []byte(r.temps), nil
	}
	if len(args) >= 1 && args[0] == "raw" {
		r.raws++
	}
	return nil, nil
}

func testTunables() tunables {
	curve := fan.Curve{Bands: []fan.Band{
		{MaxTemp: 50, Percent: 10},
		{MaxTemp: 60, Percent: 20},
		{MaxTemp: 68, Percent: 30},
		{MaxTemp: 75, Percent: 45},
	}, Hysteresis: 4}
	return tunables{
		curve:    curve,
		gpuCurve: curve,
		gov:      fan.Governor{Deadband: 3, MaxStepDown: 10},
		sensors:  config.SensorConfig{NameMatch: []string{"Temp"}, NameExclude: []string{"Inlet"}},
		pollIdle: 30 * time.Second,
	}
}

func newTestController(rr *recordingRunner) *controller {
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := &controller{
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		client: ipmi.New(ipmi.Options{Bin: "ipmitool"}, rr.run),
		now:    func() time.Time { return clock },
	}
	c.setTunables(testTunables())
	return c
}

// advance moves the controller's fake clock forward.
func advance(c *controller, base time.Time, d time.Duration) time.Time {
	at := base.Add(d)
	c.now = func() time.Time { return at }
	return at
}

func TestControllerOnlyActsOnChange(t *testing.T) {
	rr := &recordingRunner{temps: "Temp | 0Eh | ok | 3.1 | 40 degrees C"}
	c := newTestController(rr)

	// First step: 40°C -> 10%. Expect manual + setpercent = 2 raws.
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

func TestControllerInterpolatesDuty(t *testing.T) {
	// 55°C is midway 50..60 -> midway 10..20 = 15%.
	rr := &recordingRunner{temps: "Temp | 0Eh | ok | 3.1 | 55 degrees C"}
	c := newTestController(rr)
	if err := c.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s := c.snapshot(); s.Percent != 15 {
		t.Fatalf("Percent = %d, want interpolated 15", s.Percent)
	}
}

func TestControllerOverTempHandsToAuto(t *testing.T) {
	rr := &recordingRunner{temps: "Temp | 0Eh | ok | 3.1 | 80 degrees C"}
	c := newTestController(rr)
	if err := c.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s := c.snapshot(); !s.BMCAuto {
		t.Fatalf("snapshot = %+v, want BMC auto above top anchor", s)
	}
}

func TestSetTunablesAppliesNewCurve(t *testing.T) {
	rr := &recordingRunner{temps: "Temp | 0Eh | ok | 3.1 | 40 degrees C"}
	c := newTestController(rr)
	if err := c.step(context.Background()); err != nil { // 40°C -> 10%
		t.Fatal(err)
	}
	before := rr.raws
	// Reload to a curve where 40°C lands much hotter, then step again.
	tun := testTunables()
	tun.curve = fan.Curve{Bands: []fan.Band{{MaxTemp: 30, Percent: 25}, {MaxTemp: 80, Percent: 60}}, Hysteresis: 4}
	tun.gpuCurve = tun.curve
	c.setTunables(tun)
	if err := c.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rr.raws <= before {
		t.Fatalf("expected new commands after curve reload; raws before=%d after=%d", before, rr.raws)
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

func TestControllerPredictiveBoost(t *testing.T) {
	rr := &recordingRunner{temps: "Temp | 0Eh | ok | 3.1 | 55 degrees C"}
	c := newTestController(rr)
	tun := testTunables()
	tun.lookahead = 1.0
	c.setTunables(tun)
	base := c.now()

	if err := c.step(context.Background()); err != nil { // 55°C steady -> 15%
		t.Fatal(err)
	}
	// 55 -> 61 over 60s = 6°C/min; with 1 min lookahead the curve is evaluated
	// at ~67°C (boost capped at 8), well above a steady 61°C reading.
	advance(c, base, 30*time.Second)
	rr.temps = "Temp | 0Eh | ok | 3.1 | 58 degrees C"
	if err := c.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	advance(c, base, 60*time.Second)
	rr.temps = "Temp | 0Eh | ok | 3.1 | 61 degrees C"
	if err := c.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := c.snapshot()
	steady, _ := tun.curve.Duty(61)
	if float64(s.Percent) <= steady {
		t.Fatalf("Percent = %d, want above steady-state %.1f (predictive boost)", s.Percent, steady)
	}
	if s.SlopeCPM < 5 {
		t.Fatalf("SlopeCPM = %.2f, want ~6", s.SlopeCPM)
	}
}

func TestControllerReassertsManualPeriodically(t *testing.T) {
	rr := &recordingRunner{temps: "Temp | 0Eh | ok | 3.1 | 40 degrees C"}
	c := newTestController(rr)
	tun := testTunables()
	tun.reassert = time.Minute
	c.setTunables(tun)
	base := c.now()

	if err := c.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	after := rr.raws
	// Within the interval: no commands.
	advance(c, base, 30*time.Second)
	if err := c.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rr.raws != after {
		t.Fatalf("re-asserted early; raws = %d, want %d", rr.raws, after)
	}
	// Past the interval: manual control re-issued without a duty change.
	advance(c, base, 90*time.Second)
	if err := c.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rr.raws != after+2 {
		t.Fatalf("expected re-assert (2 raws); raws = %d, want %d", rr.raws, after+2)
	}
	if s := c.snapshot(); s.Reasserts != 1 {
		t.Fatalf("Reasserts = %d, want 1", s.Reasserts)
	}
}

func TestControllerVerifyFailureReasserts(t *testing.T) {
	fans := "Fan1 | 30h | ok | 7.1 | 5000 RPM\n"
	rr := &recordingRunner{temps: "Temp | 0Eh | ok | 3.1 | 40 degrees C", fans: fans}
	c := newTestController(rr)
	base := c.now()

	if err := c.step(context.Background()); err != nil { // 40°C -> 10%
		t.Fatal(err)
	}
	// Big jump: 73°C -> ~41%; change >= 15 points arms verification.
	advance(c, base, 30*time.Second)
	rr.temps = "Temp | 0Eh | ok | 3.1 | 73 degrees C"
	if err := c.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	after := rr.raws
	// Next poll: RPM unchanged -> verification fails -> re-assert issued even
	// though the duty is unchanged.
	advance(c, base, 60*time.Second)
	if err := c.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rr.raws != after+2 {
		t.Fatalf("expected verify-failure re-assert; raws = %d, want %d", rr.raws, after+2)
	}
	if s := c.snapshot(); s.VerifyFails != 1 {
		t.Fatalf("VerifyFails = %d, want 1", s.VerifyFails)
	}
}

func TestDesiredIntervalAdaptive(t *testing.T) {
	rr := &recordingRunner{temps: "Temp | 0Eh | ok | 3.1 | 40 degrees C"}
	c := newTestController(rr)
	tun := testTunables()
	tun.pollHot = 5 * time.Second
	tun.hotDuty = 40
	c.setTunables(tun)

	if err := c.step(context.Background()); err != nil { // cool -> idle cadence
		t.Fatal(err)
	}
	if d := c.desiredInterval(); d != tun.pollIdle {
		t.Fatalf("cool interval = %s, want idle %s", d, tun.pollIdle)
	}
	rr.temps = "Temp | 0Eh | ok | 3.1 | 73 degrees C" // ~41% duty >= hotDuty
	if err := c.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d := c.desiredInterval(); d != tun.pollHot {
		t.Fatalf("hot interval = %s, want hot %s", d, tun.pollHot)
	}
}
