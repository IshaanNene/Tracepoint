package html

// maxPoints bounds each displayed series (§5.8). Thinning is for display only: the
// analysis always reads every bucket.
const maxPoints = 2000

// downsample thins aligned series to at most limit points by grouping consecutive
// points. Each group is placed at its first x and takes each series' maximum, so a
// spike one bucket wide is never averaged away - the failure this report exists to
// show. A group with no values stays a gap rather than becoming a zero. It returns
// the group size, which the report states.
func downsample(x []float64, ys [][]*float64, limit int) ([]float64, [][]*float64, int) {
	n := len(x)
	if limit < 1 {
		limit = 1
	}
	if n <= limit {
		return x, ys, 1
	}
	group := (n + limit - 1) / limit
	points := (n + group - 1) / group
	gx := make([]float64, 0, points)
	gys := make([][]*float64, len(ys))
	for s := range ys {
		gys[s] = make([]*float64, 0, points)
	}
	for start := 0; start < n; start += group {
		end := min(start+group, n)
		gx = append(gx, x[start])
		for s, y := range ys {
			var peak *float64
			for i := start; i < end && i < len(y); i++ {
				if y[i] != nil && (peak == nil || *y[i] > *peak) {
					v := *y[i]
					peak = &v
				}
			}
			gys[s] = append(gys[s], peak)
		}
	}
	return gx, gys, group
}
