//go:build race

package logging_test

// raceDetectorAllocs is what the race detector's instrumentation adds to an
// allocation count measured in this package.
const raceDetectorAllocs = 2
