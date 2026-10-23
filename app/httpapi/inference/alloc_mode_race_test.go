//go:build race

package inference

// raceInstrumented tells the allocation pins which comparison they may make.
// sync.Pool drops items at random under the race detector, so a path that
// pools its state (the JSON claims decode) allocates a varying amount per run.
const raceInstrumented = true
