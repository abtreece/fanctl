# fanctl

Temperature-based BMC fan controller for Dell PowerEdge servers, over in-band
IPMI. A single Go binary plus a systemd unit: it reads CPU temperatures from the
BMC, sets a sane fan duty on a configurable curve, and hands control back to the
iDRAC's own thermal management whenever things get hot or a reading fails.

Built for the homelab problem where a server's fans run loud at idle — typically
because a third-party PCIe card (an HBA, NIC, or GPU the iDRAC has no thermal
profile for) forces a high baseline that has nothing to do with the actual
temperature.

## Why not just `ipmitool raw ...` in a cron job?

Because the raw approach has a trap that is easy to misdiagnose: on Dell 12G the
forced baseline often sits around 30% duty, so commanding `30%` looks like it
did nothing, and some iDRAC firmware silently ignores the raw fan commands
entirely. `fanctl probe` exists specifically to settle that question — it
commands a *low* duty and measures whether RPM actually drops.

## Install

From a release package:

```sh
sudo dpkg -i fanctl_linux_amd64.deb      # or .rpm / .apk
sudo fanctl doctor                       # preflight
sudo fanctl probe                        # confirm manual control is honored
sudo systemctl enable --now fanctl
```

Or straight from a built binary:

```sh
make build
sudo ./fanctl install                    # writes unit + /etc/fanctl/config.yaml, enables service
```

## Commands

| Command | What it does |
| --- | --- |
| `fanctl` (or `fanctl run`) | Run the control loop (the daemon). |
| `fanctl doctor` | Preflight: ipmitool present, `/dev/ipmi0`, firmware revision, sensor selection, fan readings. |
| `fanctl probe` | Command a low duty and measure the RPM drop to confirm manual fan control works. |
| `fanctl sweep` | Step duty down through a range, measuring RPM and temperature at each, to pick a curve floor. |
| `fanctl install` | Write the systemd unit and an initial config, then enable the service. |
| `fanctl restore-auto` | Hand the fans back to the BMC's automatic control. What the unit's `ExecStopPost` runs. |

Useful daemon flags: `--dry-run` (compute and log, never write to the BMC),
`--once` (a single iteration then exit), `--config PATH`, `--log-level`.

### Choosing a curve floor with `sweep`

Below its lowest anchor the curve is flat, so that anchor's percent *is* the
idle duty — and the quietest useful value for it is a property of the host, not
something to guess. `sweep` measures it:

```sh
sudo systemctl stop fanctl               # a running daemon would fight the sweep
sudo fanctl sweep -suggest
sudo systemctl start fanctl
```

```
DUTY      AVG RPM  MIN   MAX   SPREAD  TEMP  INLET
baseline  2740     2640  3000  13%     45°C  24°C
10%       3040     2880  3360  15%     45°C  24°C
8%        2740     2640  3000  13%     44°C  24°C
6%        2420     2160  2520  14%     44°C  24°C
4%        2030     1560  2280  35%     45°C  24°C
2%        1820     1560  1920  19%     44°C  24°C
0%        1560     1440  1680  15%     53°C  24°C

suggested floor: 6% -- 2420 RPM, about 5 dB quieter than 10% (estimated from RPM)
  lower steps rejected: at 4% the fans stopped tracking together (spread 35%, was 13% at the top of the sweep)
```

The sweep visits every step you give it; `-suggest` is what decides which are
usable. It walks them from most to least airflow and stops at the first sign
that duty no longer controls the fans:

- **Fan spread opening up** — some fans have hit their own floor while others
  still follow the commanded duty. This is judged as a *rise* over the sweep's
  own reference spread, not an absolute: chassis differ, and a host whose fans
  normally sit 37% apart at every duty is not broken. An absolute ceiling still
  applies for a host that is already incoherent at the first step.
- **RPM ceasing to fall** — a firmware clamp. Compared as RPM-per-duty-point
  slope against the steepest slope seen so far, so the verdict does not change
  with how finely you stepped the sweep.

It deliberately does not resume below the first breakdown. The 2% and 0% rows
above show why: their spread closes back to 19% and 15%, looking well behaved
again purely because every fan is now sitting on the same floor. A rule judging
each step on its own numbers would sail past the 4% breakdown and recommend 0%.

Each step aborts the sweep if a selected sensor reaches `-max-temp` (default
60°C), a GPU reaches `-max-gpu-temp` (default 75°C), or any fan falls below
`-min-rpm` (default 900). BMC automatic control is restored on every exit path,
including Ctrl-C.

Sweep refuses to run while the daemon is active, since fanctl would overwrite
the commanded duty within one poll and the numbers would be silently wrong.
Detection is best-effort (`systemctl is-active`); pass `-force` where it is
wrong, such as a remote BMC. `-steps`, `-settle`, and `-format`
(`table`/`tsv`/`json`) are the other knobs — see `fanctl sweep -h`.

On a host with `gpu.enabled`, sweep reads the GPU too: the table gains a GPU
column, the abort checks cover it, and an unreadable GPU fails the sweep rather
than continuing on CPU data alone — the same invariant the daemon holds, since
a passively-cooled card is exactly what a lowered floor puts at risk.

To keep that invariant true, sweep — unlike `doctor` and `probe` — *requires*
the config it was pointed at and will not fall back to built-in defaults. The
defaults clear `gpu.enabled`, so a mistyped `-config` would otherwise drop a
passively-cooled GPU out of the safety envelope, silently and with exit 0,
during the one operation that deliberately drives duty toward zero. A bad
`-config` exits 2 before contacting the BMC.

Two things to read off the output with care:

- Settle time measures a **transient**. A chassis takes minutes to reach thermal
  equilibrium, so idle temperature at the chosen floor will land above the
  figure in the table — hold the duty for ~10 minutes before placing the anchor.
- Without `gpu.enabled` it sees fans and the configured sensors only. A
  **passively-cooled card** that depends on chassis airflow and exposes no
  temperature of its own — an HBA, for instance — may require a higher floor
  than the fans and CPUs imply.

On a GPU host, lower the bottom anchor of **both** `curve` and `gpu.curve`. The
daemon commands the higher of the two and each is flat below its own first
anchor, so a `gpu.curve` still starting at `55 → 20` pins idle at 20% however
low the main curve's floor goes.

## Configuration

`/etc/fanctl/config.yaml` (every field optional; defaults target a Dell
12G/13G host):

```yaml
ipmitool: ipmitool
poll_interval: 30s
hysteresis: 4
sensors:
  name_match: ["Temp"]
  name_exclude: ["Inlet", "Exhaust"]
curve:
  - { max_temp: 50, percent: 10 }
  - { max_temp: 60, percent: 20 }
  - { max_temp: 68, percent: 30 }
  - { max_temp: 75, percent: 45 }
```

- **sensors** — which temperature SDR records drive the curve. Set explicit
  `ids` (e.g. `["0Eh", "0Fh"]`) to pin them, or match by name. `fanctl doctor`
  prints how many sensors your selector matched.
- **curve** — ascending temperature ceilings (°C) → fan duty percent. The
  hottest selected sensor picks the band. A temperature above the top band, or
  any failure to read sensors, hands control back to the BMC's automatic mode.
- **hysteresis** — °C margin the temperature must fall *below* a band boundary
  before stepping down, so the fans don't hunt at the edge. Upward moves are
  immediate.

### Remote BMCs (out-of-band)

By default fanctl talks to the local BMC in-band via `/dev/ipmi0`. To control a
remote host's BMC instead, use the out-of-band `lanplus` interface:

```yaml
connection:
  interface: lanplus       # "open" (default, in-band) or "lanplus"
  host: 10.0.0.5
  username: admin
  password: ${IPMI_PASSWORD}
```

Reference the password via an environment variable (expanded at load time) and
set it in the unit's environment rather than committing a secret to the file.
One fanctl process per BMC — run it wherever is convenient and point each
instance at a different host.

## Metrics

Set `metrics.listen` to expose a Prometheus endpoint (hand-rendered text
exposition — fanctl pulls in no Prometheus client dependency):

```yaml
metrics:
  listen: ":9466"
```

`GET /metrics` reports `fanctl_temperature_celsius`, `fanctl_fan_rpm_avg`,
`fanctl_fan_percent` (-1 when handed to BMC auto), `fanctl_bmc_auto`, and the
`fanctl_steps_total` / `fanctl_step_errors_total` / `fanctl_reloads_total`
counters. `GET /healthz` returns 200 while the daemon is up.

## GPU-aware cooling

Hosts with a passively-cooled datacenter GPU (e.g. an NVIDIA Tesla T4) dissipate
the GPU's heat entirely through chassis airflow — your server fans. The GPU's
temperature is not in the BMC's IPMI sensors, so CPU-only fan control can starve
a hot GPU of airflow. Enable GPU monitoring to factor it in:

```yaml
gpu:
  enabled: true
  command: nvidia-smi   # optional override
```

fanctl then drives the curve off `max(hottest CPU sensor, hottest GPU)`. Crucial
safety property: if GPU monitoring is enabled and `nvidia-smi` cannot be read,
fanctl **hands control back to the BMC's automatic mode** rather than cooling on
CPU data alone. On a GPU host, set the curve's top band where you want the BMC to
take over (a T4 throttles around 89 °C, so handing back by ~80 °C is sensible).
With monitoring on, `fanctl doctor` reports the GPU temperature and `/metrics`
exposes `fanctl_gpu_temperature_celsius`.

## Safety model

The BMC's automatic thermal management is always the fallback. fanctl returns
control to it when:

- the hottest sensor exceeds the top curve band,
- a temperature or fan read fails,
- the daemon stops or crashes (both `fanctl` on shutdown and the unit's
  `ExecStopPost` re-assert automatic control).

Manual mode holds a fixed duty with no automatic ramp, so set the top band where
you're comfortable handing back to the firmware.

## Supported hardware

Dell PowerEdge 12G/13G (R420/R430-class and similar) using the standard Dell
manual-fan-control raw commands (`0x30 0x30 ...`) over in-band IPMI. Other
generations may work; run `fanctl probe` to find out. Requires `ipmitool` and
the `ipmi_devintf` kernel module (`/dev/ipmi0`).

## License

Apache-2.0.
