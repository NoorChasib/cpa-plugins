package importer

import (
	"syscall"
	"time"
)

// threadCPU returns the CPU time of the calling OS thread (RUSAGE_THREAD), for per-core timings on a
// loaded machine; mask-dry-run locks its goroutine to one thread while it measures.
func threadCPU() (time.Duration, bool) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(1 /* RUSAGE_THREAD */, &ru); err != nil {
		return 0, false
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano()), true
}
