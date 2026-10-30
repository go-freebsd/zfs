//go:build freebsd

package primitives

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-freebsd/nvlist/pkg/nvlist"
	"github.com/go-freebsd/zfs/pkg/ioctl"
)

// These tests read a real pool and compare the result with zfs(8) and
// zpool(8), which act as an independent oracle. They need root, a loaded ZFS
// module, a pool named by ZFS_TEST_POOL and the datasets that
// internal/ci/fixtures.sh creates.

func testPool(t *testing.T) string {
	t.Helper()
	pool := os.Getenv("ZFS_TEST_POOL")
	if pool == "" {
		t.Skip("set ZFS_TEST_POOL to run against a real pool")
	}
	return pool
}

func testClient(t *testing.T) (*Client, string) {
	t.Helper()
	pool := testPool(t)
	c, err := Open()
	if err != nil {
		t.Fatalf("ZFS_TEST_POOL is set but /dev/zfs cannot be opened: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	if ok, err := c.Exists(pool + "/a@s2"); err != nil || !ok {
		t.Fatalf("fixtures are missing (%v, %v): run internal/ci/fixtures.sh %s", ok, err, pool)
	}
	return c, pool
}

// cli runs a command and returns its output with the trailing newline cut.
func cli(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Fatalf("%s %v: %v\n%s", name, args, err, ee.Stderr)
		}
		t.Fatalf("%s %v: %v", name, args, err)
	}
	return strings.TrimRight(string(out), "\n")
}

// rows splits tab separated output (-H) into fields.
func rows(out string) [][]string {
	var r [][]string
	for _, l := range strings.Split(out, "\n") {
		if l != "" {
			r = append(r, strings.Split(l, "\t"))
		}
	}
	return r
}

func lines(out string) []string {
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

func sorted(s []string) []string { s = append([]string(nil), s...); sort.Strings(s); return s }

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPoolsMatchZpool(t *testing.T) {
	c, pool := testClient(t)
	got, err := c.Pools()
	if err != nil {
		t.Fatal(err)
	}
	want := sorted(lines(cli(t, "zpool", "list", "-H", "-o", "name")))
	if !equal(got, want) {
		t.Errorf("Pools = %v, zpool list = %v", got, want)
	}
	found := false
	for _, p := range got {
		found = found || p == pool
	}
	if !found {
		t.Errorf("test pool %q missing from %v", pool, got)
	}
}

func TestListMatchesZfsList(t *testing.T) {
	c, pool := testClient(t)
	all := Filesystem | Volume | Snapshot | Bookmark
	got, err := c.List(pool, ListOptions{Types: all, Depth: Unlimited})
	if err != nil {
		t.Fatal(err)
	}
	have := names(got)
	want := lines(cli(t, "zfs", "list", "-H", "-o", "name", "-t", "filesystem,volume,snapshot,bookmark", "-r", pool))
	if !equal(sorted(have), sorted(want)) {
		missing, extra := diff(want, have)
		t.Errorf("List differs from zfs list: missing %v, extra %v", missing, extra)
	}
	// 128 datasets, 6 snapshots and 1 bookmark are created by fixtures.sh.
	if len(have) < 135 {
		t.Errorf("only %d entries: the fixtures (135) were not listed", len(have))
	}

	// Order: a dataset comes before its snapshots, bookmarks and children.
	pos := map[string]int{}
	for i, n := range have {
		pos[n] = i
	}
	for i, n := range have {
		if j := strings.IndexAny(n, "@#"); j >= 0 {
			if p, ok := pos[n[:j]]; !ok || p >= i {
				t.Errorf("%s is listed before its dataset", n)
			}
		} else if j := strings.LastIndexByte(n, '/'); j >= 0 {
			if p, ok := pos[n[:j]]; !ok || p >= i {
				t.Errorf("%s is listed before its parent", n)
			}
		}
	}

	// Types are real: spot-check the kinds.
	byName := map[string]*Dataset{}
	for _, d := range got {
		byName[d.Name] = d
	}
	for name, typ := range map[string]Type{
		pool: Filesystem, pool + "/a": Filesystem, pool + "/vol": Volume,
		pool + "/a@s1": Snapshot, pool + "/vol@v1": Snapshot, pool + "/a#bm1": Bookmark,
	} {
		if d := byName[name]; d == nil || d.Type != typ {
			t.Errorf("%s: type %v, want %v", name, d, typ)
		}
	}
}

func diff(want, have []string) (missing, extra []string) {
	in := func(s []string, x string) bool {
		for _, y := range s {
			if x == y {
				return true
			}
		}
		return false
	}
	for _, w := range want {
		if !in(have, w) {
			missing = append(missing, w)
		}
	}
	for _, h := range have {
		if !in(want, h) {
			extra = append(extra, h)
		}
	}
	return
}

func TestListDepthAndSnapshotRange(t *testing.T) {
	c, pool := testClient(t)
	got, err := c.List(pool, ListOptions{Depth: 1})
	if err != nil {
		t.Fatal(err)
	}
	want := lines(cli(t, "zfs", "list", "-H", "-o", "name", "-d", "1", "-t", "filesystem,volume", pool))
	if !equal(sorted(names(got)), sorted(want)) {
		t.Errorf("depth 1: %v, zfs list -d 1: %v", names(got), want)
	}

	snaps, err := c.List(pool+"/a", ListOptions{Types: Snapshot})
	if err != nil || len(snaps) != 2 || snaps[0].Name != pool+"/a@s1" || snaps[1].Name != pool+"/a@s2" {
		t.Fatalf("snapshots of a: %v, %v", names(snaps), err)
	}
	// A range that holds only the second snapshot.
	ranged, err := c.List(pool+"/a", ListOptions{Types: Snapshot, MinTxg: snaps[1].CreationTxg})
	if err != nil || len(ranged) != 1 || ranged[0].Name != pool+"/a@s2" {
		t.Errorf("MinTxg %d: %v, %v", snaps[1].CreationTxg, names(ranged), err)
	}
	ranged, err = c.List(pool+"/a", ListOptions{Types: Snapshot, MaxTxg: snaps[0].CreationTxg})
	if err != nil || len(ranged) != 1 || ranged[0].Name != pool+"/a@s1" {
		t.Errorf("MaxTxg %d: %v, %v", snaps[0].CreationTxg, names(ranged), err)
	}
}

func TestListSimpleOnARealKernel(t *testing.T) {
	c, pool := testClient(t)
	opts := ListOptions{Types: Filesystem | Volume | Snapshot, Depth: Unlimited}
	full, err := c.List(pool+"/a", opts)
	if err != nil {
		t.Fatal(err)
	}
	opts.Simple = true
	simple, err := c.List(pool+"/a", opts)
	if err != nil {
		t.Fatal(err)
	}
	if !equal(names(simple), names(full)) {
		t.Errorf("simple: %v, full: %v", names(simple), names(full))
	}
	for _, d := range simple {
		if len(d.Props) != 0 || d.GUID == 0 || d.Type == 0 {
			t.Errorf("%s: simple listing must carry identity but no properties: %d props, %+v", d.Name, len(d.Props), d)
		}
	}
	for _, d := range full {
		if len(d.Props) == 0 {
			t.Errorf("%s: no properties in the full listing", d.Name)
		}
	}
}

func TestListManyChildren(t *testing.T) {
	c, pool := testClient(t)
	got, err := c.List(pool+"/many", ListOptions{Depth: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 121 { // the parent and 120 children
		t.Fatalf("%d entries, want 121", len(got))
	}
	seen := map[string]bool{}
	for _, d := range got {
		if seen[d.Name] {
			t.Errorf("%s listed twice", d.Name)
		}
		seen[d.Name] = true
	}
}

func TestMissingAndExistsOnARealKernel(t *testing.T) {
	c, pool := testClient(t)
	for name, want := range map[string]bool{
		pool: true, pool + "/a": true, pool + "/vol": true, pool + "/a@s1": true, pool + "/a#bm1": true,
		pool + "/nope": false, pool + "/a@nope": false, pool + "/a#nope": false, pool + "x": false,
	} {
		if got, err := c.Exists(name); err != nil || got != want {
			t.Errorf("Exists(%q) = %v, %v; want %v", name, got, err, want)
		}
	}
	if _, err := c.Get(pool + "/nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Get missing: %v", err)
	}
	if _, err := c.List(pool+"/nope", ListOptions{}); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("List missing: %v", err)
	}
}

// Compares every property the kernel returns with "zfs get -Hp all".
func TestPropertiesMatchZfsGet(t *testing.T) {
	c, pool := testClient(t)
	totalStrs := 0
	for _, name := range []string{
		pool, pool + "/a", pool + "/a/b", pool + "/a/b/c", pool + "/vol", pool + "/clone",
		pool + "/a@s1", pool + "/a/b@r1", pool + "/vol@v1",
	} {
		d, err := c.Get(name)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		numeric, strs, skipped, mismatches := compareProps(t, d)
		for _, m := range mismatches {
			t.Errorf("%s: %s", name, m)
		}
		t.Logf("%s: %d numeric and %d string properties equal, %d not comparable", name, numeric, strs, skipped)
		if numeric < 8 || numeric+strs < 10 {
			t.Errorf("%s: only %d numeric / %d string properties were compared: the test would pass vacuously", name, numeric, strs)
		}
		totalStrs += strs
	}
	if totalStrs < 5 {
		t.Errorf("only %d string properties were compared in total", totalStrs)
	}
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func compareProps(t *testing.T, d *Dataset) (numeric, strs, skipped int, mismatches []string) {
	t.Helper()
	out := cli(t, "zfs", "get", "-Hp", "-o", "property,value,source", "all", d.Name)
	for _, r := range rows(out) {
		if len(r) != 3 {
			continue
		}
		prop, val, src := r[0], r[1], r[2]
		ours, ok := d.Props[prop]
		if !ok {
			continue // computed by libzfs, or not returned for this kind
		}
		inherited := ours.Source != "" && ours.Source != d.Name && ours.Source != "$recvd"
		if prop == "mountpoint" && inherited {
			skipped++ // the kernel stores the ancestor's value; Mountpoint() is checked separately
			continue
		}
		if _, isList := ours.Value.(*nvlist.List); isList {
			skipped++ // clones: a list of names, checked separately
			continue
		}
		switch v := ours.Value.(type) {
		case uint64:
			if !isDigits(val) {
				skipped++ // an enumerated property: the CLI prints its name
				continue
			}
			n, _ := strconv.ParseUint(val, 10, 64)
			if prop == "available" && (n > v && n-v < 1<<20 || v > n && v-n < 1<<20) {
				numeric++ // the pool-wide free space moves while earlier tests' frees are synced
				continue
			}
			if n != v {
				mismatches = append(mismatches, prop+": kernel "+strconv.FormatUint(v, 10)+", zfs get "+val)
				continue
			}
			numeric++
		case string:
			if val != v && !(v == "" && val == "-") {
				mismatches = append(mismatches, prop+": kernel "+strconv.Quote(v)+", zfs get "+strconv.Quote(val))
				continue
			}
			strs++
		default:
			mismatches = append(mismatches, prop+": unexpected value type")
			continue
		}
		if prop == "volsize" || prop == "volblocksize" {
			continue // the kernel reports no source for these; zfs get says "local"
		}
		var wantSrc []string
		switch {
		case ours.Source == "":
			wantSrc = []string{"default", "-"}
		case ours.Source == d.Name:
			wantSrc = []string{"local"}
		case ours.Source == "$recvd":
			wantSrc = []string{"received"}
		default:
			wantSrc = []string{"inherited from " + ours.Source}
		}
		okSrc := false
		for _, w := range wantSrc {
			okSrc = okSrc || src == w
		}
		if !okSrc {
			mismatches = append(mismatches, prop+": source "+strconv.Quote(ours.Source)+", zfs get says "+strconv.Quote(src))
		}
	}
	return
}

func TestUserProperties(t *testing.T) {
	c, pool := testClient(t)
	d, err := c.Get(pool + "/a")
	if err != nil {
		t.Fatal(err)
	}
	if p := d.Props["test:note"]; p.Value != "hello world" || p.Source != pool+"/a" {
		t.Errorf("test:note = %#v", p)
	}
	// Inherited by the child.
	b, _ := c.Get(pool + "/a/b")
	if p := b.Props["test:note"]; p.Value != "hello world" || p.Source != pool+"/a" {
		t.Errorf("inherited test:note = %#v", p)
	}
	if q, _ := d.Uint64("quota"); q != 64<<20 {
		t.Errorf("quota = %d", q)
	}
}

// One property list of more than 128 KiB: the first reply buffer is too
// small, so this exercises the growth path of the connection for real.
func TestLargePropertyListGrowsTheBuffer(t *testing.T) {
	pool := testPool(t)
	conn, err := ioctl.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var packed int
	conn.Trace = func(ioc ioctl.IOC, name string, src, dst []byte) {
		if ioc == ioctl.IOCObjsetStats && name == pool+"/big" {
			packed = len(dst)
		}
	}
	c := NewClient(conn)
	d, err := c.Get(pool + "/big")
	if err != nil {
		t.Fatal(err)
	}
	if packed <= 128<<10 {
		t.Errorf("property list is %d bytes: not larger than the initial 128 KiB buffer, the growth path was not exercised", packed)
	}
	users := 0
	for name, p := range d.Props {
		if strings.HasPrefix(name, "user:p") {
			users++
			if s, _ := p.Value.(string); len(s) != 1000 {
				t.Errorf("%s has a value of %d bytes", name, len(s))
			}
		}
	}
	if users != 150 {
		t.Errorf("%d user properties, want 150", users)
	}
	t.Logf("property list: %d bytes", packed)
}

func TestHoldsMatchZfsHolds(t *testing.T) {
	c, pool := testClient(t)
	got, err := c.Holds(pool + "/a@s1")
	if err != nil {
		t.Fatal(err)
	}
	var have []string
	for _, h := range got {
		have = append(have, h.Tag)
		if d := time.Since(h.Created); d < 0 || d > 24*time.Hour {
			t.Errorf("hold %s created %v ago", h.Tag, d)
		}
	}
	var want []string
	for _, r := range rows(cli(t, "zfs", "holds", "-H", pool+"/a@s1")) {
		want = append(want, r[1])
	}
	if !equal(have, sorted(want)) || len(have) != 2 {
		t.Errorf("holds %v, zfs holds %v", have, sorted(want))
	}
	if none, err := c.Holds(pool + "/a@s2"); err != nil || len(none) != 0 {
		t.Errorf("a@s2: %v, %v", none, err)
	}
	if _, err := c.Holds(pool + "/a@nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing snapshot: %v", err)
	}
}

func TestBookmarksMatchZfsList(t *testing.T) {
	c, pool := testClient(t)
	got, err := c.Bookmarks(pool + "/a")
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range got {
		t.Logf("bookmark %s guid=%d createtxg=%d props=%v", b.Name, b.GUID, b.CreationTxg, b.Props)
	}
	rs := rows(cli(t, "zfs", "list", "-H", "-t", "bookmark", "-r", "-o", "name,guid,createtxg", pool+"/a"))
	if len(rs) != 1 || len(got) != 1 {
		t.Fatalf("bookmarks: ours %v, zfs list %v", names(got), rs)
	}
	if got[0].Name != rs[0][0] || strconv.FormatUint(got[0].GUID, 10) != rs[0][1] || strconv.FormatUint(got[0].CreationTxg, 10) != rs[0][2] {
		t.Errorf("ours %s guid=%d txg=%d, zfs list %v", got[0].Name, got[0].GUID, got[0].CreationTxg, rs[0])
	}
	if got[0].GUID == 0 || got[0].CreationTxg == 0 {
		t.Errorf("identity not decoded: %+v", got[0])
	}
	if ok, _ := c.Exists(pool + "/a#bm1"); !ok {
		t.Error("Exists(bookmark)")
	}
}

func TestSpaceMatchesZfs(t *testing.T) {
	c, pool := testClient(t)

	w, err := c.SpaceWritten(pool+"/a@s2", pool+"/a@s1")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.TrimSpace(cli(t, "zfs", "get", "-Hp", "-o", "value", "written@s1", pool+"/a@s2"))
	if strconv.FormatUint(w.Written, 10) != want || w.Written < 200<<10 {
		t.Errorf("SpaceWritten = %+v, zfs get written@s1 = %s (the fixture wrote ~256 KiB)", w, want)
	}
	if _, err := c.SpaceWritten(pool+"/nope", pool+"/a@s1"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing: %v", err)
	}

	r, err := c.SnapshotRangeSpace(pool+"/a/b@r1", pool+"/a/b@r2")
	if err != nil {
		t.Fatal(err)
	}
	out := cli(t, "zfs", "destroy", "-nvp", pool+"/a/b@r1%r2")
	reclaim := ""
	for _, row := range rows(out) {
		if len(row) == 2 && row[0] == "reclaim" {
			reclaim = row[1]
		}
	}
	if reclaim == "" {
		t.Fatalf("no reclaim line in zfs destroy -nvp output:\n%s", out)
	}
	if strconv.FormatUint(r.Used, 10) != reclaim {
		t.Errorf("SnapshotRangeSpace = %+v, zfs destroy -nvp reclaim = %s", r, reclaim)
	}
}

func TestMountpointAndClonesMatchZfsGet(t *testing.T) {
	c, pool := testClient(t)
	for _, name := range []string{pool, pool + "/a", pool + "/a/b", pool + "/a/b/c", pool + "/clone", pool + "/big", pool + "/many/d7"} {
		d, err := c.Get(name)
		if err != nil {
			t.Fatal(err)
		}
		want := strings.TrimSpace(cli(t, "zfs", "get", "-H", "-o", "value", "mountpoint", name))
		if d.Mountpoint() != want {
			t.Errorf("%s: Mountpoint() = %q, zfs get mountpoint = %q", name, d.Mountpoint(), want)
		}
	}
	// The interesting cases are really in the fixtures.
	b, _ := c.Get(pool + "/a/b/c")
	if b.Mountpoint() != "/fixture_a/b/c" {
		t.Errorf("inherited mountpoint = %q, want /fixture_a/b/c", b.Mountpoint())
	}

	s1, err := c.Get(pool + "/a@s1")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.TrimSpace(cli(t, "zfs", "get", "-H", "-o", "value", "clones", pool+"/a@s1"))
	if got := strings.Join(s1.Clones(), ","); got != want || want != pool+"/clone" {
		t.Errorf("Clones() = %v, zfs get clones = %q", s1.Clones(), want)
	}
	cl, _ := c.Get(pool + "/clone")
	if cl.Origin != pool+"/a@s1" {
		t.Errorf("origin = %q", cl.Origin)
	}
}
