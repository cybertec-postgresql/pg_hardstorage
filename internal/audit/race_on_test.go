//go:build race

package audit

// raceDetector reports whether the test binary was built with -race.
const raceDetector = true
