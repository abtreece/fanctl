package main

import (
	"context"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// sdNotify sends a status line to systemd's notification socket. It is a no-op
// when NOTIFY_SOCKET is unset — i.e. when not running under a systemd unit — so
// the daemon behaves identically from a shell, in tests, and under systemd.
func sdNotify(state string) error {
	addr := os.Getenv("NOTIFY_SOCKET")
	if addr == "" {
		return nil
	}
	// A leading '@' denotes a socket in the abstract namespace, whose real
	// name starts with a NUL byte.
	if strings.HasPrefix(addr, "@") {
		addr = "\x00" + addr[1:]
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: addr, Net: "unixgram"})
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	_, err = conn.Write([]byte(state))
	return err
}

// watchdogInterval reports how often systemd expects a WATCHDOG=1 ping, or 0
// when the watchdog is not enabled for this process.
func watchdogInterval() time.Duration {
	usec, err := strconv.ParseInt(os.Getenv("WATCHDOG_USEC"), 10, 64)
	if err != nil || usec <= 0 {
		return 0
	}
	// WATCHDOG_PID, when set, scopes the watchdog to one process so a forked
	// child cannot keep the unit alive on the main process's behalf.
	if pid := os.Getenv("WATCHDOG_PID"); pid != "" && pid != strconv.Itoa(os.Getpid()) {
		return 0
	}
	return time.Duration(usec) * time.Microsecond
}

// runWatchdog pings systemd's watchdog only for as long as the control loop is
// still servicing its select. Liveness is probed by sending on health, which
// only succeeds if the loop receives it; a wedged loop makes the probe time out,
// the ping is withheld, and systemd restarts the unit — whose ExecStopPost hands
// the fans back to BMC automatic control.
//
// A plain "ping on a timer" goroutine would keep the unit alive while the loop
// was stuck, which is precisely the failure this guards against.
//
// grace bounds how long a healthy loop may legitimately be busy (one full
// control step) before it counts as wedged.
func runWatchdog(ctx context.Context, log *slog.Logger, health chan<- struct{}, interval, grace time.Duration) {
	ticker := time.NewTicker(interval / 2)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		if !loopIsLive(ctx, health, grace) {
			if ctx.Err() != nil {
				return
			}
			log.Error("control loop unresponsive; withholding watchdog ping", "grace", grace)
			continue
		}
		if err := sdNotify("WATCHDOG=1"); err != nil {
			log.Warn("watchdog ping failed", "err", err)
		}
	}
}

// loopIsLive reports whether the control loop accepted a liveness probe within
// grace.
func loopIsLive(ctx context.Context, health chan<- struct{}, grace time.Duration) bool {
	t := time.NewTimer(grace)
	defer t.Stop()
	select {
	case health <- struct{}{}:
		return true
	case <-t.C:
		return false
	case <-ctx.Done():
		return false
	}
}

// watchdogGrace derives the unresponsiveness threshold from the step timeout,
// capped at half the watchdog interval so a wedged loop is still detected
// within one systemd watchdog period.
func watchdogGrace(stepTimeout, interval time.Duration) time.Duration {
	grace := stepTimeout + 10*time.Second
	if max := interval / 2; grace > max {
		grace = max
	}
	return grace
}
