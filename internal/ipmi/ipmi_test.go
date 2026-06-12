package ipmi

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// fakeRunner records calls and returns canned output keyed by the joined args.
type fakeRunner struct {
	responses map[string]string
	calls     [][]string
	err       error
}

func (f *fakeRunner) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	if f.err != nil {
		return nil, f.err
	}
	return []byte(f.responses[strings.Join(args, " ")]), nil
}

const sampleTemps = `Inlet Temp       | 04h | ok  |  7.1 | 22 degrees C
Temp             | 0Eh | ok  |  3.1 | 36 degrees C
Temp             | 0Fh | ok  |  3.2 | 35 degrees C
Exhaust Temp     | 01h | ns  |  7.1 | Disabled`

const sampleFans = `Fan1A            | 30h | ok  |  7.1 | 6600 RPM
Fan1B            | 31h | ok  |  7.1 | 6000 RPM
Fan Redundancy   | 75h | ok  |  7.1 | Fully Redundant`

func TestTemperatures(t *testing.T) {
	f := &fakeRunner{responses: map[string]string{"sdr type temperature": sampleTemps}}
	c := New("ipmitool", f.run)
	temps, err := c.Temperatures(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The "Disabled" exhaust row has no numeric reading and must be skipped.
	if len(temps) != 3 {
		t.Fatalf("got %d temps, want 3: %+v", len(temps), temps)
	}
	if temps[1] != (Temp{ID: "0Eh", Name: "Temp", Celsius: 36}) {
		t.Errorf("temps[1] = %+v", temps[1])
	}
}

func TestFans(t *testing.T) {
	f := &fakeRunner{responses: map[string]string{"sdr type fan": sampleFans}}
	c := New("ipmitool", f.run)
	fans, err := c.Fans(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The "Fully Redundant" row is not an RPM reading and must be skipped.
	if len(fans) != 2 {
		t.Fatalf("got %d fans, want 2: %+v", len(fans), fans)
	}
	if got := AverageRPM(fans); got != 6300 {
		t.Errorf("AverageRPM = %d, want 6300", got)
	}
}

func TestSetPercentEncoding(t *testing.T) {
	f := &fakeRunner{responses: map[string]string{}}
	c := New("ipmitool", f.run)
	if err := c.SetPercent(context.Background(), 30); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(f.calls[0], " ")
	want := "ipmitool raw 0x30 0x30 0x02 0xff 0x1e" // 30 -> 0x1e
	if got != want {
		t.Errorf("SetPercent(30) issued %q, want %q", got, want)
	}
}

func TestSetPercentRange(t *testing.T) {
	c := New("ipmitool", (&fakeRunner{}).run)
	if err := c.SetPercent(context.Background(), 101); err == nil {
		t.Error("SetPercent(101) should error")
	}
}

func TestFirmwareRevision(t *testing.T) {
	f := &fakeRunner{responses: map[string]string{
		"mc info": "Device ID                 : 32\nFirmware Revision         : 2.65\nIPMI Version              : 2.0",
	}}
	c := New("ipmitool", f.run)
	rev, err := c.FirmwareRevision(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rev != "2.65" {
		t.Errorf("FirmwareRevision = %q, want 2.65", rev)
	}
}

func TestSDRErrorWraps(t *testing.T) {
	f := &fakeRunner{err: fmt.Errorf("boom")}
	c := New("ipmitool", f.run)
	if _, err := c.Temperatures(context.Background()); err == nil {
		t.Error("expected error from failing runner")
	}
}
