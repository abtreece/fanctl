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
| `fanctl install` | Write the systemd unit and an initial config, then enable the service. |

Useful daemon flags: `--dry-run` (compute and log, never write to the BMC),
`--once` (a single iteration then exit), `--config PATH`, `--log-level`.

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
