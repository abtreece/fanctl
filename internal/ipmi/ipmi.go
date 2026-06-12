// Package ipmi is a thin wrapper around the in-band ipmitool interface: it reads
// temperature and fan SDR records and drives the Dell manual-fan-control raw
// commands. The command runner is injectable so the parsing and control logic
// can be unit-tested without a BMC.
package ipmi

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Runner executes a command and returns its combined output. Injected for tests.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// ExecRunner is the default Runner: it shells out to the real binary.
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// Client talks to the BMC through ipmitool.
type Client struct {
	bin string
	run Runner
}

// New returns a Client. bin defaults to "ipmitool" when empty; run defaults to
// ExecRunner when nil.
func New(bin string, run Runner) *Client {
	if bin == "" {
		bin = "ipmitool"
	}
	if run == nil {
		run = ExecRunner
	}
	return &Client{bin: bin, run: run}
}

// Temp is a parsed temperature SDR record.
type Temp struct {
	ID      string // SDR record id, e.g. "0Eh"
	Name    string // sensor name, e.g. "Temp" or "Inlet Temp"
	Celsius int
}

// Fan is a parsed fan SDR record.
type Fan struct {
	ID   string
	Name string
	RPM  int
}

// Temperatures returns every temperature sensor reported by the SDR.
func (c *Client) Temperatures(ctx context.Context) ([]Temp, error) {
	lines, err := c.sdr(ctx, "temperature")
	if err != nil {
		return nil, err
	}
	var out []Temp
	for _, ln := range lines {
		f := splitSDR(ln)
		if len(f) < 5 {
			continue
		}
		v, ok := leadingInt(f[4]) // "36 degrees C"
		if !ok {
			continue
		}
		out = append(out, Temp{ID: f[1], Name: f[0], Celsius: v})
	}
	return out, nil
}

// Fans returns every fan sensor reporting an RPM value.
func (c *Client) Fans(ctx context.Context) ([]Fan, error) {
	lines, err := c.sdr(ctx, "fan")
	if err != nil {
		return nil, err
	}
	var out []Fan
	for _, ln := range lines {
		f := splitSDR(ln)
		if len(f) < 5 || !strings.Contains(f[4], "RPM") {
			continue
		}
		v, ok := leadingInt(f[4]) // "6600 RPM"
		if !ok {
			continue
		}
		out = append(out, Fan{ID: f[1], Name: f[0], RPM: v})
	}
	return out, nil
}

// SetManual switches fan control to manual (Dell 12G/13G raw command).
func (c *Client) SetManual(ctx context.Context) error {
	return c.raw(ctx, "0x30", "0x30", "0x01", "0x00")
}

// SetAuto hands fan control back to the BMC's automatic thermal management.
func (c *Client) SetAuto(ctx context.Context) error {
	return c.raw(ctx, "0x30", "0x30", "0x01", "0x01")
}

// SetPercent sets a fixed fan duty percent (requires manual mode first).
func (c *Client) SetPercent(ctx context.Context, pct int) error {
	if pct < 0 || pct > 100 {
		return fmt.Errorf("fan percent %d out of range 0-100", pct)
	}
	return c.raw(ctx, "0x30", "0x30", "0x02", "0xff", fmt.Sprintf("0x%02x", pct))
}

// FirmwareRevision returns the BMC firmware revision string from `mc info`.
func (c *Client) FirmwareRevision(ctx context.Context) (string, error) {
	out, err := c.run(ctx, c.bin, "mc", "info")
	if err != nil {
		return "", fmt.Errorf("ipmitool mc info: %w", err)
	}
	for _, ln := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "Firmware Revision") {
			if _, v, ok := strings.Cut(ln, ":"); ok {
				return strings.TrimSpace(v), nil
			}
		}
	}
	return "", fmt.Errorf("firmware revision not found in mc info output")
}

func (c *Client) sdr(ctx context.Context, typ string) ([]string, error) {
	out, err := c.run(ctx, c.bin, "sdr", "type", typ)
	if err != nil {
		return nil, fmt.Errorf("ipmitool sdr type %s: %w", typ, err)
	}
	return strings.Split(strings.TrimRight(string(out), "\n"), "\n"), nil
}

func (c *Client) raw(ctx context.Context, args ...string) error {
	full := append([]string{"raw"}, args...)
	if _, err := c.run(ctx, c.bin, full...); err != nil {
		return fmt.Errorf("ipmitool %s: %w", strings.Join(full, " "), err)
	}
	return nil
}

// AverageRPM returns the mean RPM across fans, or 0 when there are none.
func AverageRPM(fans []Fan) int {
	if len(fans) == 0 {
		return 0
	}
	sum := 0
	for _, f := range fans {
		sum += f.RPM
	}
	return sum / len(fans)
}

// splitSDR splits an ipmitool SDR line on '|' and trims each field.
func splitSDR(line string) []string {
	parts := strings.Split(line, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// leadingInt parses the first whitespace-delimited token of s as an int, e.g.
// "36 degrees C" -> 36, "6600 RPM" -> 6600.
func leadingInt(s string) (int, bool) {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, false
	}
	return n, true
}
