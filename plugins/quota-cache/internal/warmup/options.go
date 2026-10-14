// Package warmup primes idle Claude subscription windows with the unmodified
// Claude Code CLI. It never reads, copies or routes subscription OAuth tokens.
package warmup

import (
	"errors"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // CPA containers need not install a system timezone database.
)

const DefaultModel = "claude-haiku-5-5"

type Options struct {
	Enabled                                              bool
	Model, Effort, Hours, Timezone, CLIPath, ProfileRoot string
	Spacing                                              time.Duration
	location                                             *time.Location
	start, end                                           int
}

func Defaults(snapshotPath string) Options {
	return Options{Model: DefaultModel, Effort: "low", Hours: "00:00-24:00", Timezone: "UTC", CLIPath: "claude", ProfileRoot: filepath.Join(filepath.Dir(snapshotPath), "claude-profiles"), Spacing: 10 * time.Second}
}

var modelName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)
var authIndex = regexp.MustCompile(`^[0-9a-f]{16}$`)

func (o Options) Validate() (Options, error) {
	invalid := errors.New("invalid warm-up model, effort, hours, timezone or runner path")
	if !modelName.MatchString(o.Model) || o.CLIPath == "" || o.ProfileRoot == "" || o.Spacing < time.Second {
		return o, invalid
	}
	switch o.Effort {
	case "low", "medium", "high", "xhigh", "max":
	default:
		return o, invalid
	}
	parts := strings.Split(o.Hours, "-")
	if len(parts) != 2 {
		return o, invalid
	}
	minute := func(value string, end bool) (int, bool) {
		if end && value == "24:00" {
			return 1440, true
		}
		if len(value) != 5 || value[2] != ':' {
			return 0, false
		}
		h, e1 := strconv.Atoi(value[:2])
		m, e2 := strconv.Atoi(value[3:])
		return h*60 + m, e1 == nil && e2 == nil && h >= 0 && h < 24 && m >= 0 && m < 60
	}
	var a, b bool
	o.start, a = minute(parts[0], false)
	o.end, b = minute(parts[1], true)
	var err error
	o.location, err = time.LoadLocation(o.Timezone)
	if !a || !b || o.start == o.end || err != nil {
		return o, invalid
	}
	o.ProfileRoot, err = filepath.Abs(o.ProfileRoot)
	if err != nil {
		return o, invalid
	}
	if strings.ContainsRune(o.CLIPath, filepath.Separator) {
		o.CLIPath, err = filepath.Abs(o.CLIPath)
		if err != nil {
			return o, invalid
		}
	}
	return o, nil
}

func (o Options) withinHours(now time.Time) bool {
	local := now.In(o.location)
	minute := local.Hour()*60 + local.Minute()
	if o.start < o.end {
		return minute >= o.start && minute < o.end
	}
	return minute >= o.start || minute < o.end
}
