//go:build !race

package tui

// raceSlowdown scales time limits in tests: the race detector makes code
// several times slower.
const raceSlowdown = 1
