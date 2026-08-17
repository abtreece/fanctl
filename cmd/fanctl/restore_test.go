package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeAutoSetter struct {
	err   error
	calls int
}

func (f *fakeAutoSetter) SetAuto(context.Context) error {
	f.calls++
	return f.err
}

func TestRestoreAutoSuccess(t *testing.T) {
	var stdout, stderr bytes.Buffer
	f := &fakeAutoSetter{}

	if code := restoreAuto(&stdout, &stderr, f, time.Second); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	if f.calls != 1 {
		t.Fatalf("SetAuto called %d times, want 1", f.calls)
	}
	if !strings.Contains(stdout.String(), "restored") {
		t.Fatalf("stdout = %q, want a confirmation", stdout.String())
	}
}

// ExecStopPost failures should be visible and non-zero, not silently swallowed.
func TestRestoreAutoFailureExitsNonZero(t *testing.T) {
	var stdout, stderr bytes.Buffer
	f := &fakeAutoSetter{err: errors.New("bmc unreachable")}

	if code := restoreAuto(&stdout, &stderr, f, time.Second); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "bmc unreachable") {
		t.Fatalf("stderr = %q, want the underlying error", stderr.String())
	}
}

// The unit runs this as ExecStopPost, where a hung BMC would otherwise delay
// shutdown indefinitely.
func TestRestoreAutoRespectsTimeout(t *testing.T) {
	blocking := autoSetterFunc(func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})

	done := make(chan int, 1)
	go func() {
		var stdout, stderr bytes.Buffer
		done <- restoreAuto(&stdout, &stderr, blocking, 50*time.Millisecond)
	}()

	select {
	case code := <-done:
		if code != 1 {
			t.Fatalf("exit code = %d, want 1 on timeout", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("restoreAuto ignored its timeout against a hung BMC")
	}
}

type autoSetterFunc func(context.Context) error

func (f autoSetterFunc) SetAuto(ctx context.Context) error { return f(ctx) }
