//go:build !nativefixture

package plugin

import "github.com/NoorChasib/cpa-plugins/plugins/token-usage/internal/usage"

// Production builds neither retain observations nor decorate status with them.
type fixtureRecorder struct{}

func (*fixtureRecorder) record(usage.Event)                       {}
func (*fixtureRecorder) addStatus(map[string]any, map[string]any) {}
