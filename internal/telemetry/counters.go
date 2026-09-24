package telemetry

import "time"

// counters turns monotonically increasing server counters into per-interval deltas.
//
// Servers report totals since they started; what matters is how much happened between
// two readings. A counter that went backwards means the server restarted or the
// statistics were reset, and that interval's delta is dropped rather than reported as a
// huge negative number.
type counters struct {
	prev map[string]float64
	at   time.Time
	have bool
}

// step records a new reading. It returns the deltas and the time between the two
// readings, and ok is false on the first reading, which only primes the baseline.
func (c *counters) step(now time.Time, current map[string]float64) (deltas map[string]float64, elapsed time.Duration, ok bool) {
	defer func() {
		c.prev, c.at, c.have = current, now, true
	}()
	if !c.have {
		return nil, 0, false
	}
	deltas = make(map[string]float64, len(current))
	for k, v := range current {
		p, seen := c.prev[k]
		if !seen || v < p {
			continue
		}
		deltas[k] = v - p
	}
	return deltas, now.Sub(c.at), true
}

// ratio is part / whole, or ok false when there was nothing to divide.
func ratio(part, whole float64) (float64, bool) {
	if whole <= 0 {
		return 0, false
	}
	return part / whole, true
}
