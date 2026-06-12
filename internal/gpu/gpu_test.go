package gpu

import (
	"context"
	"fmt"
	"testing"
)

type fakeRunner struct {
	out string
	err error
}

func (f fakeRunner) run(_ context.Context, _ string, _ ...string) ([]byte, error) {
	return []byte(f.out), f.err
}

func TestTemperatures(t *testing.T) {
	r := New("nvidia-smi", fakeRunner{out: "34\n72\n"}.run)
	temps, err := r.Temperatures(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 2 || temps[1] != (Temp{Index: 1, Celsius: 72}) {
		t.Fatalf("temps = %+v", temps)
	}
	if m, ok := Max(temps); !ok || m != 72 {
		t.Errorf("Max = %d, %v; want 72, true", m, ok)
	}
}

func TestTemperaturesErrorPropagates(t *testing.T) {
	r := New("nvidia-smi", fakeRunner{err: fmt.Errorf("not found")}.run)
	if _, err := r.Temperatures(context.Background()); err == nil {
		t.Error("expected error when nvidia-smi fails")
	}
}

func TestTemperaturesUnparseable(t *testing.T) {
	r := New("nvidia-smi", fakeRunner{out: "N/A\n"}.run)
	if _, err := r.Temperatures(context.Background()); err == nil {
		t.Error("expected error on unparseable output")
	}
}

func TestTemperaturesEmpty(t *testing.T) {
	r := New("nvidia-smi", fakeRunner{out: "\n"}.run)
	if _, err := r.Temperatures(context.Background()); err == nil {
		t.Error("expected error on empty output")
	}
}
