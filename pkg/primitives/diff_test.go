package primitives

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"testing"
	"time"

	"github.com/go-freebsd/zfs/pkg/ioctl"
)

const (
	modeFile = 0o100644
	modeDir  = 0o040755
	modeLink = 0o120777
	modeFifo = 0o010644
)

func st(gen, mode, links, sec uint64) ioctl.Stat {
	return ioctl.Stat{Gen: gen, Mode: mode, Links: links, Ctime: [2]uint64{sec, 7}}
}

func collect(t *testing.T, c *Client, from, to string) []string {
	t.Helper()
	var out []string
	for e, err := range c.Diff(context.Background(), from, to) {
		if err != nil {
			t.Fatalf("Diff: %v", err)
		}
		line := fmt.Sprintf("%c %c %s", e.Change, e.Type, e.Path)
		if e.NewPath != "" {
			line += " -> " + e.NewPath
		}
		if e.LinkDelta != 0 {
			line += fmt.Sprintf(" (%+d)", e.LinkDelta)
		}
		out = append(out, line)
	}
	return out
}

func TestDiffPortsLibzfsRules(t *testing.T) {
	k := newFake(t)
	c := newClient(k)
	const a, b = "tank/a@s1", "tank/a@s2"
	k.diffRecords = []diffRec{{ddrInUse, 2, 12}, {ddrFree, 20, 30}}
	k.objs = map[string]map[uint64]fakeObj{
		a: {
			2:  {path: "/gone", stat: st(1, modeFile, 1, 10)},  // absent in b: removed
			4:  {path: "/same", stat: st(1, modeFile, 1, 10)},  // same ctime: not reported
			5:  {path: "/old", stat: st(1, modeFile, 1, 10)},   // renamed
			6:  {path: "/edit", stat: st(1, modeFile, 1, 10)},  // modified
			7:  {path: "/redo", stat: st(1, modeFile, 1, 10)},  // re-created: other generation
			8:  {path: "/morph", stat: st(1, modeFile, 1, 10)}, // file became a link, same generation
			9:  {path: "/hard", stat: st(1, modeFile, 1, 10)},  // gained a hard link
			10: {path: "/dir", stat: st(1, modeDir, 2, 10)},    // directory: link counts do not count
			12: {path: "/dying", stat: st(1, modeFile, 1, 10), errno: syscall.ESTALE},
			21: {path: "/freed1", stat: st(1, modeFile, 1, 10)},
			22: {path: "/freed2", stat: st(1, modeFifo, 1, 10)},
			25: {path: "/queued", errno: syscall.ESTALE}, // delete queue on the from side: skipped
			99: {path: "/outside", stat: st(1, modeFile, 1, 10)},
		},
		b: {
			3:  {path: "/new", stat: st(2, modeFile, 1, 11)},
			4:  {path: "/same", stat: st(1, modeFile, 1, 10)},
			5:  {path: "/new-name", stat: st(1, modeFile, 1, 12)},
			6:  {path: "/edit", stat: st(1, modeFile, 1, 12)},
			7:  {path: "/redo", stat: st(5, modeFile, 1, 12)},
			8:  {path: "/morph", stat: st(1, modeLink, 1, 12)},
			9:  {path: "/hard", stat: st(1, modeFile, 2, 12)},
			10: {path: "/dir", stat: st(1, modeDir, 3, 12)},
			12: {path: "(x)", stat: st(1, modeFile, 1, 12), errno: syscall.ESTALE},
		},
	}
	// Object 3 is only in b (added); 2 only in a (removed); 11 in neither.
	k.diffRecords[0].last = 12
	got := collect(t, c, a, b)
	want := []string{
		"- F /gone",
		"+ F /new",
		"R F /old -> /new-name",
		"M F /edit",
		"- F /redo", "+ F /redo",
		"- F /morph", "+ @ /morph",
		"M F /hard (+1)",
		"M / /dir",
		"M F (on_delete_queue)", // ESTALE is no error: the object is named as zfs diff names it
		"- F /freed1",
		"- | /freed2",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("got\n%q\nwant\n%q", got, want)
	}
	if len(k.diffArgs) != 1 || k.diffArgs[0] != [2]string{a, b} {
		t.Errorf("the ioctl was asked for %v", k.diffArgs)
	}
}

func TestDiffAgainstTheLiveFilesystemUsesATemporarySnapshot(t *testing.T) {
	k := newFake(t)
	c := newClient(k)
	k.diffRecords = []diffRec{{ddrInUse, 3, 3}}
	k.objs = map[string]map[uint64]fakeObj{"tank/a@zfs-diff-1": {3: {path: "/new", stat: st(2, modeFile, 1, 11)}}}
	for _, to := range []string{"", ""} {
		got := collect(t, c, "tank/a@s1", to)
		if fmt.Sprint(got) != "[+ F /new]" {
			t.Errorf("got %q", got)
		}
	}
	if len(k.tmpSnaps) != 2 || k.diffArgs[0] != [2]string{"tank/a@s1", "tank/a@zfs-diff-1"} {
		t.Errorf("tmp snapshots %v, ioctl args %v", k.tmpSnaps, k.diffArgs)
	}
	// "@name" is a snapshot of the dataset of from.
	k.objs["tank/a@s2"] = map[uint64]fakeObj{3: {path: "/new", stat: st(2, modeFile, 1, 11)}}
	if got := collect(t, c, "tank/a@s1", "@s2"); len(got) != 1 {
		t.Errorf("got %q", got)
	}
}

func TestDiffRefusesBadNamesAndReportsKernelErrors(t *testing.T) {
	k := newFake(t)
	c := newClient(k)
	for _, tc := range [][2]string{{"tank/a", "tank/a@s2"}, {"tank/a@", "tank/a@s2"}, {"tank/a#b", "tank/a@s2"}, {"tank/a@s1", "tank/a"}, {"@s1", "tank/a@s2"}} {
		var err error
		for _, e := range c.Diff(context.Background(), tc[0], tc[1]) {
			err = e
		}
		if err == nil || len(k.diffArgs) != 0 {
			t.Errorf("%v: err %v, ioctl %v", tc, err, k.diffArgs)
		}
	}
	k.writeErr = map[ioctl.IOC]error{ioctl.IOCDiff: &ioctl.Error{Ioc: ioctl.IOCDiff, Errno: syscall.EXDEV}}
	var err error
	for _, e := range c.Diff(context.Background(), "tank/a@s1", "tank/b@s2") {
		err = e
	}
	if !errors.Is(err, syscall.EXDEV) {
		t.Errorf("EXDEV: %v", err)
	}
	// An object that cannot be described is an error that names it.
	k.writeErr = nil
	k.diffRecords = []diffRec{{ddrInUse, 3, 3}}
	k.objs = map[string]map[uint64]fakeObj{"tank/a@s1": {3: {errno: syscall.EIO}}, "tank/a@s2": {}}
	err = nil
	for _, e := range c.Diff(context.Background(), "tank/a@s1", "tank/a@s2") {
		err = e
	}
	if !errors.Is(err, syscall.EIO) {
		t.Errorf("EIO: %v", err)
	}
}

// Leaving the loop early, or cancelling, ends the kernel's write and leaves
// nothing behind, even when the kernel has far more to say than a pipe holds.
func TestDiffStopsEarlyAndOnCancel(t *testing.T) {
	k := newFake(t)
	c := newClient(k)
	k.diffRecords = []diffRec{{ddrInUse, 1, 1}}
	for i := 0; i < 20000; i++ { // 480 KB of records: more than a pipe holds
		k.diffRecords = append(k.diffRecords, diffRec{ddrInUse, 1000 + uint64(i), 1000 + uint64(i)})
	}
	k.objs = map[string]map[uint64]fakeObj{"tank/a@s1": {}, "tank/a@s2": {1: {path: "/first", stat: st(1, modeFile, 1, 1)}}}

	done := make(chan int, 1)
	go func() {
		n := 0
		for _, err := range c.Diff(context.Background(), "tank/a@s1", "tank/a@s2") {
			if err != nil {
				t.Error(err)
			}
			n++
			break
		}
		done <- n
	}()
	select {
	case n := <-done:
		if n != 1 {
			t.Errorf("%d entries", n)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("breaking out of the loop did not end the diff")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var err error
	for _, e := range c.Diff(ctx, "tank/a@s1", "tank/a@s2") {
		err = e
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
	// And it still works.
	if got := collect(t, c, "tank/a@s1", "tank/a@s2"); len(got) != 1 {
		t.Errorf("%q", got)
	}
}

func TestEscapePath(t *testing.T) {
	for in, want := range map[string]string{
		"/plain/name.txt": "/plain/name.txt",
		"/a b":            `/a\0040b`,
		"/a\nb":           `/a\0012b`,
		"/a\tb":           `/a\0011b`,
		`/a\b`:            `/a\0134b`,
		"/café":           `/caf\0303\0251`,
		"/del\x7f":        `/del\0177`,
		"":                "",
	} {
		if got := EscapePath(in); got != want {
			t.Errorf("EscapePath(%q) = %q, want %q", in, got, want)
		}
	}
	e := DiffEntry{Path: "/a b", NewPath: "/c d"}
	if e.EscapedPath() != `/a\0040b` || e.EscapedNewPath() != `/c\0040d` {
		t.Errorf("EscapedPath = %q, EscapedNewPath = %q", e.EscapedPath(), e.EscapedNewPath())
	}
}
