package outbox

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestOpenRequestsSaveTake: a save replaces the saved set, a take returns it once and deletes it,
// and a set older than the take's max age is deleted without being returned. The set survives a
// close and reopen (the next plugin instance opens the outbox again).
func TestOpenRequestsSaveTake(t *testing.T) {
	ctx := context.Background()
	c := &clock{t: time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)}
	dir := filepath.Join(t.TempDir(), "data")
	open := func() *Outbox {
		o, err := Open(Options{Dir: dir, CapBytes: 1 << 40, Retention: 168 * time.Hour, Now: c.now})
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	o := open()
	touched := c.t.Add(-time.Minute)
	reqs := []OpenRequest{
		{RequestID: "req-a", NextInputSeq: 2, NextOrdinal: 3, NextResponse: 2, Responses: 2, BeforeAuth: true, BeginWritten: true, TouchedAt: touched},
		{RequestID: "req-b", NextResponse: 1, TouchedAt: touched},
		{RequestID: "", NextResponse: 9},                       // left out
		{RequestID: strings.Repeat("x", 201), NextResponse: 9}, // left out
	}
	if _, err := o.SaveOpenRequests(ctx, []OpenRequest{{RequestID: "stale", NextResponse: 5}}); err != nil {
		t.Fatal(err)
	}
	if n, err := o.SaveOpenRequests(ctx, reqs); err != nil || n != 2 {
		t.Fatalf("save = %d, %v", n, err)
	}
	o.Close()
	o = open()
	defer func() { o.Close() }()
	got, err := o.TakeOpenRequests(ctx, time.Minute)
	if err != nil || len(got) != 2 {
		t.Fatalf("take = %+v, %v", got, err)
	}
	byID := map[string]OpenRequest{}
	for _, r := range got {
		byID[r.RequestID] = r
	}
	if a := byID["req-a"]; a != reqs[0] {
		t.Fatalf("req-a = %+v, want %+v", a, reqs[0])
	}
	if b := byID["req-b"]; b != reqs[1] {
		t.Fatalf("req-b = %+v, want %+v", b, reqs[1])
	}
	if again, err := o.TakeOpenRequests(ctx, time.Minute); err != nil || len(again) != 0 {
		t.Fatalf("second take = %+v, %v", again, err)
	}
	if _, err := o.SaveOpenRequests(ctx, reqs[:1]); err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(3 * time.Minute)
	if old, err := o.TakeOpenRequests(ctx, 2*time.Minute); err != nil || len(old) != 0 {
		t.Fatalf("take of an old set = %+v, %v", old, err)
	}
	var left int
	if err := o.db.QueryRowContext(ctx, "SELECT count(*) FROM open_requests").Scan(&left); err != nil || left != 0 {
		t.Fatalf("rows left = %d, %v", left, err)
	}
}

// TestOpenRequestsTableAddedToOlderOutbox: an outbox made before the table existed gets it at the
// next open; the rest of the database is unchanged and user_version stays 1.
func TestOpenRequestsTableAddedToOlderOutbox(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "data")
	o, err := Open(Options{Dir: dir, CapBytes: 1 << 40, Retention: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	path := o.Path()
	o.Close()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("DROP TABLE open_requests"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	o, err = Open(Options{Dir: dir, CapBytes: 1 << 40, Retention: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	if n, err := o.SaveOpenRequests(ctx, []OpenRequest{{RequestID: "req-c", NextResponse: 1}}); err != nil || n != 1 {
		t.Fatalf("save on an upgraded outbox = %d, %v", n, err)
	}
	var version int
	if err := o.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version != 1 {
		t.Fatalf("user_version = %d, %v", version, err)
	}
}

// TestCompletionNumberingSaveTake: SaveCarry saves the numbering with the open requests in one
// transaction; a take returns it once (within maxAge) and deletes it, across a close and reopen.
func TestCompletionNumberingSaveTake(t *testing.T) {
	ctx := context.Background()
	c := &clock{t: time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)}
	dir := filepath.Join(t.TempDir(), "data")
	open := func() *Outbox {
		o, err := Open(Options{Dir: dir, CapBytes: 1 << 40, Retention: 168 * time.Hour, Now: c.now})
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	o := open()
	nums := []Numbering{
		{RequestID: "closed", NextInputSeq: 1, NextOrdinal: 1, FirstHookAt: 10, LastHookAt: 20, BareStart: 5, TakenEnd: 30, ClosedAt: 40},
		{RequestID: "open", NextInputSeq: 2, NextOrdinal: 2, NextResponse: 1, FirstHookAt: 11, LastHookAt: 21},
		{RequestID: ""}, // left out
	}
	if n, err := o.SaveCarry(ctx, []OpenRequest{{RequestID: "open", NextInputSeq: 2}}, nums); err != nil || n != 1 {
		t.Fatalf("save = %d, %v", n, err)
	}
	o.Close()
	o = open()
	got, err := o.TakeNumbering(ctx, time.Minute)
	if err != nil || len(got) != 2 {
		t.Fatalf("take = %+v, %v", got, err)
	}
	for _, g := range got {
		if g != nums[0] && g != nums[1] {
			t.Fatalf("row %+v", g)
		}
	}
	if again, err := o.TakeNumbering(ctx, time.Minute); err != nil || len(again) != 0 {
		t.Fatalf("second take = %v, %v", again, err)
	}
	if _, err := o.SaveCarry(ctx, nil, nums[:1]); err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(3 * time.Minute)
	if old, err := o.TakeNumbering(ctx, 2*time.Minute); err != nil || len(old) != 0 {
		t.Fatalf("stale take = %v, %v", old, err)
	}
	o.Close()
}
