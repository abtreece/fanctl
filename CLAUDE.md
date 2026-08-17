# fanctl

Temperature-based BMC fan controller for Dell PowerEdge over in-band IPMI.
Single Go binary + systemd unit. Reads CPU temps via `ipmitool`, applies a
configurable fan curve with hysteresis using the Dell manual-fan-control raw
commands, and falls back to BMC automatic control on over-temp, read failure, or
shutdown.

## Module

`github.com/abtreece/fanctl` — Go 1.25+
Go binary: `/usr/local/go/bin/go`
`export PATH="$HOME/go/bin:/usr/local/go/bin:$PATH"`

## Build

```bash
make build            # ldflags-stamped binary
make vet test         # vet + unit tests
go build ./...        # all packages
```

Offline builds: the module cache holds kong v1.15.0 and yaml.v3 v3.0.1 (shared
with the sibling runnerctl project). Use `GOPROXY=off` when there's no network;
`go.sum` is seeded and sufficient for build/test. `go mod tidy` needs network
(yaml.v3's test-only `gopkg.in/check.v1`), so don't run it offline.

## Architecture

```
cmd/fanctl/main.go        — kong root (daemon) + os.Args dispatch to doctor/probe/install
cmd/fanctl/run.go         — daemon: control loop + controller (change-detection, fallback-to-auto)
cmd/fanctl/doctor.go      — preflight checks; ok/warn/FAIL report, DI via doctorDeps
cmd/fanctl/probe.go       — controllability sweep: command low duty, measure RPM drop
cmd/fanctl/sweep.go       — duty sweep: step duty down, measure RPM/temp, `-suggest` a floor
cmd/fanctl/install.go     — //go:embed systemd unit + example config; write + enable
cmd/fanctl/restore.go     — `restore-auto` subcommand; what the unit's ExecStopPost runs
cmd/fanctl/watchdog.go    — sd_notify (no dep) + loop-liveness watchdog pings
cmd/fanctl/embed/         — embedded fanctl.service + config.example.yaml
internal/runcmd/          — exec helper whose deadline holds even if the child can't be killed
internal/ipmi/ipmi.go     — ipmitool wrapper; injectable Runner; SDR parsing, raw set commands
internal/fan/curve.go     — PURE policy: Curve.Duty interpolation + Governor + Predictor; no I/O
internal/config/          — Config + SensorConfig + Default(); rawFile YAML loader + Validate
systemd/fanctl.service    — packaged unit (mirror of embed copy)
config/config.example.yaml — packaged example (mirror of embed copy)
packaging/                — nfpm postinstall/preremove (preremove restores BMC auto)
```

## Conventions (inherited from runnerctl)

- CLI: `alecthomas/kong` for the root daemon; stdlib `flag` for dispatched
  subcommands (doctor/probe/install). Subcommands dispatched from `os.Args`
  before `kong.Parse`.
- Config: `gopkg.in/yaml.v3`; `Default()` seeds defaults, `LoadFile` overlays
  file values, `Validate` checks consistency.
- Logging: `log/slog`, text handler to stderr.
- Errors: lowercase, no trailing punctuation, wrap with `%w`.
- Testability: inject the command runner (`ipmi.Runner`) and doctor deps so
  logic is unit-tested without a BMC. Keep `internal/fan` pure.
- Tabs in Go (gofmt); 2-space YAML. Conventional commits.

## Key decisions

- **Manual fan control raw commands:** `0x30 0x30 0x01 0x00` (manual),
  `0x30 0x30 0x02 0xff <pct-hex>` (set duty), `0x30 0x30 0x01 0x01` (auto).
  Dell 12G/13G.
- **Curve policy:** upward moves are immediate (responsiveness = safety);
  downward moves step one band at a time, gated by hysteresis below the lower
  band's ceiling; leaving BMC-auto also requires the hysteresis margin. All in
  `internal/fan`, fully unit-tested.
- **Fallback to BMC auto** is the safe state: on over-temp (above top band),
  unreadable sensors, control-step error, and daemon shutdown (defer + unit
  `ExecStopPost`).
- **probe** exists because Dell's forced baseline often sits ~30% duty (so
  commanding 30% looks like a no-op) and some iDRAC firmware ignores the raw
  commands. Probe commands a LOW duty and measures the RPM drop.
- **sweep** is probe generalised across a range, for choosing the curve's bottom
  anchor — below it the curve is flat, so that anchor's percent *is* the idle
  duty. `-suggest` walks steps from most to least airflow and stops at the first
  breakdown (fan spread blowing out, or RPM no longer falling); it must not
  resume below that point, because once fans bottom out the lower steps look
  well behaved again on their own numbers. Two limits are stated in its output
  rather than hidden: settle time measures a transient (equilibrium runs
  several °C higher), and without `gpu.enabled` it cannot see passively-cooled
  cards with no temperature sensor of their own. With `gpu.enabled` it reads the
  GPU as well, and an unreadable GPU aborts the sweep — same fail-safe invariant
  as the daemon. To keep that invariant true, sweep (unlike doctor and probe)
  *requires* the config it was pointed at: falling back to defaults would clear
  `gpu.enabled` and leave a T4 unwatched during the one operation that
  deliberately drives duty toward zero.
- **Both `-suggest` breakdown tests are relative, not absolute.** Fan spread is
  judged as a *rise* over the sweep's own reference spread (baseline, else the
  top step), because the R430 idles with its fans 37% apart at every duty and an
  absolute threshold called that host broken at the first step. The clamp test
  compares RPM-per-duty-point slope against the steepest slope seen so far, not
  a raw RPM drop against a percentage of the previous step, so the verdict does
  not change with how finely the sweep was stepped. An absolute spread ceiling
  and an absolute minimum slope still catch hosts that are already incoherent or
  already clamped at the first step, where there is no healthy reference.
- **Lowering the idle floor on a GPU host means lowering both curves.** The
  daemon commands `max(cpuCurveDuty, gpuCurveDuty)` (`run.go`) and `Curve.Duty`
  is flat below its first anchor, so a `gpu.curve` starting at `55 → 20` holds
  idle at 20% no matter how low the main curve's floor goes. `sweep -suggest`
  says so on GPU hosts.
- **Sensor selection:** explicit SDR `ids` win; otherwise name include/exclude
  (default match "Temp", exclude "Inlet"/"Exhaust") picks CPU sensors across
  Dell generations.
- **A hang is not a safe state.** Fallback-to-auto only protects against *errors*;
  a wedged `ipmitool` used to stall the loop forever with the fans pinned and no
  path back to BMC auto. Three layers now prevent that: `step_timeout` (default
  15s, capped at the shortest poll cadence in play) bounds each iteration and its
  fallback;
  `internal/runcmd` makes that deadline hold even when the child ignores SIGKILL
  (`exec.CommandContext` + `CombinedOutput` blocks while any grandchild holds the
  output pipe, and a process stuck in an uninterruptible `/dev/ipmi0` ioctl cannot
  be killed at all); and the systemd watchdog restarts the unit if the loop stops
  servicing its select. `step_timeout` is restart-only — the watchdog grace period
  is derived from it.
- **The watchdog pings from the control loop, not a timer.** `runWatchdog` proves
  liveness by sending on an unbuffered channel the loop must receive. A goroutine
  pinging on a timer would keep a wedged daemon alive, which is the exact failure
  being guarded against.
- **`restore-auto` instead of a raw command in the unit.** `ExecStopPost` cannot
  know the connection: a `lanplus` host has no local `/dev/ipmi0`, and `ipmitool`
  is in `/usr/bin` on Debian-family and `/usr/sbin` on RHEL-family. Routing through
  fanctl reuses the configured connection. For the same reason the unit no longer
  carries `ConditionPathExists=/dev/ipmi0`, which silently blocked out-of-band
  deployments from starting at all; `doctor` checks the device instead.

## Origin

Extracted from a working bash fan-curve service on a PowerEdge R420 (iDRAC
firmware 2.65, loud at idle due to an LSI SAS2008 HBA forcing a ~6100 RPM
baseline). The same controller is intended to run on the user's R430 too —
hence config-driven sensor selection and curve rather than hardcoded values.

## Roadmap

- [x] Optional out-of-band IPMI (lanplus) for remote BMCs — `connection:` config,
  `ipmi.Options`/`ipmi.New` prepends `-I/-H/-U/-P`.
- [x] Config file watch / SIGHUP reload — `watch.go` (fsnotify on config dir) +
  reload closure in `runDaemon`; reloads curve/sensors/hysteresis/poll interval,
  warns that connection/ipmitool changes need a restart.
- [x] Prometheus metrics endpoint — `metrics.go` hand-renders the text
  exposition (no client dep); optional `metrics.listen`. Controller keeps a
  mutex-guarded `Snapshot`; serves /metrics + /healthz.
- [x] GPU-aware cooling — `internal/gpu` reads nvidia-smi; controller drives the
  curve off max(CPU, GPU) and FAILS SAFE to BMC-auto when GPU monitoring is
  enabled but unreadable. `gpu.enabled`/`gpu.command` config; doctor + metrics
  surface GPU temp. Added for an R430 with a passively-cooled Tesla T4.
- [ ] Per-zone fan control where the chassis exposes it.

## GPU hosts

`internal/gpu` shells out to `nvidia-smi --query-gpu=temperature.gpu`. The
controller takes max(hottest selected IPMI sensor, hottest GPU). Key invariant:
GPU enabled + read failure => `fan.AutoLevel` (BMC auto), never CPU-only cooling
— a passively-cooled GPU (T4) relies on chassis airflow, so being GPU-blind is
unsafe. The R430 (Tesla T4) is the target host; deploy in-band there with
`gpu.enabled: true` and a curve whose top band hands back to the BMC by ~80°C.
