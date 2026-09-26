package result

import (
	"fmt"
	"strconv"
	"strings"
)

// Unit is how a knob value reads: requests a second, or users.
func (c *Capacity) Unit(v float64) string {
	n := strconv.FormatFloat(v, 'f', -1, 64)
	if c.Knob == "concurrency" {
		return n + " users"
	}
	return n + "/s"
}

// Summary is the search's outcome in one or two sentences, shared by every renderer so
// they cannot disagree.
func (c *Capacity) Summary() string {
	var b strings.Builder
	switch bd := c.Boundary; {
	case len(c.Levels) == 0:
		b.WriteString("The capacity search ran no level.")
	case bd == nil:
		b.WriteString("The capacity search found no boundary.")
	case bd.FirstBroken == 0:
		fmt.Fprintf(&b, "Capacity held at every level up to the ceiling of %s; nothing broke.", c.Unit(bd.LastOK))
	case !bd.Stable && len(bd.Range) == 2:
		fmt.Fprintf(&b, "Capacity runs out somewhere between %s and %s: a confirmation run flipped, so the boundary is a range, not a point.",
			c.Unit(bd.Range[0]), c.Unit(bd.Range[1]))
	case bd.LastOK == 0:
		fmt.Fprintf(&b, "Capacity broke at the first level, %s.", c.Unit(bd.FirstBroken))
	default:
		fmt.Fprintf(&b, "Capacity holds at %s and breaks at %s", c.Unit(bd.LastOK), c.Unit(bd.FirstBroken))
		if bd.Stable {
			b.WriteString(", confirmed by re-running both.")
		} else {
			b.WriteString(", unconfirmed.")
		}
	}
	if l := c.firstBroken(); l != nil {
		if l.Culprit != nil {
			fmt.Fprintf(&b, " At %s the %s tier broke first: %s.", c.Unit(l.Value), *l.Culprit, l.BreakReason)
		} else if l.BreakReason != "" {
			fmt.Fprintf(&b, " At %s: %s.", c.Unit(l.Value), l.BreakReason)
		}
	}
	if c.Knee != nil {
		fmt.Fprintf(&b, " Latency began climbing sharply at %s.", c.Unit(*c.Knee))
	}
	if u := c.USL; u != nil && u.Fitted && u.PeakN > 0 {
		fmt.Fprintf(&b, " The scalability fit (R² %.2f) predicts peak throughput of %.0f/s at a concurrency of %.0f.", u.R2, u.PeakThroughput, u.PeakN)
	}
	return b.String()
}

// firstBroken is the level at the boundary's broken side, as first run.
func (c *Capacity) firstBroken() *CapacityLevel {
	if c.Boundary == nil || c.Boundary.FirstBroken == 0 {
		return nil
	}
	for i := range c.Levels {
		if l := &c.Levels[i]; !l.OK && l.Value == c.Boundary.FirstBroken {
			return l
		}
	}
	return nil
}
