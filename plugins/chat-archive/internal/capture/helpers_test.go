package capture_test

import (
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/fingerprint"
)

func canonTime() time.Time { return time.Date(2026, 11, 8, 1, 2, 3, 0, time.UTC) }

func canonExact(v string) [32]byte { return fingerprint.ExactFP(v) }
