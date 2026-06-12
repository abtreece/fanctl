// Package fan holds the pure, side-effect-free fan-curve logic: given a
// temperature and the previously-applied level, decide the next fan level.
// Keeping this independent of IPMI makes the control policy unit-testable.
package fan

import "fmt"

// Band maps an inclusive upper temperature bound (°C) to a fan duty percent.
type Band struct {
	MaxTemp int
	Percent int
}

// Curve is an ascending set of bands plus downward hysteresis. A temperature
// above the hottest band hands control back to the BMC's own automatic mode,
// which is always the safe fallback.
type Curve struct {
	Bands      []Band
	Hysteresis int
}

// AutoLevel is the sentinel level meaning "hand back to BMC automatic control".
const AutoLevel = -1

// unsetLevel forces a fresh band selection on the first iteration.
const unsetLevel = -2

// Validate checks the curve is well-formed: at least one band, ascending
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
	return nil
}

// Initial is the seed value callers pass as prev on the first iteration so the
// curve selects the band matching the current temperature directly.
func Initial() int { return unsetLevel }

// lookup returns the band index whose ceiling first covers temp, or AutoLevel
// when temp exceeds every band.
func (c Curve) lookup(temp int) int {
	for i, b := range c.Bands {
		if temp <= b.MaxTemp {
			return i
		}
	}
	return AutoLevel
}

// Level returns the next fan level given the previously-applied level and the
// current temperature. Upward moves are immediate (responsiveness is safety);
// downward moves step a single band at a time and only once the temperature has
// fallen a full hysteresis margin below the lower band's ceiling, which stops
// the fans hunting at a boundary. Leaving BMC-auto also requires the hysteresis
// margin below the top band.
func (c Curve) Level(prev, temp int) int {
	last := len(c.Bands) - 1

	// First iteration or a corrupt prev: pick the band for temp directly.
	if prev < AutoLevel || prev > last {
		return c.lookup(temp)
	}

	// Currently handed off to BMC-auto because it was too hot: only reclaim
	// manual control once comfortably back under the top band.
	if prev == AutoLevel {
		if temp <= c.Bands[last].MaxTemp-c.Hysteresis {
			return c.lookup(temp)
		}
		return AutoLevel
	}

	// Too hot now: hand back to BMC-auto immediately.
	if temp > c.Bands[last].MaxTemp {
		return AutoLevel
	}

	// Raise: jump straight to the correct hotter band.
	if t := c.lookup(temp); t > prev {
		return t
	}

	// Lower: a single step, gated by hysteresis below the next band's ceiling.
	if prev > 0 && temp < c.Bands[prev-1].MaxTemp-c.Hysteresis {
		return prev - 1
	}
	return prev
}

// Percent returns the duty percent for a level. ok is false for AutoLevel,
// signalling the caller to hand control back to the BMC.
func (c Curve) Percent(level int) (pct int, ok bool) {
	if level < 0 || level > len(c.Bands)-1 {
		return 0, false
	}
	return c.Bands[level].Percent, true
}
