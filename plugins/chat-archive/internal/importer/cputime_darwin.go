package importer

import "time"

// threadCPU is unavailable on darwin (no RUSAGE_THREAD); mask-dry-run uses wall time there.
func threadCPU() (time.Duration, bool) { return 0, false }
