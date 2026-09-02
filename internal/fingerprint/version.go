// Package fingerprint classifies inbound client requests from their headers
// into candidate baseline observations and compares client versions.
//
// Everything here is pure: no I/O and no clocks. The interceptor hot path
// calls Classify on every model request, so the checks are bounded string and
// regexp operations.
package fingerprint

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Provider identifies which CPA header-defaults block a fingerprint targets.
type Provider string

// Managed providers.
const (
	ProviderClaude Provider = "claude"
	ProviderCodex  Provider = "codex"
)

// Version is a three-part client version.
type Version struct {
	Major int
	Minor int
	Patch int
}

var versionPattern = regexp.MustCompile(`^v?(\d{1,6})\.(\d{1,6})\.(\d{1,6})$`)

// ParseVersion parses "M.m.p" (an optional leading v is tolerated so
// operators can paste either form into config).
func ParseVersion(s string) (Version, error) {
	m := versionPattern.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return Version{}, fmt.Errorf("version %q is not M.m.p", s)
	}
	return newVersion(m[1], m[2], m[3])
}

func newVersion(major, minor, patch string) (Version, error) {
	a, errA := strconv.Atoi(major)
	b, errB := strconv.Atoi(minor)
	c, errC := strconv.Atoi(patch)
	if errA != nil || errB != nil || errC != nil {
		return Version{}, fmt.Errorf("version components must be integers")
	}
	return Version{Major: a, Minor: b, Patch: c}, nil
}

// MustParseVersion panics on malformed input; for compiled constants only.
func MustParseVersion(s string) Version {
	v, err := ParseVersion(s)
	if err != nil {
		panic(err)
	}
	return v
}

// String renders M.m.p.
func (v Version) String() string {
	return strconv.Itoa(v.Major) + "." + strconv.Itoa(v.Minor) + "." + strconv.Itoa(v.Patch)
}

// IsZero reports whether the version is unset.
func (v Version) IsZero() bool { return v.Major == 0 && v.Minor == 0 && v.Patch == 0 }

// Compare returns -1, 0, or 1.
func (v Version) Compare(o Version) int {
	switch {
	case v.Major != o.Major:
		return cmpInt(v.Major, o.Major)
	case v.Minor != o.Minor:
		return cmpInt(v.Minor, o.Minor)
	default:
		return cmpInt(v.Patch, o.Patch)
	}
}

// Newer reports v > o.
func (v Version) Newer(o Version) bool { return v.Compare(o) > 0 }

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// MarshalText renders the version as M.m.p for JSON status and state files.
func (v Version) MarshalText() ([]byte, error) { return []byte(v.String()), nil }

// UnmarshalText parses M.m.p; an empty value yields the zero version.
func (v *Version) UnmarshalText(text []byte) error {
	if strings.TrimSpace(string(text)) == "" {
		*v = Version{}
		return nil
	}
	parsed, err := ParseVersion(string(text))
	if err != nil {
		return err
	}
	*v = parsed
	return nil
}
