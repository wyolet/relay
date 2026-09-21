//go:build !race

package policy

// raceInstrumented tells the allocation gate which comparison it may make.
// In a normal build the counts are exact and reproducible, so the pin is an
// equality.
const raceInstrumented = false
