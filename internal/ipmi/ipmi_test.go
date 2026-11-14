package ipmi

import (
	"context"
	"errors"
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
	// Like exec's CombinedOutput, a failing command still returns whatever it
	// printed: that output is the diagnosis.
	return []byte(f.responses[strings.Join(args, " ")]), f.err
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
	c := New(Options{Bin: "ipmitool"}, f.run)
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
	c := New(Options{Bin: "ipmitool"}, f.run)
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
	c := New(Options{Bin: "ipmitool"}, f.run)
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
	c := New(Options{Bin: "ipmitool"}, (&fakeRunner{}).run)
	if err := c.SetPercent(context.Background(), 101); err == nil {
		t.Error("SetPercent(101) should error")
	}
}

func TestFirmwareRevision(t *testing.T) {
	f := &fakeRunner{responses: map[string]string{
		"mc info": "Device ID                 : 32\nFirmware Revision         : 2.65\nIPMI Version              : 2.0",
	}}
	c := New(Options{Bin: "ipmitool"}, f.run)
	rev, err := c.FirmwareRevision(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rev != "2.65" {
		t.Errorf("FirmwareRevision = %q, want 2.65", rev)
	}
}

func TestOutOfBandConnectionArgs(t *testing.T) {
	f := &fakeRunner{responses: map[string]string{}}
	c := New(Options{Bin: "ipmitool", Interface: "lanplus", Host: "10.0.0.5", Username: "admin", Password: "secret"}, f.run)
	if err := c.SetAuto(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(f.calls[0], " ")
	want := "ipmitool -I lanplus -H 10.0.0.5 -U admin -P secret raw 0x30 0x30 0x01 0x01"
	if got != want {
		t.Errorf("out-of-band call = %q, want %q", got, want)
	}
}

func TestInBandHasNoConnectionArgs(t *testing.T) {
	f := &fakeRunner{responses: map[string]string{}}
	c := New(Options{Bin: "ipmitool"}, f.run) // default = in-band
	if err := c.SetAuto(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(f.calls[0], " ")
	want := "ipmitool raw 0x30 0x30 0x01 0x01"
	if got != want {
		t.Errorf("in-band call = %q, want %q", got, want)
	}
}

func TestSDRErrorWraps(t *testing.T) {
	f := &fakeRunner{err: fmt.Errorf("boom")}
	c := New(Options{Bin: "ipmitool"}, f.run)
	if _, err := c.Temperatures(context.Background()); err == nil {
		t.Error("expected error from failing runner")
	}
}

// An exit status alone cannot distinguish a firmware lockout from a privilege
// or connection problem; ipmitool's own message, with its completion code, can.
func TestFailureIncludesIpmitoolOutput(t *testing.T) {
	exitErr := errors.New("exit status 1")
	f := &fakeRunner{
		err: exitErr,
		responses: map[string]string{
			"raw 0x30 0x30 0x01 0x00": "Unable to send RAW command (channel=0x0 netfn=0x30 lun=0x0 cmd=0x30 rsp=0xd4): Insufficient privilege level\n",
		},
	}
	c := New(Options{Bin: "ipmitool"}, f.run)

	err := c.SetManual(context.Background())
	if err == nil {
		t.Fatal("expected error from failing runner")
	}
	want := "ipmitool raw 0x30 0x30 0x01 0x00: exit status 1: Unable to send RAW command (channel=0x0 netfn=0x30 lun=0x0 cmd=0x30 rsp=0xd4): Insufficient privilege level"
	if err.Error() != want {
		t.Errorf("error = %q\nwant    %q", err, want)
	}
	if !errors.Is(err, exitErr) {
		t.Error("error no longer wraps the runner's error")
	}
}

func TestFailureOutputIsSingleLine(t *testing.T) {
	f := &fakeRunner{
		err: errors.New("exit status 1"),
		responses: map[string]string{
			"sdr type fan": "Error: Unable to establish IPMI v2 / RMCP+ session\n\nUnable to open SDR for reading\n",
		},
	}
	c := New(Options{Bin: "ipmitool"}, f.run)

	_, err := c.Fans(context.Background())
	want := "ipmitool sdr type fan: exit status 1: Error: Unable to establish IPMI v2 / RMCP+ session; Unable to open SDR for reading"
	if err == nil || err.Error() != want {
		t.Errorf("error = %v\nwant    %q", err, want)
	}
}

func TestFailureWithoutOutputIsUnchanged(t *testing.T) {
	f := &fakeRunner{err: errors.New("exit status 1")}
	c := New(Options{Bin: "ipmitool"}, f.run)

	err := c.SetAuto(context.Background())
	want := "ipmitool raw 0x30 0x30 0x01 0x01: exit status 1"
	if err == nil || err.Error() != want {
		t.Errorf("error = %v, want %q", err, want)
	}
}
