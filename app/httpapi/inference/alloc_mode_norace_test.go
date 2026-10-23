//go:build !race

package inference

// raceInstrumented tells the allocation pins which comparison they may make.
// In a normal build the counts are exact and reproducible.
const raceInstrumented = false
