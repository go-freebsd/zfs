//go:build freebsd

package primitives

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// readOnlyPool builds a small pool on a file, puts a tree with snapshots in it
// and imports it again with readonly=on, without mounting anything. It skips
// the test where a second pool cannot be made.
func readOnlyPool(t *testing.T) (c *Client, pool string) {
	t.Helper()
	testPool(t) // the CI environment: root, a loaded module
	var err error
	c, err = Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })

	dir := t.TempDir()
	vdev := filepath.Join(dir, "vdev")
	f, err := os.Create(vdev)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(96 << 20); err != nil {
		t.Fatal(err)
	}
	f.Close()
	pool = fmt.Sprintf("rotest%d", os.Getpid())
	mnt := filepath.Join(dir, "mnt")
	if out, err := exec.Command("zpool", "create", "-f", "-o", "cachefile=none", "-O", "mountpoint="+mnt, pool, vdev).CombinedOutput(); err != nil {
		t.Skipf("cannot make a second pool on a file: %v\n%s", err, out)
	}
	t.Cleanup(func() { exec.Command("zpool", "export", "-f", pool).Run() })

	for _, d := range []string{pool + "/fs", pool + "/fs/kid"} {
		cli(t, "zfs", "create", d)
	}
	for _, d := range []string{pool + "/fs", pool + "/fs/kid"} {
		mp := cli(t, "zfs", "get", "-H", "-o", "value", "mountpoint", d)
		if err := os.WriteFile(filepath.Join(mp, "f"), []byte("data of "+d), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cli(t, "zfs", "snapshot", "-r", pool+"/fs@a")
	cli(t, "zfs", "snapshot", "-r", pool+"/fs@b")
	cli(t, "zpool", "export", pool)
	if out, err := exec.Command("zpool", "import", "-N", "-o", "readonly=on", "-o", "cachefile=none", "-d", dir, pool).CombinedOutput(); err != nil {
		t.Skipf("cannot import a pool read-only: %v\n%s", err, out)
	}
	if got := cli(t, "zpool", "get", "-H", "-o", "value", "readonly", pool); got != "on" {
		t.Fatalf("the pool is not read-only (%q): the test would prove nothing", got)
	}
	return c, pool
}

func TestReadOnlyPoolOnARealKernel(t *testing.T) {
	c, pool := readOnlyPool(t)
	fs, kid := pool+"/fs", pool+"/fs/kid"

	if ro, err := c.PoolReadOnly(pool); err != nil || !ro {
		t.Fatalf("PoolReadOnly(%s) = %v, %v", pool, ro, err)
	}
	if props, err := c.PoolProps(pool); err != nil || props["size"] == nil {
		t.Errorf("PoolProps: %v, %v", props, err)
	}

	t.Run("mounts are read-only", func(t *testing.T) {
		// What zfs mount makes of it, as the reference.
		cli(t, "zfs", "mount", fs)
		mp := cli(t, "zfs", "get", "-H", "-o", "value", "mountpoint", fs)
		_, refErr := os.Create(filepath.Join(mp, "new"))
		if !errors.Is(refErr, syscall.EROFS) {
			t.Fatalf("zfs mount: a write gave %v, want EROFS: the reference proves nothing", refErr)
		}
		cli(t, "zfs", "unmount", fs)

		if err := c.MountAll(fs, nil); err != nil {
			t.Fatal(err)
		}
		if got := mountedBelow(t, fs); len(got) != 2 {
			t.Fatalf("mounted: %q", got)
		}
		for _, d := range []string{fs, kid} {
			mp := cli(t, "zfs", "get", "-H", "-o", "value", "mountpoint", d)
			if body, err := os.ReadFile(filepath.Join(mp, "f")); err != nil || !strings.HasPrefix(string(body), "data of ") {
				t.Errorf("%s: %q, %v", d, body, err)
			}
			if _, err := os.Create(filepath.Join(mp, "new")); !errors.Is(err, syscall.EROFS) {
				t.Errorf("%s: a write gave %v, want EROFS", d, err)
			}
		}
		if err := c.UnmountAll(fs, nil); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("replication send", func(t *testing.T) {
		for name, tc := range map[string]struct {
			args []string
			opts SendOptions
		}{
			"-R":  {[]string{"-R", fs + "@b"}, SendOptions{Replicate: true}},
			"-Rh": {[]string{"-R", "-h", fs + "@b"}, SendOptions{Replicate: true, Holds: true}},
			"-RI": {[]string{"-R", "-I", "@a", fs + "@b"}, SendOptions{Replicate: true, Intermediates: true, From: "@a"}},
		} {
			want := theirs(t, tc.args...)
			o := tc.opts
			got := ours(t, c, fs+"@b", &o) // no hold can be taken: Send must not need one
			ws, gs := shapeOf(t, want), shapeOf(t, got)
			if ws.header != gs.header || strings.Join(ws.subs, "\n") != strings.Join(gs.subs, "\n") || len(want) != len(got) {
				t.Errorf("%s: differs from zfs send\nzfs:  %s\nhere: %s", name, ws.header, gs.header)
			}
			if len(ws.subs) < 3 {
				t.Errorf("%s: %d streams: the comparison would prove nothing", name, len(ws.subs))
			}
		}
		if h := holdRows(fs); len(h) != 0 {
			t.Errorf("holds on a read-only pool: %v", h)
		}
	})

	t.Run("receive is refused", func(t *testing.T) {
		src := plainTree(t, testPoolName(t), "ro_src")
		stream := theirs(t, "-R", src+"@b")
		target := pool + "/received"
		_, err := c.Receive(context.Background(), bytes.NewReader(stream), target, &ReceiveOptions{NoMount: true})
		if !errors.Is(err, syscall.EROFS) {
			t.Errorf("Receive into a read-only pool: %v, want EROFS", err)
		}
		if ok, _ := c.Exists(target); ok {
			t.Error("a dataset was made in a read-only pool")
		}
		cmd := exec.Command("zfs", "receive", "-u", target)
		cmd.Stdin = bytes.NewReader(stream)
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Errorf("zfs receive into a read-only pool worked: %s", out)
		}
		// The ordinary writes fail the same way.
		if err := c.Create(pool+"/new", nil); !errors.Is(err, syscall.EROFS) {
			t.Errorf("Create: %v", err)
		}
		if err := c.Snapshot([]string{fs + "@c"}, nil); !errors.Is(err, syscall.EROFS) {
			t.Errorf("Snapshot: %v", err)
		}
	})
}

func testPoolName(t *testing.T) string { return testPool(t) }

// The readonly property of a dataset in a writable pool: a receive works, and
// what is mounted is mounted read-only, as zfs mount does.
func TestReadOnlyPropertyOfADataset(t *testing.T) {
	c, pool := testClient(t)
	src := plainTree(t, pool, "rp_src")
	base := theirs(t, "-R", src+"@a")
	incr := theirs(t, "-R", "-i", "@a", src+"@b")

	dst := wds(t, pool, "rp_dst")
	if _, err := c.Receive(context.Background(), bytes.NewReader(base), dst, &ReceiveOptions{NoMount: true}); err != nil {
		t.Fatal(err)
	}
	cli(t, "zfs", "set", "readonly=on", dst, dst+"/kid")
	// An incremental receive into a read-only dataset works (as with zfs receive).
	if _, err := c.Receive(context.Background(), bytes.NewReader(incr), dst, &ReceiveOptions{NoMount: true}); err != nil {
		t.Fatalf("incremental receive into readonly=on datasets: %v", err)
	}
	if guid(t, dst+"@b") != guid(t, src+"@b") {
		t.Error("the snapshot differs")
	}
	dir := tdir(t)
	cli(t, "zfs", "set", "mountpoint="+dir, dst)
	exec.Command("zfs", "unmount", dst).Run() // setting the mountpoint may have mounted it
	if err := c.Mount(dst, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Create(filepath.Join(dir, "new")); !errors.Is(err, syscall.EROFS) {
		t.Errorf("a write into a readonly=on dataset gave %v, want EROFS", err)
	}
}
