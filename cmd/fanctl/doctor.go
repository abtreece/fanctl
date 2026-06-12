package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/abtreece/fanctl/internal/config"
	"github.com/abtreece/fanctl/internal/gpu"
	"github.com/abtreece/fanctl/internal/ipmi"
)

// doctorDeps are injected so doctor is testable without a BMC or a real PATH.
type doctorDeps struct {
	stat     func(string) (os.FileInfo, error)
	lookPath func(string) (string, error)
	newIPMI  func(*config.Config) doctorIPMI
	newGPU   func(*config.Config) doctorGPU
}

// doctorIPMI is the BMC surface doctor exercises.
type doctorIPMI interface {
	Temperatures(context.Context) ([]ipmi.Temp, error)
	Fans(context.Context) ([]ipmi.Fan, error)
	FirmwareRevision(context.Context) (string, error)
}

// doctorGPU is the GPU surface doctor exercises.
type doctorGPU interface {
	Temperatures(context.Context) ([]gpu.Temp, error)
}

func defaultDoctorDeps() doctorDeps {
	return doctorDeps{
		stat:     os.Stat,
		lookPath: exec.LookPath,
		newIPMI: func(cfg *config.Config) doctorIPMI {
			return newIPMIClient(cfg)
		},
		newGPU: func(cfg *config.Config) doctorGPU {
			return gpu.New(cfg.GPU.Command, gpu.ExecRunner)
		},
	}
}

func runDoctor(args []string) int {
	fs := flag.NewFlagSet("fanctl doctor", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cfgPath := fs.String("config", config.DefaultPath, "Path to config file")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	return runDoctorWithDeps(*cfgPath, os.Stdout, defaultDoctorDeps())
}

func runDoctorWithDeps(cfgPath string, stdout io.Writer, deps doctorDeps) int {
	report := &doctorReport{w: stdout}

	cfg := config.Default(cfgPath)
	if err := config.LoadFile(cfgPath, cfg); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			report.warn("config", "%s not found; using built-in defaults", cfgPath)
		} else {
			report.fail("config", "%v", err)
		}
	} else {
		report.ok("config", "loaded %s", cfgPath)
	}
	if err := config.Validate(cfg); err != nil {
		report.fail("config", "%v", err)
	} else {
		report.ok("config", "schema validation passed (%d curve bands)", len(cfg.Curve))
	}

	if _, err := deps.lookPath(cfg.IPMITool); err != nil {
		report.fail("ipmitool", "%q not found in PATH", cfg.IPMITool)
		return report.exitCode() // nothing below works without it
	}
	report.ok("ipmitool", "%q found", cfg.IPMITool)

	if _, err := deps.stat("/dev/ipmi0"); err != nil {
		report.warn("ipmi.device", "/dev/ipmi0 not present; ensure the ipmi_devintf kernel module is loaded")
	} else {
		report.ok("ipmi.device", "/dev/ipmi0 present")
	}

	client := deps.newIPMI(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if rev, err := client.FirmwareRevision(ctx); err != nil {
		report.warn("bmc.firmware", "could not read firmware revision: %v", err)
	} else {
		report.ok("bmc.firmware", "BMC firmware revision %s", rev)
	}

	temps, err := client.Temperatures(ctx)
	if err != nil {
		report.fail("sensors.read", "could not read temperatures: %v", err)
	} else {
		matched := 0
		for _, t := range temps {
			if cfg.Sensors.Selects(t.ID, t.Name) {
				matched++
			}
		}
		if matched == 0 {
			report.fail("sensors.match", "no temperature sensors matched the selector (have %d sensors); adjust sensors.ids/name_match", len(temps))
		} else {
			report.ok("sensors.match", "%d of %d temperature sensors selected by the curve", matched, len(temps))
		}
	}

	if fans, err := client.Fans(ctx); err != nil {
		report.fail("fans.read", "could not read fans: %v", err)
	} else if len(fans) == 0 {
		report.warn("fans.read", "no fan RPM readings returned")
	} else {
		report.ok("fans.read", "%d fans, average %d RPM", len(fans), ipmi.AverageRPM(fans))
	}

	if cfg.GPU.Enabled {
		if _, err := deps.lookPath(cfg.GPU.Command); err != nil {
			report.fail("gpu.tool", "%q not found in PATH (gpu monitoring is enabled)", cfg.GPU.Command)
		} else if temps, err := deps.newGPU(cfg).Temperatures(ctx); err != nil {
			report.fail("gpu.read", "gpu monitoring enabled but temperature read failed: %v", err)
		} else if t, ok := gpu.Max(temps); ok {
			report.ok("gpu.read", "%d GPU(s), hottest %d°C", len(temps), t)
		} else {
			report.fail("gpu.read", "gpu monitoring enabled but no GPU temperatures reported")
		}
	} else {
		report.ok("gpu", "GPU monitoring disabled")
	}

	report.note("run `fanctl probe` to confirm manual fan control is honored before relying on the daemon")
	return report.exitCode()
}

// doctorReport prints aligned ok/warn/FAIL lines and tracks failures.
type doctorReport struct {
	w        io.Writer
	failures int
	warnings int
}

func (r *doctorReport) ok(name, format string, args ...any) {
	fmt.Fprintf(r.w, "ok    %-16s %s\n", name, fmt.Sprintf(format, args...))
}

func (r *doctorReport) warn(name, format string, args ...any) {
	r.warnings++
	fmt.Fprintf(r.w, "warn  %-16s %s\n", name, fmt.Sprintf(format, args...))
}

func (r *doctorReport) fail(name, format string, args ...any) {
	r.failures++
	fmt.Fprintf(r.w, "FAIL  %-16s %s\n", name, fmt.Sprintf(format, args...))
}

func (r *doctorReport) note(msg string) {
	fmt.Fprintf(r.w, "\nnote: %s\n", msg)
}

func (r *doctorReport) exitCode() int {
	if r.failures > 0 {
		return 1
	}
	return 0
}
