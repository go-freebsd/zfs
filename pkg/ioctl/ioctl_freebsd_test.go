//go:build freebsd

package ioctl

import (
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"runtime"
	"testing"

	"github.com/go-freebsd/nvlist/pkg/nvlist"
)

// These tests need a loaded ZFS module, root, and a pool named by
// ZFS_TEST_POOL (CI creates one on a file vdev). They are the first
// end-to-end proof of the zfs_iocparm wrapper, the zfs_cmd_t layout, the
// ioctl numbers and the nvlist codec against a real kernel.

func testConn(t *testing.T) (*Conn, string) {
	t.Helper()
	pool := os.Getenv("ZFS_TEST_POOL")
	if pool == "" {
		t.Skip("set ZFS_TEST_POOL to run against a real pool")
	}
	c, err := Open()
	if err != nil {
		t.Fatalf("ZFS_TEST_POOL is set but /dev/zfs cannot be opened: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	// Log raw packed lists so they can be turned into golden test vectors.
	c.Trace = func(ioc IOC, name string, src, dst []byte) {
		t.Logf("TRACE %v %q src=%d bytes dst=%d bytes\n  dst hex: %s", ioc, name, len(src), len(dst), hex.EncodeToString(dst))
	}
	return c, pool
}

func TestPoolConfigs(t *testing.T) {
	c, pool := testConn(t)
	cmd := NewCmd()
	out, err := c.Do(IOCPoolConfigs, cmd, nil)
	if err != nil {
		t.Fatalf("POOL_CONFIGS: %v", err)
	}
	if out == nil {
		t.Fatal("POOL_CONFIGS returned no list")
	}
	t.Logf("cookie %d\n%s", cmd.Cookie, out.Format())
	if !out.Has(pool) {
		t.Errorf("pool %q not in %v", pool, out.Names())
	}
}

func TestObjsetStats(t *testing.T) {
	c, pool := testConn(t)
	cmd := NewCmd()
	cmd.SetName(pool)
	out, err := c.Do(IOCObjsetStats, cmd, nil)
	if err != nil {
		t.Fatalf("OBJSET_STATS %s: %v", pool, err)
	}
	t.Logf("stats: type=%d guid=%d creation_txg=%d is_snapshot=%d\n%s",
		cmd.ObjsetStats.Type, cmd.ObjsetStats.GUID, cmd.ObjsetStats.CreationTxg, cmd.ObjsetStats.IsSnapshot, out.Format())
	if cmd.ObjsetStats.Type != 2 { // DMU_OST_ZFS
		t.Errorf("dds_type = %d, want 2 (zfs filesystem): zfs_cmd_t layout is off", cmd.ObjsetStats.Type)
	}
	if cmd.ObjsetStats.GUID == 0 {
		t.Error("dds_guid is 0")
	}
	if out == nil || out.Len() == 0 {
		t.Fatal("no properties returned")
	}
	// Every property is a nested {value, source} list.
	used, ok := out.List("used")
	if !ok {
		t.Fatalf("no \"used\" property in %v", out.Names())
	}
	if _, ok := used.Uint64("value"); !ok {
		t.Errorf("used = %s", used.Format())
	}
}

func TestNotFound(t *testing.T) {
	c, _ := testConn(t)
	_, err := c.Call(IOCObjsetStats, "gojail_no_such_pool_xyz", nil)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v, want ENOENT", err)
	}
	var e *Error
	if !errors.As(err, &e) || e.Ioc != IOCObjsetStats {
		t.Errorf("err = %#v", err)
	}
}

// A successful snapshot proves the packed input is accepted by the kernel's
// strict nvlist_unpack; stat and destroy then exercise three more ioctls.
func TestSnapshotLifecycle(t *testing.T) {
	c, pool := testConn(t)
	snap := pool + "@ioctl_smoke"

	in := nvlist.New()
	snaps := nvlist.New()
	snaps.Add(snap, nvlist.Flag{})
	in.Add("snaps", snaps)

	t.Cleanup(func() {
		d := nvlist.New()
		d.Add("snaps", snaps)
		c.Call(IOCDestroySnaps, pool, d)
	})

	if out, err := c.Call(IOCSnapshot, pool, in); err != nil {
		t.Fatalf("SNAPSHOT: %v", err)
	} else if out != nil && out.Len() > 0 {
		t.Fatalf("SNAPSHOT reported per-snapshot errors:\n%s", out.Format())
	}

	cmd := NewCmd()
	cmd.SetName(snap)
	if _, err := c.Do(IOCObjsetStats, cmd, nil); err != nil {
		t.Fatalf("OBJSET_STATS %s: %v", snap, err)
	}
	if cmd.ObjsetStats.IsSnapshot != 1 {
		t.Errorf("dds_is_snapshot = %d, want 1", cmd.ObjsetStats.IsSnapshot)
	}

	d := nvlist.New()
	d.Add("snaps", snaps)
	if out, err := c.Call(IOCDestroySnaps, pool, d); err != nil {
		t.Fatalf("DESTROY_SNAPS: %v", err)
	} else if out != nil && out.Len() > 0 {
		t.Fatalf("DESTROY_SNAPS reported errors:\n%s", out.Format())
	}
	if _, err := c.Call(IOCObjsetStats, snap, nil); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("snapshot still exists after destroy: %v", err)
	}
}

// Known answers per ioctl area, to catch an ioctl table that is off by some
// entries on a given FreeBSD release.
func TestIoctlNumbersKnownAnswers(t *testing.T) {
	c, pool := testConn(t)

	// GET_HOLDS on a dataset that is not a snapshot is EINVAL, not ENOSYS or
	// ENOTTY; a wrong table would hit some other ioctl and fail differently.
	in := nvlist.New()
	if _, err := c.Call(IOCGetHolds, pool+"@gojail_missing", in); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("GET_HOLDS on a missing snapshot: %v, want ENOENT", err)
	}
	// GET_BOOKMARKS on an existing dataset with no bookmarks succeeds.
	if out, err := c.Call(IOCGetBookmarks, pool, nvlist.New()); err != nil {
		t.Errorf("GET_BOOKMARKS: %v", err)
	} else if out != nil && out.Len() != 0 {
		t.Errorf("unexpected bookmarks: %s", out.Format())
	}
	// SEND_SPACE of a missing snapshot is ENOENT.
	if _, err := c.Call(IOCSendSpace, pool+"@gojail_missing", nvlist.New()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("SEND_SPACE on a missing snapshot: %v, want ENOENT", err)
	}
}

func TestNoPreemptBlocksAsyncSignalsOnlyForTheCall(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	for _, sig := range []int{sigURG, sigCHLD} {
		if threadSignalBlocked(sig) {
			t.Skipf("signal %d is blocked to begin with", sig)
		}
	}
	done := noPreempt()
	for _, sig := range []int{sigURG, sigCHLD} {
		if !threadSignalBlocked(sig) {
			t.Errorf("signal %d is not blocked during the call", sig)
		}
	}
	done()
	for _, sig := range []int{sigURG, sigCHLD} {
		if threadSignalBlocked(sig) {
			t.Errorf("signal %d stays blocked after the call", sig)
		}
	}
}
