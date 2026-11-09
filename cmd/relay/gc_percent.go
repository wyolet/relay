package main

import (
	"os"
	"runtime/debug"
	"strconv"
)

// defaultGCPercent replaces Go's GOGC=100 when GOGC is unset. Relay's live heap is small and its request path allocates fast, so letting the heap grow to 5× live between collections cut p99 overhead by up to half and cores per request by 9–16 % on 16 and 32 cores. GOMEMLIMIT still bounds the heap.
const defaultGCPercent = 400

// applyGCPercent runs after the env file loads: the runtime read GOGC from the process environment only, so a value from the file is applied here.
func applyGCPercent() {
	if percent, ok := gcPercent(os.LookupEnv("GOGC")); ok {
		debug.SetGCPercent(percent)
	}
}

func gcPercent(value string, set bool) (int, bool) {
	if !set {
		return defaultGCPercent, true
	}
	if value == "off" {
		return -1, true
	}
	n, err := strconv.Atoi(value)
	return n, err == nil
}
