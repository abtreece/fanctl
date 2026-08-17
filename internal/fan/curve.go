// Package fan holds the pure, side-effect-free fan-control policy: a curve
// that interpolates temperature to duty percent, a predictor that estimates
// temperature slope, and a governor that turns a duty target into the next
// commanded state. Keeping this independent of IPMI makes the policy
// unit-testable.
package fan

import (
	"fmt"
	"math"
)

// Band is a curve anchor: at MaxTemp °C the fans run at Percent duty. Duty is
// linearly interpolated between anchors.
type Band struct {
	MaxTemp int
	Percent int
}

// Curve is an ascending set of anchors. A temperature above the hottest anchor
// hands control back to the BMC's own automatic mode, which is always the safe
// fallback.
type Curve struct {
	Bands      []Band
	Hysteresis int
}

// Validate checks the curve is well-formed: at least one anchor, ascending
// temperatures, in-range percents, non-negative hysteresis.
func (c Curve) Validate() error {
	if len(c.Bands) == 0 {
		return fmt.Errorf("curve must have at least one band")
	}
	for i, b := range c.Bands {
		if b.Percent < 0 || b.Percent > 100 {
			return fmt.Errorf("band %d: percent %d out of range 0-100", i, b.Percent)
		}
		if i > 0 && b.MaxTemp <= c.Bands[i-1].MaxTemp {
			return fmt.Errorf("band %d: max_temp %d must be greater than previous band's %d", i, b.MaxTemp, c.Bands[i-1].MaxTemp)
		}
	}
	if c.Hysteresis < 0 {
		return fmt.Errorf("hysteresis must be >= 0")
	}
	if top := c.Bands[len(c.Bands)-1].MaxTemp; c.Hysteresis >= top {
		return fmt.Errorf("hysteresis %d must be less than the top anchor's max_temp %d, or manual control could never be reclaimed from BMC automatic mode", c.Hysteresis, top)
	}
	return nil
}

// Duty returns the interpolated duty percent for temp. Below the first anchor
// it returns the first anchor's percent; between anchors it interpolates
// linearly; above the last anchor it returns auto=true, signalling handoff to
// BMC automatic control.
func (c Curve) Duty(temp float64) (pct float64, auto bool) {
	last := len(c.Bands) - 1
	if temp > float64(c.Bands[last].MaxTemp) {
		return 0, true
	}
	if temp <= float64(c.Bands[0].MaxTemp) {
		return float64(c.Bands[0].Percent), false
	}
	for i := 1; i <= last; i++ {
		hi := c.Bands[i]
		if temp > float64(hi.MaxTemp) {
			continue
		}
		lo := c.Bands[i-1]
		frac := (temp - float64(lo.MaxTemp)) / float64(hi.MaxTemp-lo.MaxTemp)
		return float64(lo.Percent) + frac*float64(hi.Percent-lo.Percent), false
	}
	return float64(c.Bands[last].Percent), false
}

// ReclaimBelow is the temperature at or below which manual control may be
// reclaimed from BMC automatic mode: a full hysteresis margin under the top
// anchor, so the controller does not flap at the handoff boundary.
func (c Curve) ReclaimBelow() float64 {
	return float64(c.Bands[len(c.Bands)-1].MaxTemp - c.Hysteresis)
}

// State is the governor's commanded fan state.
type State struct {
	Pct  int  // commanded duty percent; meaningless when Auto
	Auto bool // control handed to the BMC's automatic mode
	Init bool // false until the first decision seeds the state
	// Hold counts consecutive decisions a sub-deadband decrease has been held.
	// It is bookkeeping, not commanded output: callers deciding whether to
	// write to the BMC must compare with SameCommand, not ==, or every held
	// poll looks like a change and produces the very churn the deadband exists
	// to prevent.
	Hold int
}

// SameCommand reports whether two states would command the same thing of the
// BMC, ignoring bookkeeping like Hold.
func (s State) SameCommand(o State) bool {
	return s.Pct == o.Pct && s.Auto == o.Auto && s.Init == o.Init
}

// Governor turns a duty target into the next commanded state. It is pure: all
// state lives in the State value the caller threads through.
type Governor struct {
	// Deadband is the minimum percent-point decrease before the duty is
	// lowered; small downward wobble is held to avoid hunting and IPMI churn.
	// Increases are never gated: responsiveness upward is safety.
	Deadband int
	// MaxStepDown caps the percent-point decrease per decision so the fans
	// spin down smoothly after a load drops. 0 means uncapped.
	MaxStepDown int
	// DeadbandHoldPolls is how many consecutive decisions a sub-deadband
	// decrease is held before it is applied anyway.
	//
	// Without it the deadband does not delay a small decrease, it cancels one
	// forever. Below the curve's lowest anchor the curve is flat, so the target
	// is pinned at the floor and the gap to a duty sitting 1-2 points above it
	// can never grow to Deadband. A host that arrives at 7% with a 6% floor
	// stays at 7% indefinitely, never reaching the duty its curve was tuned
	// for. Holding for a few polls keeps the anti-hunting property — a target
	// genuinely oscillating across a band boundary resets the count and still
	// holds — while letting a settled temperature converge.
	//
	// 0 restores the old cancel-forever behaviour.
	DeadbandHoldPolls int
}

// Next computes the next state. target/wantAuto come from Curve.Duty (after
// any predictive boost); canReclaim reports whether every temperature source
// is back under its curve's ReclaimBelow threshold.
func (g Governor) Next(s State, target float64, wantAuto, canReclaim bool) State {
	pct := clampPct(int(math.Round(target)))

	if !s.Init {
		return State{Pct: pct, Auto: wantAuto, Init: true}
	}
	if s.Auto {
		if wantAuto || !canReclaim {
			return s
		}
		return State{Pct: pct, Auto: false, Init: true}
	}
	if wantAuto {
		return State{Auto: true, Init: true}
	}
	if pct > s.Pct {
		// Upward moves are never gated: responsiveness upward is safety.
		return State{Pct: pct, Auto: false, Init: true}
	}
	if pct == s.Pct {
		// The decrease went away; nothing is being held any more.
		s.Hold = 0
		return s
	}
	if s.Pct-pct < g.Deadband {
		// Hold the small decrease, but count it. Once it has persisted across
		// DeadbandHoldPolls decisions it is a settled target, not wobble, and
		// falls through to be applied.
		if hold := s.Hold + 1; g.DeadbandHoldPolls <= 0 || hold < g.DeadbandHoldPolls {
			s.Hold = hold
			return s
		}
	}
	if g.MaxStepDown > 0 && s.Pct-pct > g.MaxStepDown {
		pct = s.Pct - g.MaxStepDown
	}
	return State{Pct: pct, Auto: false, Init: true}
}

func clampPct(p int) int {
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return p
}
