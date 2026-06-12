package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/abtreece/fanctl/internal/ipmi"
)

// scriptedFans returns a different fan set on each successive Fans() call,
// simulating the spin-down between baseline and post-command reads.
type scriptedFans struct {
	reads    [][]ipmi.Fan
	idx      int
	autoSet  bool
	manual   int
	setCalls int
}

func (s *scriptedFans) Fans(context.Context) ([]ipmi.Fan, error) {
	r := s.reads[min(s.idx, len(s.reads)-1)]
	s.idx++
	return r, nil
}
func (s *scriptedFans) SetManual(context.Context) error           { s.setCalls++; return nil }
func (s *scriptedFans) SetPercent(_ context.Context, p int) error { s.manual = p; return nil }
func (s *scriptedFans) SetAuto(context.Context) error             { s.autoSet = true; return nil }

func fansAt(rpm int) []ipmi.Fan {
	return []ipmi.Fan{{ID: "30h", Name: "Fan1", RPM: rpm}, {ID: "31h", Name: "Fan2", RPM: rpm}}
}

func TestProbeDetectsWorkingControl(t *testing.T) {
	sf := &scriptedFans{reads: [][]ipmi.Fan{fansAt(6200), fansAt(3050)}}
	var out bytes.Buffer
	code := probe(&out, &out, sf, probeOptions{low: 10, settle: time.Millisecond, minDropPct: 20, restore: true})
	if code != 0 {
		t.Fatalf("probe exit = %d, want 0\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "WORKS") {
		t.Errorf("expected WORKS verdict, got:\n%s", out.String())
	}
	if !sf.autoSet {
		t.Error("probe should restore auto when restore=true")
	}
}

func TestProbeDetectsIgnoredControl(t *testing.T) {
	// Baseline ~6200, barely moves to 6100 -> control ignored.
	sf := &scriptedFans{reads: [][]ipmi.Fan{fansAt(6200), fansAt(6100)}}
	var out bytes.Buffer
	code := probe(&out, &out, sf, probeOptions{low: 10, settle: time.Millisecond, minDropPct: 20, restore: false})
	if code != 1 {
		t.Fatalf("probe exit = %d, want 1\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "IGNORED") {
		t.Errorf("expected IGNORED verdict, got:\n%s", out.String())
	}
}
