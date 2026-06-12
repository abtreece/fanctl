package fan

import "time"

// boostCap limits the predictive temperature boost in °C so a sensor glitch
// cannot spike the fans to full duty.
const boostCap = 8.0

// slopeWindow is how much sample history the predictor keeps. Slope is
// computed across this window, so it spans a few polls at typical cadences.
const slopeWindow = 90 * time.Second

// minSlopeSpan is the minimum sample span needed to trust a slope; below this
// the predictor reports 0 rather than amplify single-poll noise.
const minSlopeSpan = 10 * time.Second

// Predictor tracks recent temperature samples for one source and estimates the
// rate of rise, so the controller can drive the curve off where the
// temperature is heading rather than where it is. The caller supplies
// timestamps, keeping the type deterministic and unit-testable.
type Predictor struct {
	samples []sample
}

type sample struct {
	at   time.Time
	temp float64
}

// Observe records a temperature reading and drops samples older than the
// slope window.
func (p *Predictor) Observe(at time.Time, temp float64) {
	p.samples = append(p.samples, sample{at, temp})
	cutoff := at.Add(-slopeWindow)
	i := 0
	for i < len(p.samples)-1 && p.samples[i].at.Before(cutoff) {
		i++
	}
	p.samples = p.samples[i:]
}

// Reset clears the history, e.g. after a config reload changes the curve.
func (p *Predictor) Reset() { p.samples = nil }

// SlopePerMin returns the temperature rate of change in °C/minute across the
// retained window, or 0 when there is not enough history.
func (p *Predictor) SlopePerMin() float64 {
	if len(p.samples) < 2 {
		return 0
	}
	first, last := p.samples[0], p.samples[len(p.samples)-1]
	span := last.at.Sub(first.at)
	if span < minSlopeSpan {
		return 0
	}
	return (last.temp - first.temp) / span.Minutes()
}

// Boost returns the °C to add to the current temperature when evaluating the
// curve: slope (°C/min) times the lookahead horizon (minutes), clamped to
// [0, boostCap]. Falling temperatures contribute no boost — the governor's
// deadband and step-down limit already smooth the way down.
func (p *Predictor) Boost(lookaheadMin float64) float64 {
	if lookaheadMin <= 0 {
		return 0
	}
	b := p.SlopePerMin() * lookaheadMin
	if b < 0 {
		return 0
	}
	if b > boostCap {
		return boostCap
	}
	return b
}
