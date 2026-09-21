//go:build race

package policy

// raceInstrumented tells the allocation gate which comparison it may make.
// The race detector's shadow-memory bookkeeping allocates a varying amount
// per run (measured 63/64/63 for the same call), so under -race the gate
// checks a ceiling instead of an equality — still enough to catch a real
// regression, and the rest of the test's assertions keep running.
const raceInstrumented = true
