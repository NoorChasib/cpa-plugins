package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// Placeholder ids and auth indexes, 16 lower-case hex digits each.
const (
	refreshAuthA = "0123456789abcdef"
	refreshAuthB = "fedcba9876543210"
)

var refreshNow = time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)

// oversizeRefresh is a valid file but for its size, one byte over the limit
// and more.
var oversizeRefresh = `{"schema":1,"requests":[],"pad":"` + strings.Repeat("x", MaxRefreshRequestBytes) + `"}`

func refreshRequest(id string, at time.Time) RefreshRequest {
	return RefreshRequest{ID: id, Provider: "codex", AuthIndex: refreshAuthA, Reason: RefreshReasonReset, At: at}
}

// refreshID16 is a placeholder request id for n.
func refreshID16(n int) string { return fmt.Sprintf("%016x", n) }

func writeRefresh(t *testing.T, path string, value any) {
	t.Helper()
	raw, ok := value.(string)
	if !ok {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		raw = string(encoded)
	}
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

func loadRefresh(t *testing.T, path string) []RefreshRequest {
	t.Helper()
	requests, err := LoadRefreshRequests(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return requests
}

// The names and limits are the contract between Quota Glance, which writes
// the file, and quota-cache, which reads it, at their own client tags.
func TestRefreshConstantsAndPath(t *testing.T) {
	if RefreshRequestSchema != 1 || MaxRefreshRequestBytes != 4096 || MaxRefreshRequests != 8 || RefreshRequestMaxAge != 10*time.Minute || RefreshReasonReset != "reset" {
		t.Fatal("refresh request constants changed")
	}
	for in, want := range map[string]string{
		DefaultPath:              "plugins/data/quota-cache/snapshot.refresh.json",
		"/data/cache/state.json": "/data/cache/state.refresh.json",
		"/data/cache/state":      "/data/cache/state.refresh.json",
	} {
		if got := RefreshRequestPath(in); got != want {
			t.Errorf("RefreshRequestPath(%q) = %q, want %q", in, got, want)
		}
	}
	raw, err := json.Marshal(refreshRequest(refreshID16(1), refreshNow))
	if err != nil || string(raw) != `{"id":"0000000000000001","provider":"codex","auth_index":"0123456789abcdef","reason":"reset","at":"2026-10-12T09:00:00Z"}` {
		t.Fatalf("wire form %s %v", raw, err)
	}
}

// Quota Glance appends with an empty id and a zero time: the id is random and
// the time is now, in UTC, whole seconds. The file is the documented form,
// at 0600, and holds nothing else.
func TestAppendRefreshRequestWritesTheDocumentedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.refresh.json")
	now := time.Date(2026, 10, 12, 11, 0, 0, 123456789, time.FixedZone("x", 3600))
	if err := AppendRefreshRequest(path, RefreshRequest{Provider: "claude", AuthIndex: refreshAuthB, Reason: RefreshReasonReset}, now); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v", info, err)
	}
	raw, _ := os.ReadFile(path)
	pattern := `^\{"schema":1,"requests":\[\{"id":"[0-9a-f]{16}","provider":"claude","auth_index":"fedcba9876543210","reason":"reset","at":"2026-10-12T10:00:00Z"\}\]\}$`
	if !regexp.MustCompile(pattern).Match(raw) {
		t.Fatalf("file %s", raw)
	}
	requests := loadRefresh(t, path)
	if len(requests) != 1 || !requests[0].At.Equal(now.Truncate(time.Second)) {
		t.Fatalf("loaded %+v", requests)
	}
	// Two appends never share an id.
	if err := AppendRefreshRequest(path, RefreshRequest{Provider: "claude", AuthIndex: refreshAuthB, Reason: RefreshReasonReset}, now); err != nil {
		t.Fatal(err)
	}
	if requests = loadRefresh(t, path); len(requests) != 2 || requests[0].ID == requests[1].ID {
		t.Fatalf("loaded %+v", requests)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("directory holds %v; want only the file", entries)
	}
}

// Requests no longer live are dropped, either way, and of the rest the newest
// seven stay beside the new one, which is always kept, however new the others
// claim to be. The file stays far under its limit.
func TestAppendRefreshRequestPrunesByAgeAndCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.refresh.json")
	var seeded []RefreshRequest
	seeded = append(seeded,
		refreshRequest(refreshID16(100), refreshNow.Add(-RefreshRequestMaxAge-time.Second)),
		refreshRequest(refreshID16(101), refreshNow.Add(RefreshRequestMaxAge+time.Second)),
	)
	for i := range 9 {
		// Dated just ahead of now, so each is newer than the request added.
		seeded = append(seeded, refreshRequest(refreshID16(i), refreshNow.Add(time.Duration(i+1)*time.Second)))
	}
	writeRefresh(t, path, map[string]any{"schema": 1, "requests": seeded})
	added := refreshRequest(refreshID16(200), refreshNow)
	if err := AppendRefreshRequest(path, added, refreshNow); err != nil {
		t.Fatal(err)
	}
	requests := loadRefresh(t, path)
	var ids []string
	for _, r := range requests {
		ids = append(ids, r.ID)
	}
	want := []string{refreshID16(200)}
	for i := 2; i < 9; i++ {
		want = append(want, refreshID16(i))
	}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("kept %v\nwant %v", ids, want)
	}
	// However often it is written, the file stays bounded.
	for i := range 40 {
		if err := AppendRefreshRequest(path, RefreshRequest{Provider: "codex", AuthIndex: refreshAuthA, Reason: RefreshReasonReset}, refreshNow.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	info, _ := os.Stat(path)
	if requests = loadRefresh(t, path); len(requests) != MaxRefreshRequests || info.Size() > 1024 {
		t.Fatalf("%d requests in %d bytes", len(requests), info.Size())
	}
}

// A request with the id of one already in the file replaces it.
func TestAppendRefreshRequestReplacesTheSameID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.refresh.json")
	for _, at := range []time.Time{refreshNow.Add(-time.Minute), refreshNow} {
		if err := AppendRefreshRequest(path, refreshRequest(refreshID16(7), at), refreshNow); err != nil {
			t.Fatal(err)
		}
	}
	if requests := loadRefresh(t, path); len(requests) != 1 || !requests[0].At.Equal(refreshNow) {
		t.Fatalf("loaded %+v", requests)
	}
}

// quota-cache creates its directory. Without it nothing is created, and the
// error names no path, so the caller may log it.
func TestAppendRefreshRequestNeverCreatesTheDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "quota-cache")
	path := filepath.Join(dir, "snapshot.refresh.json")
	err := AppendRefreshRequest(path, refreshRequest("", time.Time{}), refreshNow)
	if err == nil || strings.Contains(err.Error(), dir) || strings.Contains(err.Error(), "refresh.json") {
		t.Fatalf("error %v", err)
	}
	if _, statErr := os.Lstat(dir); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("the directory was created: %v", statErr)
	}
}

// A write that fails, here because the directory is read-only, leaves the
// file as it was.
func TestAppendRefreshRequestIsAtomic(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.refresh.json")
	if err := AppendRefreshRequest(path, refreshRequest(refreshID16(1), refreshNow), refreshNow); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if err := AppendRefreshRequest(path, refreshRequest(refreshID16(2), refreshNow), refreshNow); err == nil {
		t.Fatal("a write into a read-only directory succeeded")
	}
	after, _ := os.ReadFile(path)
	entries, _ := os.ReadDir(dir)
	if string(after) != string(before) || len(entries) != 1 {
		t.Fatalf("after a failed write: %s, %v", after, entries)
	}
}

// A write that fails after its temporary file was made, here because a
// directory stands where the file goes and the rename fails, leaves no
// temporary file behind.
func TestAppendRefreshRequestLeavesNoTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.refresh.json")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := AppendRefreshRequest(path, refreshRequest(refreshID16(1), refreshNow), refreshNow); err == nil {
		t.Fatal("a write over a directory succeeded")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != filepath.Base(path) || !entries[0].IsDir() {
		t.Fatalf("after a failed write the directory holds %v", entries)
	}
}

// A file LoadRefreshRequests cannot read counts as empty, and is replaced: a
// symbolic link itself, never what it points at.
func TestAppendRefreshRequestReplacesAnUnreadableFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.refresh.json")
	elsewhere := filepath.Join(dir, "elsewhere.json")
	writeRefresh(t, elsewhere, `{"schema":1,"requests":[]}`)
	for name, setup := range map[string]func(){
		"not JSON":     func() { writeRefresh(t, path, "not json") },
		"other schema": func() { writeRefresh(t, path, `{"schema":2,"requests":[]}`) },
		"too large":    func() { writeRefresh(t, path, oversizeRefresh) },
		"symlink":      func() { _ = os.Symlink(elsewhere, path) },
	} {
		t.Run(name, func(t *testing.T) {
			_ = os.Remove(path)
			setup()
			if err := AppendRefreshRequest(path, refreshRequest(refreshID16(9), refreshNow), refreshNow); err != nil {
				t.Fatal(err)
			}
			if requests := loadRefresh(t, path); len(requests) != 1 || requests[0].ID != refreshID16(9) {
				t.Fatalf("loaded %+v", requests)
			}
		})
	}
	if raw, _ := os.ReadFile(elsewhere); string(raw) != `{"schema":1,"requests":[]}` {
		t.Fatalf("the link's target was written: %s", raw)
	}
}

// A request LoadRefreshRequests would skip is not written.
func TestAppendRefreshRequestRefusesAnInvalidRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.refresh.json")
	for name, request := range map[string]RefreshRequest{
		"provider":   {Provider: "xai", AuthIndex: refreshAuthA, Reason: RefreshReasonReset},
		"auth index": {Provider: "codex", AuthIndex: "claude-one.json", Reason: RefreshReasonReset},
		"reason":     {Provider: "codex", AuthIndex: refreshAuthA, Reason: "manual"},
		"id":         {ID: "ABCDEF0123456789", Provider: "codex", AuthIndex: refreshAuthA, Reason: RefreshReasonReset},
	} {
		if err := AppendRefreshRequest(path, request, refreshNow); !errors.Is(err, ErrRefreshRequestInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an invalid request wrote the file: %v", err)
	}
}

// Appends from one process never lose one another's request.
func TestAppendRefreshRequestConcurrently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.refresh.json")
	var wg sync.WaitGroup
	for range MaxRefreshRequests {
		wg.Go(func() {
			if err := AppendRefreshRequest(path, RefreshRequest{Provider: "codex", AuthIndex: refreshAuthA, Reason: RefreshReasonReset}, refreshNow); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if requests := loadRefresh(t, path); len(requests) != MaxRefreshRequests {
		t.Fatalf("%d requests kept; want %d", len(requests), MaxRefreshRequests)
	}
}

// Every rule of the reader. A missing file holds no requests and is no error;
// a file that fails a rule holds none and is ErrUnavailable; a request that
// fails one is skipped on its own.
func TestLoadRefreshRequests(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.refresh.json")
	if requests, err := LoadRefreshRequests(path); err != nil || requests == nil || len(requests) != 0 {
		t.Fatalf("missing: %v %v", requests, err)
	}
	unreadable := map[string]func(){
		"directory": func() { _ = os.Mkdir(path, 0o700) },
		"symlink": func() {
			writeRefresh(t, path+".target", `{"schema":1,"requests":[]}`)
			_ = os.Symlink(path+".target", path)
		},
		"not JSON":      func() { writeRefresh(t, path, `{"schema":1,`) },
		"schema 2":      func() { writeRefresh(t, path, `{"schema":2,"requests":[]}`) },
		"no schema":     func() { writeRefresh(t, path, `{"requests":[]}`) },
		"not a list":    func() { writeRefresh(t, path, `{"schema":1,"requests":{}}`) },
		"over 4 KiB":    func() { writeRefresh(t, path, oversizeRefresh) },
		"not an object": func() { writeRefresh(t, path, `[]`) },
	}
	for name, setup := range unreadable {
		t.Run(name, func(t *testing.T) {
			_ = os.RemoveAll(path)
			_ = os.Remove(path + ".target")
			setup()
			if requests, err := LoadRefreshRequests(path); !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrRefreshNotRead) || requests == nil || len(requests) != 0 {
				t.Fatalf("%v %v", requests, err)
			}
		})
	}
	_ = os.RemoveAll(path)
	// Exactly 4 KiB is read.
	pad := `{"schema":1,"requests":[],"pad":"`
	writeRefresh(t, path, pad+strings.Repeat("x", MaxRefreshRequestBytes-len(pad)-2)+`"}`)
	if requests := loadRefresh(t, path); len(requests) != 0 {
		t.Fatalf("4 KiB: %v", requests)
	}
	writeRefresh(t, path, `{"schema":1}`)
	if requests := loadRefresh(t, path); len(requests) != 0 {
		t.Fatalf("no requests: %v", requests)
	}

	valid := func(id, at string) string {
		return `{"id":"` + id + `","provider":"claude","auth_index":"` + refreshAuthB + `","reason":"reset","at":"` + at + `"}`
	}
	writeRefresh(t, path, `{"schema":1,"requests":[`+strings.Join([]string{
		valid(refreshID16(3), "2026-10-12T09:03:00Z"),
		valid(refreshID16(1), "2026-10-12T11:01:00+02:00"),
		// Skipped, each for one rule.
		valid("0123456789ABCDEF", "2026-10-12T09:00:00Z"),
		valid("0123456789abcde", "2026-10-12T09:00:00Z"),
		`{"id":"` + refreshID16(4) + `","provider":"xai","auth_index":"` + refreshAuthB + `","reason":"reset","at":"2026-10-12T09:00:00Z"}`,
		`{"id":"` + refreshID16(5) + `","provider":"Codex","auth_index":"` + refreshAuthB + `","reason":"reset","at":"2026-10-12T09:00:00Z"}`,
		`{"id":"` + refreshID16(6) + `","provider":"codex","auth_index":"claude-one.json","reason":"reset","at":"2026-10-12T09:00:00Z"}`,
		`{"id":"` + refreshID16(7) + `","provider":"codex","auth_index":"` + refreshAuthB + `","reason":"manual","at":"2026-10-12T09:00:00Z"}`,
		`{"id":"` + refreshID16(8) + `","provider":"codex","auth_index":"` + refreshAuthB + `","reason":"reset"}`,
		valid(refreshID16(9), "2026-10-12 09:00:00"),
		`{"id":"` + refreshID16(10) + `","provider":"codex","auth_index":"` + refreshAuthB + `","reason":"reset","at":1791795600}`,
		`"` + refreshID16(11) + `"`,
		`null`,
		// A repeated id counts once.
		valid(refreshID16(3), "2026-10-12T09:05:00Z"),
		// A field this reader does not know is ignored.
		`{"id":"` + refreshID16(2) + `","provider":"codex","auth_index":"` + refreshAuthA + `","reason":"reset","at":"2026-10-12T09:02:00Z","note":"later"}`,
	}, ",")+`]}`)
	requests := loadRefresh(t, path)
	var got []string
	for _, r := range requests {
		got = append(got, r.ID+" "+r.Provider+" "+r.At.UTC().Format(time.RFC3339))
	}
	want := []string{
		refreshID16(1) + " claude 2026-10-12T09:01:00Z",
		refreshID16(2) + " codex 2026-10-12T09:02:00Z",
		refreshID16(3) + " claude 2026-10-12T09:03:00Z",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("loaded, oldest first:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A file that is there but cannot be opened, here because its mode lets no
// one read it, holds no requests, and its error says it was not read, so a
// reader can tell it from one that fails a rule and try again. A chmod
// changes none of the file's size, modification time or inode.
func TestLoadRefreshRequestsTellsAFileNotRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens a file whatever its mode")
	}
	path := filepath.Join(t.TempDir(), "snapshot.refresh.json")
	if err := AppendRefreshRequest(path, refreshRequest(refreshID16(1), refreshNow), refreshNow); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	requests, err := LoadRefreshRequests(path)
	if !errors.Is(err, ErrUnavailable) || !errors.Is(err, ErrRefreshNotRead) || requests == nil || len(requests) != 0 {
		t.Fatalf("%v %v", requests, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if requests := loadRefresh(t, path); len(requests) != 1 {
		t.Fatalf("readable again: %v", requests)
	}
}

func TestRefreshRequestLive(t *testing.T) {
	for offset, live := range map[time.Duration]bool{
		0: true, -RefreshRequestMaxAge: true, RefreshRequestMaxAge: true,
		-RefreshRequestMaxAge - time.Second: false, RefreshRequestMaxAge + time.Second: false,
	} {
		if got := refreshRequest(refreshID16(1), refreshNow.Add(offset)).Live(refreshNow); got != live {
			t.Errorf("dated %v from now: live %v, want %v", offset, got, live)
		}
	}
}
