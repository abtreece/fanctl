package main

import (
	"strings"
	"testing"
)

func TestWriteMetrics(t *testing.T) {
	var b strings.Builder
	writeMetrics(&b, Snapshot{
		HaveObs: true, TempC: 41, FanRPM: 3040, Percent: 10, BMCAuto: false,
		Steps: 7, StepErrors: 1, Reloads: 2,
	})
	out := b.String()
	for _, want := range []string{
		"fanctl_up 1",
		"fanctl_steps_total 7",
		"fanctl_step_errors_total 1",
		"fanctl_reloads_total 2",
		"fanctl_temperature_celsius 41",
		"fanctl_fan_rpm_avg 3040",
		"fanctl_fan_percent 10",
		"fanctl_bmc_auto 0",
		"# TYPE fanctl_steps_total counter",
		"# TYPE fanctl_temperature_celsius gauge",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics output missing %q\n---\n%s", want, out)
		}
	}
}

func TestWriteMetricsNoObservationOmitsGauges(t *testing.T) {
	var b strings.Builder
	writeMetrics(&b, Snapshot{HaveObs: false, Steps: 0})
	out := b.String()
	if strings.Contains(out, "fanctl_temperature_celsius") {
		t.Errorf("should omit value gauges before first observation:\n%s", out)
	}
	if !strings.Contains(out, "fanctl_up 1") {
		t.Errorf("fanctl_up should always be present:\n%s", out)
	}
}

func TestMetricsAutoMode(t *testing.T) {
	var b strings.Builder
	writeMetrics(&b, Snapshot{HaveObs: true, Percent: -1, BMCAuto: true})
	out := b.String()
	if !strings.Contains(out, "fanctl_fan_percent -1") || !strings.Contains(out, "fanctl_bmc_auto 1") {
		t.Errorf("auto-mode metrics wrong:\n%s", out)
	}
}
