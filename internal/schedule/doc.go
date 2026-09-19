// Package schedule turns a piecewise-linear rate profile into exact arrival times.
//
// It computes the cumulative arrival function Lambda(t) and its inverse, so arrival k
// fires at Lambda-inverse(k) rather than after a drifting 1/rate sleep. Uniform and
// Poisson arrival processes share the same inverse via time rescaling. See docs/adr/001.
package schedule
