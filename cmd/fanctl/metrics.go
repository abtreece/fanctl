package main

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// startMetricsServer starts an HTTP server exposing /metrics (Prometheus text
// exposition) and /healthz. The exposition is hand-rendered so fanctl carries
// no Prometheus client dependency. Returns the server for graceful shutdown.
func startMetricsServer(log *slog.Logger, addr string, c *controller) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		writeMetrics(w, c.snapshot())
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		log.Info("metrics server starting", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("metrics server error", "err", err)
		}
	}()
	return srv
}

// writeMetrics renders the controller snapshot as Prometheus text exposition.
func writeMetrics(w io.Writer, s Snapshot) {
	gauge := func(name, help string, val any) {
		_, _ = fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %v\n", name, help, name, name, val)
	}
	counter := func(name, help string, val uint64) {
		_, _ = fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, help, name, name, val)
	}

	gauge("fanctl_up", "fanctl controller is running.", 1)
	counter("fanctl_steps_total", "Total control iterations performed.", s.Steps)
	counter("fanctl_step_errors_total", "Control iterations that failed.", s.StepErrors)
	counter("fanctl_reloads_total", "Configuration reloads applied.", s.Reloads)
	counter("fanctl_reasserts_total", "Periodic manual-control re-asserts issued.", s.Reasserts)
	counter("fanctl_rpm_verify_failures_total", "Duty changes whose RPM response was missing, triggering a re-assert.", s.VerifyFails)

	if !s.HaveObs {
		return // no observation yet; omit value gauges
	}
	gauge("fanctl_temperature_celsius", "Hottest temperature driving the curve (CPU or GPU), in Celsius.", s.TempC)
	if s.HaveGPU {
		gauge("fanctl_gpu_temperature_celsius", "Hottest NVIDIA GPU temperature in Celsius.", s.GPUTempC)
	}
	gauge("fanctl_fan_rpm_avg", "Average fan RPM across all fans.", s.FanRPM)
	gauge("fanctl_fan_percent", "Commanded fan duty percent; -1 when handed to BMC automatic control.", s.Percent)
	gauge("fanctl_bmc_auto", "1 when fan control is handed back to the BMC automatic mode, else 0.", boolToInt(s.BMCAuto))
	gauge("fanctl_fan_percent_computed", "Interpolated duty target before governor smoothing.", fmt.Sprintf("%.1f", s.ComputedPct))
	gauge("fanctl_temp_slope_celsius_per_min", "Steepest temperature rise across sources, °C/min.", fmt.Sprintf("%.2f", s.SlopeCPM))
	gauge("fanctl_hot_poll", "1 when the fast poll cadence is active, else 0.", boolToInt(s.HotPoll))
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
