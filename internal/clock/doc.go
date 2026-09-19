// Package clock defines the Clock abstraction used everywhere a wall or monotonic
// reading is needed, plus a real implementation and a fake for deterministic tests.
// Nothing in the engine reads time.Now directly.
package clock
