// cpa-plugin-auto-baseline is a CLIProxyAPI native plugin that learns the
// newest authentic Claude Code / Codex CLI client fingerprint from inbound
// requests and promotes it into config.yaml's claude-header-defaults /
// codex-header-defaults so CPA's hot reload raises its measured baseline.
//
// Build as a CPA-loadable shared library (the exported ABI entrypoint lives
// in abi.go and requires CGO):
//
//	CGO_ENABLED=1 go build -buildmode=c-shared -o auto-baseline.so .
//
// The library basename defines the plugin ID: auto-baseline.
package main

// The embedded tzdata copy lets display-timezone resolve IANA zone names
// even when the host container image ships without /usr/share/zoneinfo.
import _ "time/tzdata"

// main is required by buildmode=c-shared; the host never calls it.
func main() {}
