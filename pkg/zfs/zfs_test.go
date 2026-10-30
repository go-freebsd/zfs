package zfs

import (
	"errors"
	"io/fs"
	"reflect"
	"syscall"
	"testing"

	"github.com/go-freebsd/zfs/pkg/ioctl"
)

func names(ds []*Dataset) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.Name
	}
	return out
}

func TestGetDecodesStatsAndProperties(t *testing.T) {
	c := newClient(newFake(t))

	a, err := c.Get("tank/a")
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != "tank/a" || a.Type != Filesystem || a.GUID != 1010 || a.CreationTxg != 10 {
		t.Errorf("%+v", a)
	}
	if got := a.Props["compression"]; got != (Property{Value: uint64(15), Source: "tank/a"}) {
		t.Errorf("compression = %#v", got)
	}
	if a.Mountpoint() != "/tank/a" || a.Used() != 1000 || a.Available() != 1<<20 || a.Referenced() != 100 {
		t.Errorf("helpers: %q %d %d %d", a.Mountpoint(), a.Used(), a.Available(), a.Referenced())
	}
	if a.Creation().Unix() != 1700000010 {
		t.Errorf("creation = %v", a.Creation())
	}
	if _, ok := a.Uint64("mountpoint"); ok {
		t.Error("a string property is not a number")
	}
	if _, ok := a.String("used"); ok {
		t.Error("a numeric property is not a string")
	}
	if a.Quota() != 0 {
		t.Error("absent property must read as zero")
	}

	vol, _ := c.Get("tank/vol")
	if vol.Type != Volume {
		t.Errorf("volume type = %v", vol.Type)
	}
	snap, _ := c.Get("tank/a@s1")
	if snap.Type != Snapshot || snap.NumClones != 1 {
		t.Errorf("snapshot = %+v", snap)
	}
	clone, _ := c.Get("tank/clone")
	if clone.Origin != "tank/a@s1" {
		t.Errorf("origin = %q", clone.Origin)
	}
}

func TestGetBookmark(t *testing.T) {
	c := newClient(newFake(t))
	b, err := c.Get("tank/a#bm1")
	if err != nil {
		t.Fatal(err)
	}
	if b.Type != Bookmark || b.GUID != 7001 || b.CreationTxg != 17 || b.Name != "tank/a#bm1" {
		t.Errorf("%+v", b)
	}
	if _, err := c.Get("tank/a#nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing bookmark: %v", err)
	}
}

func TestGetPropertyFilter(t *testing.T) {
	c := newClient(newFake(t))
	d, err := c.Get("tank/a", "used", "mountpoint", "doesnotexist")
	if err != nil || len(d.Props) != 2 || d.Props["used"].Value != uint64(1000) {
		t.Errorf("%+v, %v", d, err)
	}
}

func TestMissingAndExists(t *testing.T) {
	k := newFake(t)
	c := newClient(k)
	if _, err := c.Get("tank/missing"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Get: %v", err)
	}
	for name, want := range map[string]bool{
		"tank": true, "tank/a": true, "tank/a@s1": true, "tank/a#bm1": true,
		"tank/missing": false, "tank/a@nope": false, "tank/a#nope": false,
	} {
		if got, err := c.Exists(name); err != nil || got != want {
			t.Errorf("Exists(%q) = %v, %v; want %v", name, got, err, want)
		}
	}
	k.hook = func(ioc ioctl.IOC, cmd *ioctl.Cmd, n int) error {
		if ioc == ioctl.IOCObjsetStats && cmd.GetName() == "tank/broken" {
			return &ioctl.Error{Ioc: ioc, Errno: syscall.EIO}
		}
		return nil
	}
	if ok, err := c.Exists("tank/broken"); ok || !errors.Is(err, syscall.EIO) {
		t.Errorf("other errors must surface, not read as \"missing\": %v, %v", ok, err)
	}
}

func TestListDefaultTypesAndOrder(t *testing.T) {
	c := newClient(newFake(t))
	got, err := c.List("tank", ListOptions{Depth: Unlimited})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"tank", "tank/a", "tank/a/child", "tank/clone", "tank/vol", "tank/z"}
	if !reflect.DeepEqual(names(got), want) {
		t.Errorf("order = %v\nwant    %v", names(got), want)
	}
	if got[4].Type != Volume {
		t.Error("tank/vol must be a volume")
	}
}

func TestListAllTypes(t *testing.T) {
	c := newClient(newFake(t))
	got, err := c.List("tank", ListOptions{Types: Filesystem | Volume | Snapshot | Bookmark, Depth: Unlimited})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"tank",
		"tank/a", "tank/a@s1", "tank/a@s2", "tank/a@s3", "tank/a#bm1", "tank/a#bm2",
		"tank/a/child",
		"tank/clone",
		"tank/vol", "tank/vol@v1",
		"tank/z",
	}
	if !reflect.DeepEqual(names(got), want) {
		t.Errorf("order = %v\nwant    %v", names(got), want)
	}
}

func TestListDepth(t *testing.T) {
	c := newClient(newFake(t))
	for depth, want := range map[int][]string{
		0: {"tank"},
		1: {"tank", "tank/a", "tank/clone", "tank/vol", "tank/z"},
		2: {"tank", "tank/a", "tank/a/child", "tank/clone", "tank/vol", "tank/z"},
	} {
		got, err := c.List("tank", ListOptions{Depth: depth})
		if err != nil || !reflect.DeepEqual(names(got), want) {
			t.Errorf("depth %d: %v, %v; want %v", depth, names(got), err, want)
		}
	}
	// A list of a sub-tree starts at its root.
	got, _ := c.List("tank/a", ListOptions{Depth: Unlimited})
	if !reflect.DeepEqual(names(got), []string{"tank/a", "tank/a/child"}) {
		t.Errorf("sub-tree = %v", names(got))
	}
}

func TestListSnapshotsOnlyWithTxgRange(t *testing.T) {
	c := newClient(newFake(t))
	all, _ := c.List("tank/a", ListOptions{Types: Snapshot})
	if !reflect.DeepEqual(names(all), []string{"tank/a@s1", "tank/a@s2", "tank/a@s3"}) {
		t.Errorf("snapshots by creation = %v", names(all))
	}
	got, err := c.List("tank/a", ListOptions{Types: Snapshot, MinTxg: 16, MaxTxg: 25})
	if err != nil || !reflect.DeepEqual(names(got), []string{"tank/a@s2"}) {
		t.Errorf("range = %v, %v", names(got), err)
	}
	if got[0].Type != Snapshot || got[0].GUID != 2020 {
		t.Errorf("%+v", got[0])
	}
}

func TestListSimpleSkipsProperties(t *testing.T) {
	c := newClient(newFake(t))
	got, err := c.List("tank", ListOptions{Depth: Unlimited, Simple: true, Types: Filesystem | Volume | Snapshot})
	if err != nil || len(got) == 0 {
		t.Fatal(err)
	}
	for _, d := range got {
		if len(d.Props) != 0 {
			t.Errorf("%s: simple listing returned %d properties", d.Name, len(d.Props))
		}
		if d.GUID == 0 || d.Type == 0 {
			t.Errorf("%s: identity missing: %+v", d.Name, d)
		}
	}
}

func TestListMissingRootIsAnError(t *testing.T) {
	c := newClient(newFake(t))
	if _, err := c.List("tank/missing", ListOptions{}); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v", err)
	}
}

func TestListRootThatIsASnapshotHasNoChildren(t *testing.T) {
	c := newClient(newFake(t))
	got, err := c.List("tank/a@s1", ListOptions{Types: Snapshot, Depth: Unlimited})
	if err != nil || !reflect.DeepEqual(names(got), []string{"tank/a@s1"}) {
		t.Errorf("%v, %v", names(got), err)
	}
}

func TestListToleratesADatasetDestroyedDuringIteration(t *testing.T) {
	k := newFake(t)
	k.hook = func(ioc ioctl.IOC, cmd *ioctl.Cmd, n int) error {
		if ioc == ioctl.IOCDatasetListNext && cmd.GetName() == "tank" && cmd.Cookie != 0 {
			return &ioctl.Error{Ioc: ioc, Name: cmd.GetName(), Errno: syscall.ENOENT}
		}
		return nil
	}
	got, err := newClient(k).List("tank", ListOptions{Depth: 1})
	if err != nil || !reflect.DeepEqual(names(got), []string{"tank", "tank/z"}) {
		t.Errorf("%v, %v: ENOENT mid-iteration ends the iteration", names(got), err)
	}
}

func TestListSurfacesOtherErrors(t *testing.T) {
	k := newFake(t)
	k.hook = func(ioc ioctl.IOC, cmd *ioctl.Cmd, n int) error {
		if ioc == ioctl.IOCDatasetListNext {
			return &ioctl.Error{Ioc: ioc, Errno: syscall.EIO}
		}
		return nil
	}
	if _, err := newClient(k).List("tank", ListOptions{Depth: 1}); !errors.Is(err, syscall.EIO) {
		t.Errorf("err = %v", err)
	}
}

func TestWalkStopsAtTheFirstCallbackError(t *testing.T) {
	stop := errors.New("enough")
	var seen []string
	err := newClient(newFake(t)).Walk("tank", ListOptions{Depth: Unlimited}, func(d *Dataset) error {
		seen = append(seen, d.Name)
		if len(seen) == 2 {
			return stop
		}
		return nil
	})
	if err != stop || !reflect.DeepEqual(seen, []string{"tank", "tank/a"}) {
		t.Errorf("err %v, seen %v", err, seen)
	}
}

// The cursor protocol is checked inside the fake: it fails the test if a
// continued iteration names anything but the parent it was started for.
func TestIterationNamesTheParentOnEveryCall(t *testing.T) {
	k := newFake(t)
	if _, err := newClient(k).List("tank", ListOptions{Types: Filesystem | Volume | Snapshot, Depth: Unlimited}); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, c := range k.calls {
		if c == "ZFS_IOC_DATASET_LIST_NEXT tank" {
			n++
		}
	}
	if n != 5 { // four children, then ESRCH
		t.Errorf("%d list-next calls for the pool's children, want 5: %v", n, k.calls)
	}
}

func TestHolds(t *testing.T) {
	c := newClient(newFake(t))
	holds, err := c.Holds("tank/a@s1")
	if err != nil || len(holds) != 2 || holds[0].Tag != "keep" || holds[1].Tag != "other" ||
		holds[0].Created.Unix() != 1700000100 {
		t.Errorf("%+v, %v", holds, err)
	}
	if none, err := c.Holds("tank/a@s2"); err != nil || len(none) != 0 {
		t.Errorf("no holds: %v, %v", none, err)
	}
	if _, err := c.Holds("tank/a@missing"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing snapshot: %v", err)
	}
}

func TestBookmarksAcceptBothValueShapes(t *testing.T) {
	for _, bare := range []bool{false, true} {
		k := newFake(t)
		k.bareBookmarkProps = bare
		bms, err := newClient(k).Bookmarks("tank/a")
		if err != nil || len(bms) != 2 {
			t.Fatalf("bare=%v: %v, %v", bare, bms, err)
		}
		// ordered by creation transaction, not by name or kernel order
		if bms[0].Name != "tank/a#bm1" || bms[1].Name != "tank/a#bm2" {
			t.Errorf("bare=%v: order %v", bare, names(bms))
		}
		if bms[0].GUID != 7001 || bms[0].CreationTxg != 17 || bms[0].Props["creation"].Value != uint64(1700000250) {
			t.Errorf("bare=%v: %+v", bare, bms[0])
		}
	}
	none, err := newClient(newFake(t)).Bookmarks("tank/z")
	if err != nil || len(none) != 0 {
		t.Errorf("no bookmarks: %v, %v", none, err)
	}
}

func TestSpace(t *testing.T) {
	c := newClient(newFake(t))
	w, err := c.SpaceWritten("tank/a@s2", "tank/a@s1")
	if err != nil || w != (Space{Written: 1000, Compressed: 500, Uncompressed: 2000}) {
		t.Errorf("%+v, %v", w, err)
	}
	if _, err := c.SpaceWritten("tank/missing", "tank/a@s1"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing: %v", err)
	}
	r, err := c.SnapshotRangeSpace("tank/a@s1", "tank/a@s3")
	if err != nil || r != (Space{Used: 4096, Compressed: 2048, Uncompressed: 8192}) {
		t.Errorf("%+v, %v", r, err)
	}
	if _, err := c.SnapshotRangeSpace("tank/a@nope", "tank/a@s3"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing first snapshot: %v", err)
	}
}

func TestPools(t *testing.T) {
	k := newFake(t)
	k.pools = []string{"zroot", "tank", "backup"}
	got, err := newClient(k).Pools()
	if err != nil || !reflect.DeepEqual(got, []string{"backup", "tank", "zroot"}) {
		t.Errorf("%v, %v", got, err)
	}
}

func TestTypeString(t *testing.T) {
	for in, want := range map[Type]string{
		Filesystem: "filesystem", Snapshot | Bookmark: "snapshot,bookmark", 0: "unknown",
		Filesystem | Volume | Snapshot | Bookmark: "filesystem,volume,snapshot,bookmark",
	} {
		if in.String() != want {
			t.Errorf("%d = %q, want %q", in, in.String(), want)
		}
	}
}

func TestCloseWithoutConnection(t *testing.T) {
	if err := newClient(newFake(t)).Close(); err != nil {
		t.Error(err)
	}
}

func TestClones(t *testing.T) {
	c := newClient(newFake(t))
	s1, err := c.Get("tank/a@s1")
	if err != nil || !reflect.DeepEqual(s1.Clones(), []string{"tank/clone"}) {
		t.Errorf("%v, %v", s1.Clones(), err)
	}
	s2, _ := c.Get("tank/a@s2")
	if s2.Clones() != nil {
		t.Errorf("a snapshot without clones: %v", s2.Clones())
	}
}

// The kernel stores an inherited mountpoint as the ancestor's value; the
// effective one needs the rest of the name appended, as zfs get does.
func TestMountpointIsTheEffectiveOne(t *testing.T) {
	mp := func(name, value, source string) string {
		d := &Dataset{Name: name, Props: map[string]Property{"mountpoint": {Value: value, Source: source}}}
		return d.Mountpoint()
	}
	for _, c := range []struct{ name, value, source, want string }{
		{"p/a", "/data", "p/a", "/data"},         // local
		{"p/a/b", "/data", "p/a", "/data/b"},     // inherited
		{"p/a/b/c", "/data", "p/a", "/data/b/c"}, // inherited, two levels
		{"p/a/b", "/", "p/a", "/b"},              // inherited from a dataset mounted at /
		{"p/a/b", "none", "p/a", "none"},         // none and legacy never get a suffix
		{"p/a/b", "legacy", "p/a", "legacy"},
		{"p/a", "/p/a", "", "/p/a"},       // default
		{"p/a/b", "/x", "$recvd", "/x"},   // received values are as stored
		{"p/ab", "/data", "p/a", "/data"}, // a prefix of the name that is not a parent
	} {
		if got := mp(c.name, c.value, c.source); got != c.want {
			t.Errorf("Mountpoint(%s, %q from %q) = %q, want %q", c.name, c.value, c.source, got, c.want)
		}
	}
	if (&Dataset{Name: "p/vol"}).Mountpoint() != "" {
		t.Error("a dataset without the property has no mountpoint")
	}
}
