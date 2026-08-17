package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// notifySocket points NOTIFY_SOCKET at a real unixgram listener and returns the
// channel of messages fanctl sends to it.
func notifySocket(t *testing.T) <-chan string {
	t.Helper()
	// Not t.TempDir(): unix socket paths are capped near 108 bytes and test
	// names make those paths long.
	dir, err := os.MkdirTemp("", "fanctl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	conn, err := net.ListenPacket("unixgram", filepath.Join(dir, "n.sock"))
	if err != nil {
		t.Skipf("unixgram sockets unavailable: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	t.Setenv("NOTIFY_SOCKET", filepath.Join(dir, "n.sock"))

	msgs := make(chan string, 16)
	go func() {
		defer close(msgs)
		buf := make([]byte, 256)
		for {
			n, _, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			msgs <- string(buf[:n])
		}
	}()
	return msgs
}

func TestSdNotifySendsState(t *testing.T) {
	msgs := notifySocket(t)
	if err := sdNotify("READY=1"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-msgs:
		if got != "READY=1" {
			t.Fatalf("received %q, want %q", got, "READY=1")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no notification received")
	}
}

// Off systemd there is no socket; notifying must stay a silent no-op so the
// daemon runs identically from a shell.
func TestSdNotifyNoopWithoutSocket(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	if err := sdNotify("READY=1"); err != nil {
		t.Fatalf("sdNotify without NOTIFY_SOCKET = %v, want nil", err)
	}
}

func TestWatchdogIntervalDisabled(t *testing.T) {
	t.Setenv("WATCHDOG_USEC", "")
	if got := watchdogInterval(); got != 0 {
		t.Fatalf("watchdogInterval without WATCHDOG_USEC = %s, want 0", got)
	}
}

func TestWatchdogIntervalParsed(t *testing.T) {
	t.Setenv("WATCHDOG_USEC", "90000000")
	t.Setenv("WATCHDOG_PID", strconv.Itoa(os.Getpid()))
	if got := watchdogInterval(); got != 90*time.Second {
		t.Fatalf("watchdogInterval = %s, want 90s", got)
	}
}

// WATCHDOG_PID scopes the watchdog to one process; a mismatch means it is not
// ours to service.
func TestWatchdogIntervalWrongPID(t *testing.T) {
	t.Setenv("WATCHDOG_USEC", "90000000")
	t.Setenv("WATCHDOG_PID", strconv.Itoa(os.Getpid()+1))
	if got := watchdogInterval(); got != 0 {
		t.Fatalf("watchdogInterval for another PID = %s, want 0", got)
	}
}

func TestRunWatchdogPingsResponsiveLoop(t *testing.T) {
	msgs := notifySocket(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Stand in for the control loop's select: always ready to take a probe.
	health := make(chan struct{})
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-health:
			}
		}
	}()
	go runWatchdog(ctx, discardLogger(), health, 40*time.Millisecond, time.Second)

	select {
	case got := <-msgs:
		if got != "WATCHDOG=1" {
			t.Fatalf("received %q, want %q", got, "WATCHDOG=1")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a responsive loop was never pinged")
	}
}

// The point of the design: nobody receives on health, so the ping is withheld
// and systemd is left to restart the unit.
func TestRunWatchdogWithholdsPingWhenLoopWedged(t *testing.T) {
	msgs := notifySocket(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	health := make(chan struct{}) // wedged loop: never received from
	go runWatchdog(ctx, discardLogger(), health, 40*time.Millisecond, 50*time.Millisecond)

	select {
	case got, ok := <-msgs:
		if ok {
			t.Fatalf("wedged loop was pinged with %q; systemd would never restart it", got)
		}
	case <-time.After(time.Second):
		// Correct: no ping issued.
	}
}

func TestWatchdogGrace(t *testing.T) {
	// Normal case: one full step plus slack.
	if got := watchdogGrace(15*time.Second, 90*time.Second); got != 25*time.Second {
		t.Fatalf("watchdogGrace(15s, 90s) = %s, want 25s", got)
	}
	// Capped at half the interval so a wedge is still caught within one period.
	if got := watchdogGrace(15*time.Second, 20*time.Second); got != 10*time.Second {
		t.Fatalf("watchdogGrace(15s, 20s) = %s, want 10s", got)
	}
}
