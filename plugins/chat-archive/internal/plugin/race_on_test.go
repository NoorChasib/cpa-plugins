//go:build race

package plugin_test

// raceEnabled: the race detector slows the workers' split of large bodies several times over, so
// throughput-bound loads are scaled down under -race.
const raceEnabled = true
