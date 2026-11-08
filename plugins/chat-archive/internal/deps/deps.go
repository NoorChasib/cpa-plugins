//go:build deps

// Blank imports of every module chat-archive pins up front. The deps build tag keeps
// them out of every real build; go mod tidy reads all build tags, so it keeps the requirements in go.mod
// and go.sum before any package imports them.
package deps

import (
	_ "github.com/BobuSumisu/aho-corasick" // pkg/mask prefilter
	_ "github.com/klauspost/compress/zstd" // commit stream and outbox blobs
	_ "github.com/mattn/go-sqlite3"        // plugin outbox, cgo
	_ "gopkg.in/yaml.v3"                   // plugin config loader
	_ "modernc.org/sqlite"                 // importer state, pure Go
)
