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
cmd/fanctl/install.go     — //go:embed systemd unit + example config; write + enable
cmd/fanctl/embed/         — embedded fanctl.service + config.example.yaml
internal/ipmi/ipmi.go     — ipmitool wrapper; injectable Runner; SDR parsing, raw set commands
internal/fan/curve.go     — PURE curve logic: Level(prev,temp) with hysteresis; no I/O
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
- **Sensor selection:** explicit SDR `ids` win; otherwise name include/exclude
  (default match "Temp", exclude "Inlet"/"Exhaust") picks CPU sensors across
  Dell generations.

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
