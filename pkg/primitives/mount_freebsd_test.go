//go:build freebsd

package primitives

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
)

// Mounting against a real kernel. Each case is done twice, once with this
// library and once with zfs(8) on a twin dataset, and the results must be the
// same: whether it mounted, where, with which flags.

// mountFlags returns the flags "mount -t zfs" shows for the dataset.
func mountFlags(t *testing.T, ds string) string {
	t.Helper()
	for _, l := range lines(cli(t, "mount", "-t", "zfs")) {
		if strings.HasPrefix(l, ds+" on ") {
			i := strings.LastIndex(l, "(")
			f := strings.Split(strings.Trim(l[i:], "()"), ", ")
			sort.Strings(f)
			return strings.Join(f, ",")
		}
	}
	return "not mounted"
}

// zmounted returns zfs get mounted.
func zmounted(t *testing.T, ds string) string {
	t.Helper()
	return cli(t, "zfs", "get", "-H", "-o", "value", "mounted", ds)
}

// twinCreate makes the dataset with zfs create, unmounted.
func twinCreate(t *testing.T, ds string, props ...string) {
	t.Helper()
	args := []string{"create", "-u"}
	for _, p := range props {
		args = append(args, "-o", p)
	}
	cli(t, "zfs", append(args, ds)...)
}

func TestCreateMountsLikeZfsCreate(t *testing.T) {
	c, pool := testClient(t)
	for name, props := range map[string]map[string]any{
		"plain":   {},
		"options": {"atime": false, "setuid": false, "exec": false, "readonly": true},
		"zstd":    {"compression": "zstd"},
	} {
		t.Run(name, func(t *testing.T) {
			ours, twin := wds(t, pool, "mc_"+name), wds(t, pool, "mc_twin_"+name)
			dirA, dirB := tdir(t), tdir(t)
			p := map[string]any{"mountpoint": dirA}
			tp := []string{"mountpoint=" + dirB}
			for k, v := range props {
				p[k] = v
				switch x := v.(type) {
				case bool:
					if x {
						tp = append(tp, k+"=on")
					} else {
						tp = append(tp, k+"=off")
					}
				default:
					tp = append(tp, k+"="+v.(string))
				}
			}
			if err := c.Create(ours, &CreateOptions{Props: p}); err != nil {
				t.Fatal(err)
			}
			twinCreate(t, twin, tp...)
			cli(t, "zfs", "mount", twin)

			if zmounted(t, ours) != "yes" {
				t.Fatalf("%s is not mounted", ours)
			}
			if a, b := mountFlags(t, ours), mountFlags(t, twin); a != b || a == "not mounted" {
				t.Errorf("mount flags: ours %q, zfs mount %q", a, b)
			}
			where, ok, err := c.IsMounted(ours)
			if err != nil || !ok || where != dirA {
				t.Errorf("IsMounted = %q %v %v", where, ok, err)
			}
		})
	}

	t.Run("NoMount", func(t *testing.T) {
		ds := wds(t, pool, "mc_nomount")
		if err := c.Create(ds, &CreateOptions{NoMount: true, Props: map[string]any{"mountpoint": tdir(t)}}); err != nil {
			t.Fatal(err)
		}
		if zmounted(t, ds) != "no" {
			t.Error("NoMount mounted")
		}
	})
	t.Run("a volume and a snapshot clone", func(t *testing.T) {
		vol := wds(t, pool, "mc_vol")
		if err := c.CreateVolume(vol, 1<<20, nil); err != nil {
			t.Fatal(err)
		}
		src := wds(t, pool, "mc_src")
		c.Create(src, &CreateOptions{Props: map[string]any{"mountpoint": tdir(t)}})
		c.Snapshot([]string{src + "@s"}, nil)
		clone := wds(t, pool, "mc_clone")
		if err := c.Clone(clone, src+"@s", &CreateOptions{Props: map[string]any{"mountpoint": tdir(t)}}); err != nil {
			t.Fatal(err)
		}
		if zmounted(t, clone) != "yes" {
			t.Error("clone not mounted")
		}
	})
}

func TestMountRulesMatchZfsMount(t *testing.T) {
	c, pool := testClient(t)
	type tc struct {
		props   map[string]string
		prepare func(dir string) // makes the directory non-empty
		want    error            // what ours must return (nil: mounts)
	}
	for name, tc := range map[string]tc{
		"on":     {map[string]string{}, nil, nil},
		"noauto": {map[string]string{"canmount": "noauto"}, nil, nil},
		"off":    {map[string]string{"canmount": "off"}, nil, ErrNotMountable},
		"none":   {map[string]string{"mountpoint": "none"}, nil, ErrNotMountable},
		"legacy": {map[string]string{"mountpoint": "legacy"}, nil, ErrNotMountable},
		"overlay on, content": {map[string]string{}, func(d string) {
			os.WriteFile(filepath.Join(d, "x"), []byte("x"), 0o644)
		}, nil},
		"overlay off, content": {map[string]string{"overlay": "off"}, func(d string) {
			os.WriteFile(filepath.Join(d, "x"), []byte("x"), 0o644)
		}, ErrMountpointNotEmpty},
	} {
		t.Run(name, func(t *testing.T) {
			ours, twin := wds(t, pool, "mr_"+strings.ReplaceAll(name, " ", "_")), wds(t, pool, "mr_twin_"+strings.ReplaceAll(name, " ", "_"))
			dirA, dirB := tdir(t), tdir(t)
			mk := func(ds, dir string, create func(props map[string]any)) {
				props := map[string]any{"mountpoint": dir}
				for k, v := range tc.props {
					props[k] = v
				}
				create(props)
			}
			mk(ours, dirA, func(p map[string]any) {
				if err := c.Create(ours, &CreateOptions{NoMount: true, Props: p}); err != nil {
					t.Fatal(err)
				}
			})
			mk(twin, dirB, func(p map[string]any) {
				var a []string
				for k, v := range p {
					a = append(a, k+"="+v.(string))
				}
				twinCreate(t, twin, a...)
			})
			if tc.prepare != nil {
				tc.prepare(dirA)
				tc.prepare(dirB)
			}
			err := c.Mount(ours, nil)
			_, cliErr := zrun(t, "", "zfs", "mount", twin)
			if (err == nil) != (cliErr == nil) {
				t.Errorf("ours: %v, zfs mount: %v", err, cliErr)
			}
			if tc.want == nil && err != nil || tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
			if a, b := zmounted(t, ours), zmounted(t, twin); a != b {
				t.Errorf("mounted: ours %s, zfs mount %s", a, b)
			}
			// MountPoint answers the same question without mounting
			_, mpErr := c.MountPoint(ours)
			if errors.Is(tc.want, ErrNotMountable) != errors.Is(mpErr, ErrNotMountable) {
				t.Errorf("MountPoint: %v", mpErr)
			}
		})
	}

	t.Run("missing mountpoint directory is made", func(t *testing.T) {
		ds := wds(t, pool, "mr_mkdir")
		dir := filepath.Join(tdir(t), "a", "b", "c")
		if err := c.Create(ds, &CreateOptions{Props: map[string]any{"mountpoint": dir}}); err != nil {
			t.Fatal(err)
		}
		if zmounted(t, ds) != "yes" {
			t.Error("not mounted")
		}
	})
}

func TestUnmountAndRemount(t *testing.T) {
	c, pool := testClient(t)
	ds := wds(t, pool, "um")
	dir := tdir(t)
	if err := c.Create(ds, &CreateOptions{Props: map[string]any{"mountpoint": dir}}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "f"), []byte("data"), 0o644)

	// busy: a file is open
	f, err := os.Open(filepath.Join(dir, "f"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Unmount(ds, nil); !errors.Is(err, syscall.EBUSY) {
		t.Errorf("busy unmount: %v", err)
	}
	if zmounted(t, ds) != "yes" {
		t.Error("a failed unmount left it unmounted")
	}
	if err := c.Mount(ds, nil); !errors.Is(err, syscall.EBUSY) {
		t.Errorf("mounting twice: %v", err)
	}
	if err := c.Unmount(ds, &UnmountOptions{Force: true}); err != nil {
		t.Fatalf("forced: %v", err)
	}
	f.Close()
	if zmounted(t, ds) != "no" {
		t.Error("still mounted")
	}
	if err := c.Unmount(ds, nil); err != nil {
		t.Errorf("unmounting an unmounted one: %v", err)
	}
	if err := c.Mount(ds, nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "f")); string(b) != "data" {
		t.Errorf("content after remount = %q", b)
	}

	// -o remount keeps it mounted and applies the new flags
	if err := c.SetProp(ds, "readonly", true); err != nil {
		t.Fatal(err)
	}
	if zmounted(t, ds) != "yes" || !strings.Contains(mountFlags(t, ds), "read-only") {
		t.Errorf("after readonly=on: %s %s", zmounted(t, ds), mountFlags(t, ds))
	}
}

func TestEncryptedMountNeedsTheKey(t *testing.T) {
	c, pool := testClient(t)
	ds := wds(t, pool, "enc")
	dir := tdir(t)
	key := PassphraseKey("correct horse battery")
	if err := c.Create(ds, &CreateOptions{Key: key, Props: map[string]any{"encryption": "aes-256-gcm", "mountpoint": dir}}); err != nil {
		t.Fatal(err)
	}
	if zmounted(t, ds) != "yes" {
		t.Fatal("not mounted after create")
	}
	os.WriteFile(filepath.Join(dir, "f"), []byte("secret"), 0o644)
	if err := c.Unmount(ds, &UnmountOptions{UnloadKey: true}); err != nil {
		t.Fatal(err)
	}
	if got := cli(t, "zfs", "get", "-H", "-o", "value", "keystatus", ds); got != "unavailable" {
		t.Fatalf("keystatus = %s", got)
	}
	if err := c.Mount(ds, nil); !errors.Is(err, ErrKeyNotLoaded) {
		t.Errorf("without the key: %v", err)
	}
	if err := c.Mount(ds, &MountOptions{Key: PassphraseKey("wrong wrong wrong")}); err == nil {
		t.Error("a wrong key mounted")
	}
	if err := c.Mount(ds, &MountOptions{Key: key}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "f")); string(b) != "secret" {
		t.Errorf("content = %q", b)
	}
}

// tree makes p, p/a (inherits the mountpoint), p/b (its own) and p/a/c
// (inherits) the same way with this library (ours) or zfs(8), mounted.
func tree(t *testing.T, c *Client, root string, ours bool) (rootDir, otherDir string) {
	t.Helper()
	rootDir, otherDir = tdir(t), tdir(t)
	mk := func(ds string, props ...string) {
		if ours {
			p := map[string]any{}
			for _, kv := range props {
				k, v, _ := strings.Cut(kv, "=")
				p[k] = v
			}
			if err := c.Create(ds, &CreateOptions{Props: p}); err != nil {
				t.Fatalf("%s: %v", ds, err)
			}
		} else {
			args := []string{"create"}
			for _, kv := range props {
				args = append(args, "-o", kv)
			}
			cli(t, "zfs", append(args, ds)...)
		}
	}
	mk(root, "mountpoint="+rootDir)
	mk(root + "/a")
	mk(root+"/b", "mountpoint="+otherDir)
	mk(root + "/a/c")
	return
}

// state describes a tree as zfs reports it, with the directories made
// comparable.
func state(t *testing.T, root string, dirs ...string) []string {
	t.Helper()
	out := cli(t, "zfs", "get", "-r", "-H", "-o", "name,property,value", "mounted,mountpoint,canmount", root)
	var res []string
	for _, l := range lines(out) {
		l = strings.ReplaceAll(l, root, "ROOT")
		for i, d := range dirs {
			l = strings.ReplaceAll(l, d, "DIR"+string(rune('0'+i)))
		}
		res = append(res, l)
	}
	return res
}

func sameState(t *testing.T, what string, a, b []string) {
	t.Helper()
	if len(a) == 0 || len(a) != len(b) {
		t.Fatalf("%s: states %d and %d long", what, len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Errorf("%s: ours %q, zfs %q", what, a[i], b[i])
		}
	}
}

func TestMountpointChangeMovesTheTreeLikeZfsSet(t *testing.T) {
	c, pool := testClient(t)
	ours, twin := wds(t, pool, "cl_ours"), wds(t, pool, "cl_twin")
	oa, ob := tree(t, c, ours, true)
	ta, tb := tree(t, c, twin, false)
	sameState(t, "created", state(t, ours, oa, ob), state(t, twin, ta, tb))

	// move the root: p/a and p/a/c follow, p/b stays
	na, nb := tdir(t), tdir(t)
	if err := c.SetProp(ours, "mountpoint", na); err != nil {
		t.Fatal(err)
	}
	cli(t, "zfs", "set", "mountpoint="+nb, twin)
	sameState(t, "mountpoint set", state(t, ours, na, ob), state(t, twin, nb, tb))
	if b, err := os.Stat(filepath.Join(na, "a", "c")); err != nil || !b.IsDir() {
		t.Errorf("the child is not at the new place: %v", err)
	}

	// canmount off and on
	if err := c.SetProp(ours+"/a", "canmount", "off"); err != nil {
		t.Fatal(err)
	}
	cli(t, "zfs", "set", "canmount=off", twin+"/a")
	sameState(t, "canmount off", state(t, ours, na, ob), state(t, twin, nb, tb))
	if err := c.SetProp(ours+"/a", "canmount", "on"); err != nil {
		t.Fatal(err)
	}
	cli(t, "zfs", "set", "canmount=on", twin+"/a")
	sameState(t, "canmount on", state(t, ours, na, ob), state(t, twin, nb, tb))

	// inherit on a child with its own mountpoint
	if err := c.InheritProp(ours+"/b", "mountpoint"); err != nil {
		t.Fatal(err)
	}
	cli(t, "zfs", "inherit", "mountpoint", twin+"/b")
	sameState(t, "inherit", state(t, ours, na, ob), state(t, twin, nb, tb))
}

func TestRenameAndDestroyUnmountLikeZfs(t *testing.T) {
	c, pool := testClient(t)
	ours, twin := wds(t, pool, "rn_ours"), wds(t, pool, "rn_twin")
	oa, ob := tree(t, c, ours, true)
	ta, tb := tree(t, c, twin, false)

	ournew, twinnew := wds(t, pool, "rn_ours2"), wds(t, pool, "rn_twin2")
	if err := c.Rename(ours, ournew, false); err != nil {
		t.Fatal(err)
	}
	cli(t, "zfs", "rename", twin, twinnew)
	sameState(t, "rename", state(t, ournew, oa, ob), state(t, twinnew, ta, tb))
	// the files are reachable at the mountpoint (it is a fixed path)
	if zmounted(t, ournew+"/a/c") != "yes" {
		t.Error("child not mounted after rename")
	}

	// destroy while mounted, with a file in use: refused, and everything
	// is mounted again
	f, err := os.Create(filepath.Join(ob, "busy"))
	if err != nil {
		t.Fatal(err)
	}
	err = c.Destroy(ournew, &DestroyOptions{Recursive: true})
	if !errors.Is(err, syscall.EBUSY) {
		t.Errorf("destroy with an open file: %v", err)
	}
	for _, ds := range []string{ournew, ournew + "/a", ournew + "/b", ournew + "/a/c"} {
		if zmounted(t, ds) != "yes" {
			t.Errorf("%s is not mounted after the failed destroy", ds)
		}
	}
	// -f takes it away
	if err := c.Destroy(ournew, &DestroyOptions{Recursive: true, Force: true}); err != nil {
		t.Fatalf("forced destroy: %v", err)
	}
	f.Close()
	if ok, _ := c.Exists(ournew); ok {
		t.Error("still there")
	}
	if err := c.Destroy(twinnew, &DestroyOptions{Recursive: true}); err != nil {
		t.Fatalf("destroy of the twin: %v", err)
	}
}

func TestMountAllAndUnmountAllMatchZfsMount(t *testing.T) {
	c, pool := testClient(t)
	ours := wds(t, pool, "all")
	tree(t, c, ours, true)
	if err := c.UnmountAll(ours, nil); err != nil {
		t.Fatal(err)
	}
	for _, l := range lines(cli(t, "zfs", "list", "-r", "-H", "-o", "name,mounted", ours)) {
		if strings.HasSuffix(l, "\tyes") {
			t.Errorf("still mounted: %s", l)
		}
	}
	cli(t, "zfs", "set", "canmount=noauto", ours+"/b")
	if err := c.MountAll(ours, nil); err != nil {
		t.Fatal(err)
	}
	for ds, want := range map[string]string{ours: "yes", ours + "/a": "yes", ours + "/a/c": "yes", ours + "/b": "no"} {
		if got := zmounted(t, ds); got != want {
			t.Errorf("%s mounted = %s, want %s", ds, got, want)
		}
	}
	ms, err := c.Mounts()
	if err != nil {
		t.Fatal(err)
	}
	cliMounts := map[string]bool{}
	for _, r := range rows(strings.ReplaceAll(cli(t, "zfs", "mount"), " ", "\t")) {
		cliMounts[r[0]] = true
	}
	n := 0
	for _, m := range ms {
		if strings.HasPrefix(m.Dataset, ours) {
			n++
			if !cliMounts[m.Dataset] {
				t.Errorf("Mounts lists %s, zfs mount does not", m.Dataset)
			}
		}
	}
	if n != 3 {
		t.Errorf("Mounts has %d of the tree, want 3", n)
	}
}
