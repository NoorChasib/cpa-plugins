package client_test

// Cross-check of the protocol v1 goldens in pkg/wire/testdata/v1 against pkg/canon. It lives here,
// not in pkg/wire, because canon imports wire: pkg/wire's tests must build without canon, while
// pkg/client depends on canon anyway (tombstones are encoded with canon.Marshal).

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/canon"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

const wireGoldens = "../wire/testdata/v1"

func TestWireGoldensAreCanon(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(wireGoldens, "envelopes", "*.json"))
	if err != nil || len(files) < 8 {
		t.Fatalf("goldens: %v (%d files)", err, len(files))
	}
	files = append(files, filepath.Join(wireGoldens, "tombstone.json"), filepath.Join(wireGoldens, "heartbeat.json"))
	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			out, flags, err := canon.Canonicalize(b)
			if err != nil || len(flags) != 0 || !bytes.Equal(out, b) {
				t.Fatalf("not canon/v1 (flags %v, err %v)", flags, err)
			}
			if !strings.Contains(path, "envelopes") || filepath.Base(path) == "manifest.json" {
				return
			}
			env, err := wire.DecodeEnvelope(b)
			if err != nil {
				t.Fatal(err)
			}
			body, err := wire.DecodeBody(env.Kind, env.Body)
			if err != nil {
				t.Fatal(err)
			}
			// The body is canon.Marshal of its struct, byte for byte.
			again, err := canon.Marshal(body)
			if err != nil || !bytes.Equal(again, env.Body) {
				t.Errorf("canon.Marshal(body) differs from the golden body (err %v)\n got %s\nwant %s", err, again, env.Body)
			}
			switch v := body.(type) {
			case *wire.ExchangeBegin:
				checkListSHA(t, v.Body)
			case *wire.ExchangeResponse:
				checkListSHA(t, v.Body)
			case *wire.ImportRecord:
				// The key's last segment is RecordKeyHash over canon.ItemIDs.
				sha := wire.RecordKeyHash(v.Frame, canon.ItemIDs(v.Items), v.Extra)
				if want := wire.ClaudeUUIDKey(v.SessionID, v.RecordUUID, sha); env.Key != want {
					t.Errorf("key %s, want %s", env.Key, want)
				}
			}
		})
	}
	// Tombstones are encoded with canon.Marshal by Commit; the golden is that encoding.
	b, err := os.ReadFile(filepath.Join(wireGoldens, "tombstone.json"))
	if err != nil {
		t.Fatal(err)
	}
	var tomb wire.Tombstone
	if err := json.Unmarshal(b, &tomb); err != nil {
		t.Fatal(err)
	}
	if out, err := canon.Marshal(tomb); err != nil || !bytes.Equal(out, b) {
		t.Errorf("canon.Marshal(tombstone) = %s, %v", out, err)
	}
}

// checkListSHA verifies n and list_sha of a body without a prefix-delta base.
func checkListSHA(t *testing.T, b wire.Body) {
	t.Helper()
	if b.Base != nil {
		return // S(base) is not in this envelope
	}
	ids := canon.ItemIDs(b.Items)
	if uint32(len(ids)) != b.N || canon.ListSHA(ids) != b.ListSHA {
		t.Errorf("n %d / list_sha %s do not match canon over %d items", b.N, b.ListSHA, len(ids))
	}
}
