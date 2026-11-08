//go:build race

package stream

// raceEnabled: the race detector slows JSON decoding about tenfold, so wall-clock bounds skip.
const raceEnabled = true
