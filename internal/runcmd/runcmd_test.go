package runcmd

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func requireSh(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
}

func TestCombinedOutputReturnsOutput(t *testing.T) {
	requireSh(t)
	out, err := CombinedOutput(context.Background(), "sh", "-c", "echo hello")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "hello" {
		t.Fatalf("output = %q, want %q", out, "hello")
	}
}

func TestCombinedOutputCapturesStderrAndExitError(t *testing.T) {
	requireSh(t)
	out, err := CombinedOutput(context.Background(), "sh", "-c", "echo oops >&2; exit 3")
	if err == nil {
		t.Fatal("expected a non-zero exit to surface as an error")
	}
	if !strings.Contains(string(out), "oops") {
		t.Fatalf("output = %q, want it to include stderr", out)
	}
}

// The regression this package exists for: killing the child does not free the
// caller if a grandchild still holds the output pipe open, and a process wedged
// in an uninterruptible driver ioctl cannot be killed at all. Either way the
// deadline has to win, or fanctl's control loop stalls with the fans pinned.
func TestCombinedOutputHonoursDeadlineWhenPipeOutlivesChild(t *testing.T) {
	requireSh(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	// `sleep` inherits the output pipe and outlives the SIGKILL aimed at sh.
	_, err := CombinedOutput(ctx, "sh", "-c", "sleep 30 & wait")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a deadline error")
	}
	if elapsed > 10*time.Second {
		t.Fatalf("CombinedOutput blocked for %s; it must return when ctx expires", elapsed)
	}
}

func TestCombinedOutputRespectsAlreadyCancelledContext(t *testing.T) {
	requireSh(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := CombinedOutput(ctx, "sh", "-c", "echo hi"); err == nil {
		t.Fatal("expected an error for an already-cancelled context")
	}
}
