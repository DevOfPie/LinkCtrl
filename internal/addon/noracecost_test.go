//go:build !race

package addon

// raceDetector is false: this binary was built without `-race`, so a duration
// measured in it is one an instance would recognise. See racecost_test.go for why
// the distinction exists at all.
const raceDetector = false
