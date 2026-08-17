// Package runcmd runs external commands under a caller-supplied deadline that
// is honoured even when the child process cannot be killed. Both the IPMI and
// GPU readers depend on this, so it lives in one place rather than being
// reimplemented — and quietly diverging — in each.
package runcmd

import (
	"context"
	"os/exec"
	"time"
)

// waitDelay bounds how long Wait lingers on output pipes that are still held
// open after the context-triggered kill.
const waitDelay = 5 * time.Second

// CombinedOutput runs name with args and returns its combined output, returning
// as soon as ctx expires no matter what the child is doing.
//
// exec.CommandContext alone is not sufficient: it SIGKILLs the process, but
// CombinedOutput still blocks until every writer to the output pipe closes it,
// and a process wedged in an uninterruptible driver ioctl — a hung /dev/ipmi0,
// a hung GPU — cannot be killed at all. fanctl's control loop must never stall
// on that, because a stalled loop leaves the fans pinned at their last
// commanded duty with no path back to BMC automatic control.
func CombinedOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = waitDelay

	type result struct {
		out []byte
		err error
	}
	// Buffered so the goroutine can always deliver and exit, even once the
	// select below has abandoned it.
	done := make(chan result, 1)
	go func() {
		out, err := cmd.CombinedOutput()
		done <- result{out, err}
	}()

	select {
	case r := <-done:
		return r.out, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
