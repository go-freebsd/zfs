package primitives

import (
	"errors"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/go-freebsd/zfs/pkg/ioctl"
)

// fakeMounter is a mount table in memory. Its calls go into the fake
// kernel's call log so that tests can check the order against the ioctls.
type fakeMounter struct {
	k        *fakeKernel
	table    map[string]string // dataset -> mountpoint
	dirs     map[string]bool   // existing directories
	nonEmpty map[string]bool
	jail     bool
	failUmnt map[string]error // mountpoint -> error
	failMnt  map[string]error // dataset -> error
	opts     map[string][]string
}

func newFakeMounter(k *fakeKernel) *fakeMounter {
	return &fakeMounter{k: k, table: map[string]string{}, dirs: map[string]bool{}, nonEmpty: map[string]bool{},
		failUmnt: map[string]error{}, failMnt: map[string]error{}, opts: map[string][]string{}}
}

func (m *fakeMounter) nmount(ds, path string, opts []string) error {
	m.k.calls = append(m.k.calls, "mount "+ds+" "+path)
	if err := m.failMnt[ds]; err != nil {
		return err
	}
	if _, ok := m.table[ds]; ok && !slices.Contains(opts, "update") {
		return syscall.EBUSY
	}
	m.table[ds] = path
	m.opts[ds] = opts
	return nil
}

func (m *fakeMounter) unmount(path string, force bool) error {
	f := ""
	if force {
		f = " force"
	}
	m.k.calls = append(m.k.calls, "unmount "+path+f)
	if err := m.failUmnt[path]; err != nil {
		return err
	}
	for ds, p := range m.table {
		if p == path {
			delete(m.table, ds)
			return nil
		}
	}
	return syscall.EINVAL
}

func (m *fakeMounter) mounts() ([]Mount, error) {
	var out []Mount
	for ds, p := range m.table {
		out = append(out, Mount{ds, p})
	}
	return out, nil
}
func (m *fakeMounter) mkdirAll(p string) error { m.dirs[p] = true; return nil }
func (m *fakeMounter) isEmptyDir(p string) (bool, error) {
	return !m.nonEmpty[p], nil
}
func (m *fakeMounter) rmdir(p string) error {
	m.k.calls = append(m.k.calls, "rmdir "+p)
	delete(m.dirs, p)
	return nil
}
func (m *fakeMounter) jailed() bool { return m.jail }

// mountedClient is a client on a fake pool with a fake mount table.
func mountedClient(t *testing.T) (*Client, *fakeKernel, *fakeMounter) {
	k := newFake(t)
	c := newClient(k)
	m := newFakeMounter(k)
	c.m = m
	return c, k, m
}

// mountCalls returns the mount-related entries of the call log.
func (k *fakeKernel) mountCalls() []string {
	var out []string
	for _, c := range k.calls {
		if strings.HasPrefix(c, "mount ") || strings.HasPrefix(c, "unmount ") || strings.HasPrefix(c, "rmdir ") ||
			strings.HasPrefix(c, "ZFS_IOC_DESTROY") || strings.HasPrefix(c, "ZFS_IOC_RENAME") ||
			strings.Contains(c, "SET_PROP") || strings.Contains(c, "INHERIT") {
			out = append(out, c)
		}
	}
	return out
}

func TestMountBuildsTheCall(t *testing.T) {
	c, k, m := mountedClient(t)
	if err := c.Mount("tank/a", nil); err != nil {
		t.Fatal(err)
	}
	want := []string{"defaults", "atime", "devices", "exec", "rw", "setuid", "nonbmand", "zfsutil"}
	if !slices.Equal(m.opts["tank/a"], want) {
		t.Errorf("options = %v, want %v", m.opts["tank/a"], want)
	}
	if m.table["tank/a"] != "/tank/a" || !m.dirs["/tank/a"] {
		t.Errorf("table = %v, dirs = %v", m.table, m.dirs)
	}

	// Properties turn into options; given options replace "defaults".
	for _, p := range []string{"atime", "exec", "setuid"} {
		k.ds["tank/z"].props[p] = Property{Value: uint64(0), Source: "tank/z"}
	}
	k.ds["tank/z"].props["readonly"] = Property{Value: uint64(1), Source: "tank/z"}
	if err := c.Mount("tank/z", &MountOptions{Options: []string{"nosuid"}}); err != nil {
		t.Fatal(err)
	}
	want = []string{"nosuid", "noatime", "devices", "noexec", "ro", "nosetuid", "nonbmand", "zfsutil"}
	if !slices.Equal(m.opts["tank/z"], want) {
		t.Errorf("options = %v, want %v", m.opts["tank/z"], want)
	}

	// Mounted twice is busy, unless remounting.
	if err := c.Mount("tank/a", nil); !errors.Is(err, syscall.EBUSY) {
		t.Errorf("second mount: %v", err)
	}
	if err := c.Mount("tank/a", &MountOptions{Remount: true}); err != nil {
		t.Errorf("remount: %v", err)
	}
	if o := m.opts["tank/a"]; o[0] != "update" {
		t.Errorf("remount options = %v", o)
	}
}

func TestMountNotMountable(t *testing.T) {
	c, k, m := mountedClient(t)
	str := func(v string) Property { return Property{Value: v, Source: "tank/z"} }
	num := func(v uint64) Property { return Property{Value: v, Source: "tank/z"} }
	for name, tc := range map[string]struct {
		prop string
		val  Property
		ds   string
	}{
		"none":   {"mountpoint", str("none"), "tank/z"},
		"legacy": {"mountpoint", str("legacy"), "tank/z"},
		"off":    {"canmount", num(0), "tank/z"},
		"jailed": {"jailed", num(1), "tank/z"},
		"volume": {"", Property{}, "tank/vol"},
		"redact": {"redacted", num(1), "tank/z"},
	} {
		t.Run(name, func(t *testing.T) {
			if tc.prop != "" {
				k.ds[tc.ds].props[tc.prop] = tc.val
				defer delete(k.ds[tc.ds].props, tc.prop)
			}
			err := c.Mount(tc.ds, nil)
			var nm *NotMountableError
			if !errors.Is(err, ErrNotMountable) || !errors.As(err, &nm) {
				t.Fatalf("err = %v", err)
			}
			if len(m.table) != 0 {
				t.Errorf("mounted anyway: %v", m.table)
			}
		})
	}
	if err := c.Mount("tank/nothere", nil); !errors.Is(err, syscall.ENOENT) {
		t.Errorf("missing: %v", err)
	}
	// Forced, a redacted dataset is mounted.
	k.ds["tank/z"].props["redacted"] = Property{Value: uint64(1)}
	if err := c.Mount("tank/z", &MountOptions{Force: true}); err != nil {
		t.Errorf("forced: %v", err)
	}
	// canmount=noauto is mounted by an explicit call.
	delete(k.ds["tank/z"].props, "redacted")
	k.ds["tank/z"].props["canmount"] = Property{Value: uint64(2)}
	if _, err := c.MountPoint("tank/z"); err != nil {
		t.Errorf("noauto: %v", err)
	}
	// A jail may mount a jailed dataset.
	m.jail = true
	k.ds["tank/z"].props["jailed"] = Property{Value: uint64(1)}
	if _, err := c.MountPoint("tank/z"); err != nil {
		t.Errorf("in a jail: %v", err)
	}
}

func TestMountDirectoryRules(t *testing.T) {
	c, k, m := mountedClient(t)
	m.nonEmpty["/tank/a"] = true
	k.ds["tank/a"].props["overlay"] = Property{Value: uint64(0)}
	err := c.Mount("tank/a", nil)
	var me *MountError
	if !errors.Is(err, ErrMountpointNotEmpty) || !errors.As(err, &me) || me.Mountpoint != "/tank/a" {
		t.Fatalf("err = %v", err)
	}
	if err := c.Mount("tank/a", &MountOptions{Overlay: true}); err != nil {
		t.Errorf("overlay option: %v", err)
	}
	c.Unmount("tank/a", nil)
	k.ds["tank/a"].props["overlay"] = Property{Value: uint64(1)}
	if err := c.Mount("tank/a", nil); err != nil {
		t.Errorf("overlay property: %v", err)
	}
	// overlay is on by default, as in OpenZFS
	delete(k.ds["tank/a"].props, "overlay")
	c.Unmount("tank/a", nil)
	if err := c.Mount("tank/a", nil); err != nil {
		t.Errorf("default overlay: %v", err)
	}
	// The kernel's own error is kept.
	m.failMnt["tank/z"] = syscall.EPERM
	if err := c.Mount("tank/z", nil); !errors.Is(err, syscall.EPERM) {
		t.Errorf("err = %v", err)
	}
}

func TestMountEncrypted(t *testing.T) {
	c, k, m := mountedClient(t)
	k.ds["tank/z"].props["encryption"] = Property{Value: uint64(8)} // aes-256-gcm
	k.ds["tank/z"].props["keystatus"] = Property{Value: uint64(1)}  // unavailable
	k.ds["tank/z"].props["encryptionroot"] = Property{Value: "tank/z"}
	if err := c.Mount("tank/z", nil); !errors.Is(err, ErrKeyNotLoaded) {
		t.Fatalf("without a key: %v", err)
	}
	if len(m.table) != 0 {
		t.Fatal("mounted without a key")
	}
	if err := c.Mount("tank/z", &MountOptions{Key: RawKey(make([]byte, 32))}); err != nil {
		t.Fatal(err)
	}
	var loaded bool
	for _, w := range k.writes {
		loaded = loaded || (w.ioc == ioctl.IOCLoadKey && w.name == "tank/z")
	}
	if !loaded || m.table["tank/z"] == "" {
		t.Errorf("key not loaded (%v) or not mounted (%v)", loaded, m.table)
	}
}

func TestUnmount(t *testing.T) {
	c, k, m := mountedClient(t)
	if err := c.Unmount("tank/a", nil); err != nil {
		t.Errorf("not mounted: %v", err)
	}
	c.Mount("tank/a", nil)
	m.failUmnt["/tank/a"] = syscall.EBUSY
	err := c.Unmount("tank/a", nil)
	var me *MountError
	if !errors.Is(err, syscall.EBUSY) || !errors.As(err, &me) || me.Op != "unmount" {
		t.Fatalf("busy: %v", err)
	}
	delete(m.failUmnt, "/tank/a")
	if err := c.Unmount("tank/a", &UnmountOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	if got := k.mountCalls(); got[len(got)-1] != "unmount /tank/a force" || len(m.table) != 0 {
		t.Errorf("calls = %v table = %v", got, m.table)
	}

	// UnloadKey only for an encryption root with a key.
	k.ds["tank/z"].props["encryption"] = Property{Value: uint64(8)}
	k.ds["tank/z"].props["keystatus"] = Property{Value: uint64(2)}
	k.ds["tank/z"].props["encryptionroot"] = Property{Value: "tank/z"}
	if err := c.Unmount("tank/z", &UnmountOptions{UnloadKey: true}); err != nil {
		t.Fatal(err)
	}
	if w := k.writes[len(k.writes)-1]; w.ioc != ioctl.IOCUnloadKey || w.name != "tank/z" {
		t.Errorf("last write = %v %s", w.ioc, w.name)
	}
	k.writes = nil
	k.ds["tank/a/child"].props["encryption"] = Property{Value: uint64(8)}
	k.ds["tank/a/child"].props["keystatus"] = Property{Value: uint64(2)}
	k.ds["tank/a/child"].props["encryptionroot"] = Property{Value: "tank/a"}
	c.Unmount("tank/a/child", &UnmountOptions{UnloadKey: true})
	if len(k.writes) != 0 {
		t.Errorf("unloaded the key of a non-root: %v", k.writes)
	}
}

func TestMountAllAndUnmountAll(t *testing.T) {
	c, k, m := mountedClient(t)
	k.ds["tank/z"].props["canmount"] = Property{Value: uint64(0)}
	k.ds["tank/a/child"].props["mountpoint"] = Property{Value: "/deep/er/child", Source: "tank/a/child"}
	k.ds["tank/clone"].props["canmount"] = Property{Value: uint64(2)} // noauto
	c.Mount("tank", nil)                                              // already mounted: skipped
	k.calls = nil
	if err := c.MountAll("tank", nil); err != nil {
		t.Fatal(err)
	}
	// Shallow mountpoints first; off, noauto and volumes are left alone.
	want := []string{"mount tank/a /tank/a", "mount tank/a/child /deep/er/child"}
	if got := k.mountCalls(); !slices.Equal(got, want) {
		t.Errorf("MountAll = %v, want %v", got, want)
	}
	k.calls = nil
	if err := c.UnmountAll("tank", nil); err != nil {
		t.Fatal(err)
	}
	// Deepest mountpoint first.
	want = []string{"unmount /deep/er/child", "unmount /tank/a", "unmount /tank"}
	if got := k.mountCalls(); !slices.Equal(got, want) || len(m.table) != 0 {
		t.Errorf("UnmountAll = %v (table %v), want %v", got, m.table, want)
	}
	// Failures are collected, the rest still happens.
	m.failMnt["tank/a"] = syscall.EPERM
	err := c.MountAll("tank", nil)
	if !errors.Is(err, syscall.EPERM) || m.table["tank"] == "" {
		t.Errorf("err = %v, table = %v", err, m.table)
	}
}

func TestCreateAndCloneMount(t *testing.T) {
	c, k, m := mountedClient(t)
	k.fs("tank/new", 40)
	if err := c.Create("tank/new", nil); err != nil {
		t.Fatal(err)
	}
	if m.table["tank/new"] != "/tank/new" {
		t.Errorf("not mounted: %v", m.table)
	}
	k.fs("tank/nomount", 41)
	c.Create("tank/nomount", &CreateOptions{NoMount: true})
	k.fs("tank/noauto", 42).props["canmount"] = Property{Value: uint64(2)}
	c.Create("tank/noauto", nil)
	k.fs("tank/none", 43).props["mountpoint"] = Property{Value: "none", Source: "tank/none"}
	c.Create("tank/none", nil)
	if len(m.table) != 1 {
		t.Errorf("mounted too much: %v", m.table)
	}
	// A volume is never mounted.
	if err := c.CreateVolume("tank/vol", 1<<20, nil); err != nil {
		t.Fatal(err)
	}
	if len(m.table) != 1 {
		t.Errorf("volume mounted: %v", m.table)
	}
	// Clones of filesystems are mounted.
	k.fs("tank/clone2", 51)
	if err := c.Clone("tank/clone2", "tank/a@s1", nil); err != nil {
		t.Fatal(err)
	}
	if m.table["tank/clone2"] == "" {
		t.Errorf("clone not mounted: %v", m.table)
	}
	// A failing mount leaves the dataset and says so.
	k.fs("tank/bad", 60)
	m.failMnt["tank/bad"] = syscall.EPERM
	err := c.Create("tank/bad", nil)
	var me *MountError
	if !errors.As(err, &me) || me.Op != "mount" || !errors.Is(err, syscall.EPERM) {
		t.Errorf("err = %v", err)
	}
}

// renameTree makes the change of a rename in the fake pool.
func (k *fakeKernel) renameTree(old, new string) {
	for name, d := range k.ds {
		if name == old || strings.HasPrefix(name, old+"/") {
			nn := new + name[len(old):]
			delete(k.ds, name)
			d.name = nn
			if p, ok := d.props["mountpoint"]; ok && p.Source == "default" {
				d.props["mountpoint"] = Property{Value: "/" + nn, Source: "default"}
			}
			k.ds[nn] = d
		}
	}
	for parent, kids := range k.children {
		for i, kid := range kids {
			if kid == old || strings.HasPrefix(kid, old+"/") {
				kids[i] = new + kid[len(old):]
			}
		}
		if parent == old || strings.HasPrefix(parent, old+"/") {
			k.children[new+parent[len(old):]] = kids
			delete(k.children, parent)
		}
	}
}

func TestDestroyUnmountsFirst(t *testing.T) {
	c, k, m := mountedClient(t)
	c.Mount("tank/a", nil)
	c.Mount("tank/a/child", nil)
	k.calls = nil
	if err := c.Destroy("tank/a", &DestroyOptions{Recursive: true}); err != nil {
		t.Fatal(err)
	}
	got := k.mountCalls()
	i1, i2 := slices.Index(got, "unmount /tank/a/child"), slices.Index(got, "unmount /tank/a")
	d1 := slices.IndexFunc(got, func(s string) bool { return strings.HasPrefix(s, "ZFS_IOC_DESTROY") })
	if i1 < 0 || i2 < 0 || !(i1 < i2 && i2 < d1) {
		t.Errorf("order = %v", got)
	}
	if len(m.table) != 0 {
		t.Errorf("still mounted: %v", m.table)
	}

	// A snapshot never touches the mount table.
	k.calls = nil
	c.Destroy("tank/a@s1", nil)
	for _, call := range k.mountCalls() {
		if strings.HasPrefix(call, "unmount") {
			t.Errorf("unexpected %s", call)
		}
	}

	// A destroy that fails puts the mounts back.
	c.Mount("tank/z", nil)
	k.writeErr = map[ioctl.IOC]error{ioctl.IOCDestroy: &ioctl.Error{Ioc: ioctl.IOCDestroy, Name: "tank/z", Errno: syscall.EBUSY}}
	if err := c.Destroy("tank/z", nil); !errors.Is(err, syscall.EBUSY) {
		t.Fatalf("err = %v", err)
	}
	if m.table["tank/z"] != "/tank/z" {
		t.Errorf("not mounted again: %v", m.table)
	}
	// A busy mount stops the destroy before the kernel is asked.
	k.writes, k.writeErr = nil, nil
	m.failUmnt["/tank/z"] = syscall.EBUSY
	if err := c.Destroy("tank/z", nil); !errors.Is(err, syscall.EBUSY) || len(k.writes) != 0 {
		t.Errorf("err = %v, writes = %v", err, k.writes)
	}
	// Force reaches unmount.
	delete(m.failUmnt, "/tank/z")
	k.calls = nil
	c.Destroy("tank/z", &DestroyOptions{Force: true})
	if !slices.Contains(k.calls, "unmount /tank/z force") {
		t.Errorf("calls = %v", k.calls)
	}
}

func TestRenameRemountsTheTree(t *testing.T) {
	c, k, m := mountedClient(t)
	c.Mount("tank/a", nil)
	c.Mount("tank/a/child", nil)
	k.after = func(w writeCall) {
		if w.ioc == ioctl.IOCRename {
			k.renameTree(w.name, w.value)
		}
	}
	k.calls = nil
	if err := c.Rename("tank/a", "tank/b", false); err != nil {
		t.Fatal(err)
	}
	want := []string{"unmount /tank/a/child", "unmount /tank/a", "ZFS_IOC_RENAME tank/a",
		"mount tank/b /tank/b", "mount tank/b/child /tank/b/child"} // old directories stay: only a mountpoint change removes them
	if got := k.mountCalls(); !slices.Equal(got, want) {
		t.Errorf("calls = %v\nwant    %v", got, want)
	}
	if len(m.table) != 2 || m.table["tank/b/child"] != "/tank/b/child" {
		t.Errorf("table = %v", m.table)
	}
	// A snapshot rename does not touch mounts.
	k.calls = nil
	c.Rename("tank/b@x", "tank/b@y", false)
	if got := k.mountCalls(); len(got) != 1 {
		t.Errorf("calls = %v", got)
	}
}

func TestSetMountpointMovesInheritingChildren(t *testing.T) {
	c, k, m := mountedClient(t)
	// child inherits the mountpoint from tank/a, other sets its own.
	k.ds["tank/a"].props["mountpoint"] = Property{Value: "/old", Source: "tank/a"}
	k.ds["tank/a/child"].props["mountpoint"] = Property{Value: "/old", Source: "tank/a"}
	k.fs("tank/a/other", 70).props["mountpoint"] = Property{Value: "/elsewhere", Source: "tank/a/other"}
	k.children["tank/a"] = append(k.children["tank/a"], "tank/a/other")
	c.Mount("tank/a", nil)
	c.Mount("tank/a/child", nil)
	c.Mount("tank/a/other", nil)
	k.after = func(w writeCall) {
		if w.ioc == ioctl.IOCSetProp {
			if v, ok := w.in.String("mountpoint"); ok {
				k.ds["tank/a"].props["mountpoint"] = Property{Value: v, Source: "tank/a"}
				k.ds["tank/a/child"].props["mountpoint"] = Property{Value: v, Source: "tank/a"}
			}
		}
	}
	k.calls = nil
	if err := c.SetProp("tank/a", "mountpoint", "/new"); err != nil {
		t.Fatal(err)
	}
	want := []string{"unmount /old/child", "unmount /old", "ZFS_IOC_SET_PROP tank/a", "mount tank/a /new",
		"mount tank/a/child /new/child", "rmdir /old/child", "rmdir /old"}
	if got := k.mountCalls(); !slices.Equal(got, want) {
		t.Errorf("calls = %v\nwant    %v", got, want)
	}
	if m.table["tank/a/other"] != "/elsewhere" {
		t.Errorf("a child with its own mountpoint moved: %v", m.table)
	}

	// An unrelated property does not touch mounts.
	k.calls = nil
	c.SetProp("tank/a", "compression", "lz4")
	if got := k.mountCalls(); len(got) != 1 {
		t.Errorf("calls = %v", got)
	}
}

func TestSetCanmount(t *testing.T) {
	c, k, m := mountedClient(t)
	c.Mount("tank/a", nil)
	k.after = func(w writeCall) {
		if w.ioc == ioctl.IOCSetProp {
			if v, ok := w.in.Uint64("canmount"); ok {
				k.ds[w.name].props["canmount"] = Property{Value: v, Source: w.name}
			}
		}
	}
	// off: unmounted and stays so
	if err := c.SetProp("tank/a", "canmount", "off"); err != nil {
		t.Fatal(err)
	}
	if len(m.table) != 0 {
		t.Errorf("table = %v", m.table)
	}
	// on: mounted although it was not before
	if err := c.SetProp("tank/a", "canmount", "on"); err != nil {
		t.Fatal(err)
	}
	if m.table["tank/a"] != "/tank/a" {
		t.Errorf("table = %v", m.table)
	}
	// inherit goes the same way
	c.Unmount("tank/a", nil)
	k.calls = nil
	if err := c.InheritProp("tank/a", "mountpoint"); err != nil {
		t.Fatal(err)
	}
	if got := k.mountCalls(); len(got) != 1 {
		t.Errorf("calls = %v", got)
	}
}

func TestMountWithoutAMounter(t *testing.T) {
	k := newFake(t)
	c := newClient(k)
	if _, err := c.Mounts(); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("Mounts: %v", err)
	}
	if err := c.Mount("tank/a", nil); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("Mount: %v", err)
	}
	// Everything that only mounts on the side keeps working.
	if err := c.Create("tank/z2", nil); err != nil {
		t.Errorf("Create: %v", err)
	}
	if err := c.Destroy("tank/z", nil); err != nil {
		t.Errorf("Destroy: %v", err)
	}
}

func TestAutoMountSkipsAnEncryptedDatasetWithoutItsKey(t *testing.T) {
	c, k, m := mountedClient(t)
	k.fs("tank/raw", 40).props["encryption"] = Property{Value: uint64(8)}
	k.ds["tank/raw"].props["keystatus"] = Property{Value: uint64(1)}
	if err := c.Create("tank/raw", nil); err != nil {
		t.Fatalf("a dataset waiting for its key is not an error: %v", err)
	}
	if len(m.table) != 0 {
		t.Errorf("mounted: %v", m.table)
	}
}
