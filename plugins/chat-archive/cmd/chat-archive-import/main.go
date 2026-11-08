// Command chat-archive-import is the chat archive's history importer. It is pure Go and
// builds with CGO_ENABLED=0 for linux/amd64 and darwin/arm64. See docs/importer.md.
package main

import (
	"os"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/importer"
)

func main() {
	os.Exit(importer.Main(os.Args[1:], importer.DefaultEnv()))
}
