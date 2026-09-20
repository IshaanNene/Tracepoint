// Package render holds TracePoint's renderers.
//
// Every renderer is a pure function of result.json: given the same document it
// produces byte-identical output, which is what makes golden-file tests meaningful and
// what lets `tracepoint report` reproduce exactly what a run printed.
package render
