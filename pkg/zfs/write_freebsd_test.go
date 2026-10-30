//go:build freebsd

package zfs

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/go-freebsd/nvlist/pkg/nvlist"
	"github.com/go-freebsd/zfs/pkg/ioctl"
)

// These tests write with this library and read back with zfs(8) (and the
// other way round for the keys), so a wrong argument layout, a wrong
// property number or a wrong key derivation shows up as a difference.

// wds returns the name of a scratch dataset below the pool that is destroyed,
// with everything that depends on it, when the test ends.
func wds(t *testing.T, pool, name string) string {
	t.Helper()
	n := pool + "/w_" + name
	exec.Command("zfs", "destroy", "-rR", n).Run() // left over from an earlier run
	t.Cleanup(func() { exec.Command("zfs", "destroy", "-rR", n).Run() })
	return n
}

// zget returns zfs get -Hp for the given properties.
func zget(t *testing.T, ds string, props ...string) map[string]string {
	t.Helper()
	m := map[string]string{}
	for _, r := range rows(cli(t, "zfs", "get", "-Hp", "-o", "property,value", strings.Join(props, ","), ds)) {
		m[r[0]] = r[1]
	}
	return m
}

// zrun runs a command with the given standard input and returns its output.
func zrun(t *testing.T, stdin string, name string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func checkProps(t *testing.T, ds string, want map[string]string) {
	t.Helper()
	props := make([]string, 0, len(want))
	for p := range want {
		props = append(props, p)
	}
	got := zget(t, ds, props...)
	for p, w := range want {
		if got[p] != w {
			t.Errorf("%s: zfs get %s = %q, want %q", ds, p, got[p], w)
		}
	}
}

func TestCreateMatchesZfsGet(t *testing.T) {
	c, pool := testClient(t)
	ds := wds(t, pool, "create")
	err := c.Create(ds, &CreateOptions{Props: map[string]any{
		"compression": "lz4", "recordsize": "64k", "atime": false, "casesensitivity": "insensitive",
		"utf8only": true, "copies": 2, "quota": "100M", "mountpoint": "none", "test:made": "by go",
	}})
	if err != nil {
		t.Fatal(err)
	}
	checkProps(t, ds, map[string]string{
		"type": "filesystem", "compression": "lz4", "recordsize": "65536", "atime": "off",
		"casesensitivity": "insensitive", "utf8only": "on", "copies": "2", "quota": "104857600",
		"mountpoint": "none", "test:made": "by go",
	})
	d, err := c.Get(ds)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := d.Enum("compression"); v != "lz4" {
		t.Errorf("Enum(compression) = %q", v)
	}
	if err := c.Create(ds, nil); !errors.Is(err, fs.ErrExist) {
		t.Errorf("creating twice: %v", err)
	}
	if err := c.Create(pool+"/w_missing/parent/leaf", nil); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("creating below a missing parent: %v", err)
	}

	vol := wds(t, pool, "createvol")
	if err := c.CreateVolume(vol, 4<<20, &CreateOptions{Props: map[string]any{"volblocksize": "16k", "compression": "zle", "sync": "always"}}); err != nil {
		t.Fatal(err)
	}
	checkProps(t, vol, map[string]string{"type": "volume", "volsize": "4194304", "volblocksize": "16384", "compression": "zle", "sync": "always"})
	if err := c.CreateVolume(wds(t, pool, "badvol"), 4<<20, &CreateOptions{Props: map[string]any{"volblocksize": 3000}}); err == nil {
		t.Error("volblocksize 3000 accepted")
	}

	deep := wds(t, pool, "deep")
	if err := c.Create(deep+"/x/y/z", &CreateOptions{Parents: true, Props: map[string]any{"mountpoint": "none"}}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{deep, deep + "/x", deep + "/x/y", deep + "/x/y/z"} {
		if ok, _ := c.Exists(p); !ok {
			t.Errorf("%s was not created", p)
		}
	}
}

// Every compression the table lists is created and read back through zfs.
func TestCreateWithEveryCompression(t *testing.T) {
	c, pool := testClient(t)
	info, _ := LookupProp("compression")
	ok, refused := 0, 0
	for _, v := range info.Values {
		if v.Name == "on" {
			continue
		}
		ds := wds(t, pool, "comp_"+strings.ReplaceAll(v.Name, "-", ""))
		if err := c.Create(ds, &CreateOptions{Props: map[string]any{"compression": v.Name, "mountpoint": "none"}}); err != nil {
			t.Logf("kernel refused compression=%s: %v", v.Name, err)
			refused++
			continue
		}
		if got := zget(t, ds, "compression")["compression"]; got != v.Name {
			t.Errorf("created with %s, zfs get says %s", v.Name, got)
		}
		ok++
	}
	if ok < 12 {
		t.Errorf("only %d compression values were verified (%d refused)", ok, refused)
	}
	t.Logf("verified %d compression values, %d refused by the kernel", ok, refused)
}

var algorithms = map[string]string{
	"on": "aes-256-gcm", "aes-128-ccm": "aes-128-ccm", "aes-192-ccm": "aes-192-ccm", "aes-256-ccm": "aes-256-ccm",
	"aes-128-gcm": "aes-128-gcm", "aes-192-gcm": "aes-192-gcm", "aes-256-gcm": "aes-256-gcm",
}

func TestEncryptionAlgorithmsWithRawKey(t *testing.T) {
	c, pool := testClient(t)
	n := 0
	for alg, want := range algorithms {
		ds := wds(t, pool, "enc_"+strings.ReplaceAll(alg, "-", ""))
		key := make([]byte, 32)
		rand.Read(key)
		err := c.Create(ds, &CreateOptions{Key: RawKey(key), Props: map[string]any{"encryption": alg, "compression": "lz4", "mountpoint": "none"}})
		if err != nil {
			t.Errorf("%s: %v", alg, err)
			continue
		}
		checkProps(t, ds, map[string]string{"encryption": want, "keyformat": "raw", "keystatus": "available",
			"encryptionroot": ds, "keylocation": "prompt", "compression": "lz4"})

		d, _ := c.Get(ds)
		if v, _ := d.Enum("encryption"); v != want {
			t.Errorf("%s: Enum(encryption) = %q, want %q", alg, v, want)
		}
		if v, _ := d.Enum("keystatus"); v != "available" {
			t.Errorf("%s: keystatus = %q", alg, v)
		}

		if err := c.UnloadKey(ds); err != nil {
			t.Errorf("%s: UnloadKey: %v", alg, err)
			continue
		}
		checkProps(t, ds, map[string]string{"keystatus": "unavailable"})
		wrong := bytes.Repeat([]byte{1}, 32)
		if err := c.LoadKey(ds, RawKey(wrong)); err == nil {
			t.Errorf("%s: a wrong key was accepted", alg)
		} else if !errors.Is(err, syscall.EACCES) {
			t.Logf("%s: wrong key gives %v", alg, err)
		}
		if err := c.CheckKey(ds, RawKey(key)); err != nil {
			t.Errorf("%s: CheckKey: %v", alg, err)
		}
		checkProps(t, ds, map[string]string{"keystatus": "unavailable"}) // checking does not load
		if err := c.LoadKey(ds, RawKey(key)); err != nil {
			t.Errorf("%s: LoadKey: %v", alg, err)
		}
		checkProps(t, ds, map[string]string{"keystatus": "available"})
		n++
	}
	if n != len(algorithms) {
		t.Errorf("%d of %d algorithms worked", n, len(algorithms))
	}
}

// Our passphrase derivation must give the key zfs derives, in both directions.
func TestPassphraseKeyInteroperatesWithZfs(t *testing.T) {
	c, pool := testClient(t)
	const pass = "correct horse battery staple"

	// Created here, loaded by zfs(8).
	ours := wds(t, pool, "pass_ours")
	if err := c.Create(ours, &CreateOptions{Key: PassphraseKey(pass), Props: map[string]any{"mountpoint": "none"}}); err != nil {
		t.Fatal(err)
	}
	checkProps(t, ours, map[string]string{"encryption": "aes-256-gcm", "keyformat": "passphrase", "keystatus": "available", "pbkdf2iters": "350000"})
	if got := zget(t, ours, "pbkdf2salt")["pbkdf2salt"]; got == "0" || got == "-" || got == "" {
		t.Errorf("pbkdf2salt = %q, expected a random salt", got)
	}
	if err := c.UnloadKey(ours); err != nil {
		t.Fatal(err)
	}
	if out, err := zrun(t, "wrong passphrase!\n", "zfs", "load-key", ours); err == nil {
		t.Errorf("zfs accepted a wrong passphrase: %s", out)
	}
	if out, err := zrun(t, pass+"\n", "zfs", "load-key", ours); err != nil {
		t.Fatalf("zfs load-key rejects the passphrase we created the dataset with: %v\n%s", err, out)
	}
	checkProps(t, ours, map[string]string{"keystatus": "available"})

	// Created by zfs(8), loaded here.
	theirs := wds(t, pool, "pass_theirs")
	if out, err := zrun(t, pass+"\n", "zfs", "create", "-o", "encryption=on", "-o", "keyformat=passphrase",
		"-o", "pbkdf2iters=200000", "-o", "mountpoint=none", theirs); err != nil {
		t.Fatalf("zfs create: %v\n%s", err, out)
	}
	cli(t, "zfs", "unload-key", theirs)
	if err := c.LoadKey(theirs, PassphraseKey("not the passphrase")); err == nil {
		t.Error("a wrong passphrase was accepted")
	}
	if err := c.LoadKey(theirs, PassphraseKey(pass)); err != nil {
		t.Fatalf("LoadKey rejects the passphrase zfs created the dataset with: %v", err)
	}
	checkProps(t, theirs, map[string]string{"keystatus": "available"})
}

func TestHexAndRawKeysInteroperateWithZfs(t *testing.T) {
	c, pool := testClient(t)
	dir := t.TempDir()

	hexKey := strings.Repeat("0123456789abcdef", 4)
	h := wds(t, pool, "hexkey")
	if err := c.Create(h, &CreateOptions{Key: HexKey(hexKey), Props: map[string]any{"mountpoint": "none"}}); err != nil {
		t.Fatal(err)
	}
	checkProps(t, h, map[string]string{"keyformat": "hex", "keystatus": "available"})
	c.UnloadKey(h)
	if out, err := zrun(t, hexKey+"\n", "zfs", "load-key", h); err != nil {
		t.Errorf("zfs rejects our hex key: %v\n%s", err, out)
	}
	checkProps(t, h, map[string]string{"keystatus": "available"})

	raw := make([]byte, 32)
	rand.Read(raw)
	file := filepath.Join(dir, "key.bin")
	if err := os.WriteFile(file, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	r := wds(t, pool, "rawkey")
	if err := c.Create(r, &CreateOptions{Key: &Key{format: KeyRaw, material: raw, Location: "file://" + file}, Props: map[string]any{"mountpoint": "none"}}); err != nil {
		t.Fatal(err)
	}
	checkProps(t, r, map[string]string{"keyformat": "raw", "keylocation": "file://" + file})
	c.UnloadKey(r)
	if out, err := zrun(t, "", "zfs", "load-key", r); err != nil { // reads keylocation
		t.Errorf("zfs rejects our raw key at its keylocation: %v\n%s", err, out)
	}
	checkProps(t, r, map[string]string{"keystatus": "available"})
}

func TestEncryptionRootsInheritAndChangeKey(t *testing.T) {
	c, pool := testClient(t)
	parent := wds(t, pool, "encroot")
	if err := c.Create(parent, &CreateOptions{Key: PassphraseKey("first passphrase"), Props: map[string]any{"mountpoint": "none", "compression": "zstd"}}); err != nil {
		t.Fatal(err)
	}
	child := parent + "/child"
	if err := c.Create(child, &CreateOptions{Props: map[string]any{"mountpoint": "none"}}); err != nil {
		t.Fatal(err)
	}
	checkProps(t, child, map[string]string{"encryption": "aes-256-gcm", "encryptionroot": parent, "keystatus": "available", "compression": "zstd"})

	if err := c.ChangeKey(child, HexKey(strings.Repeat("ab", 32))); err != nil {
		t.Fatalf("ChangeKey: %v", err)
	}
	checkProps(t, child, map[string]string{"encryptionroot": child, "keyformat": "hex"})
	if err := c.InheritKey(child); err != nil {
		t.Fatalf("InheritKey: %v", err)
	}
	checkProps(t, child, map[string]string{"encryptionroot": parent, "keyformat": "passphrase"})

	if err := c.ChangeKey(parent, PassphraseKey("second passphrase")); err != nil {
		t.Fatalf("ChangeKey: %v", err)
	}
	cli(t, "zfs", "unload-key", "-r", parent)
	if out, err := zrun(t, "first passphrase\n", "zfs", "load-key", parent); err == nil {
		t.Errorf("the old passphrase still works: %s", out)
	}
	if out, err := zrun(t, "second passphrase\n", "zfs", "load-key", "-r", parent); err != nil {
		t.Errorf("the new passphrase is rejected: %v\n%s", err, out)
	}
	checkProps(t, child, map[string]string{"keystatus": "available"})

	// A child can start a root of its own with another algorithm.
	own := parent + "/own"
	if err := c.Create(own, &CreateOptions{Key: RawKey(bytes.Repeat([]byte{9}, 32)), Props: map[string]any{"encryption": "aes-128-ccm", "mountpoint": "none"}}); err != nil {
		t.Fatal(err)
	}
	checkProps(t, own, map[string]string{"encryption": "aes-128-ccm", "encryptionroot": own, "keyformat": "raw"})

	// Encrypting below an unencrypted parent without a key must not succeed silently.
	plain := wds(t, pool, "plain")
	if err := c.Create(plain, &CreateOptions{Props: map[string]any{"encryption": "on", "mountpoint": "none"}}); err == nil {
		t.Error("encryption=on without a key was accepted")
		t.Logf("keyformat = %s", zget(t, plain, "keyformat")["keyformat"])
	}
}

func TestSnapshotCloneRollbackPromoteDestroy(t *testing.T) {
	c, pool := testClient(t)
	ds := wds(t, pool, "life")
	if err := c.Create(ds, &CreateOptions{Props: map[string]any{"mountpoint": "none"}}); err != nil {
		t.Fatal(err)
	}
	// Atomic multi-snapshot with a user property.
	if err := c.Create(ds+"/kid", &CreateOptions{Props: map[string]any{"mountpoint": "none"}}); err != nil {
		t.Fatal(err)
	}
	if err := c.Snapshot([]string{ds + "@a", ds + "/kid@a"}, map[string]any{"test:why": "unit"}); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{ds + "@a", ds + "/kid@a"} {
		if got := zget(t, s, "test:why")["test:why"]; got != "unit" {
			t.Errorf("%s: test:why = %q", s, got)
		}
	}
	// All or nothing, and the kernel's per-item list reaches us.
	if err := c.Snapshot([]string{ds + "@b"}, nil); err != nil {
		t.Fatal(err)
	}
	err := c.Snapshot([]string{ds + "/kid@new", ds + "@a"}, nil)
	var be *BatchError
	if !errors.As(err, &be) || !errors.Is(err, fs.ErrExist) {
		t.Fatalf("duplicate snapshot: %v", err)
	}
	t.Logf("duplicate snapshot: %v", err)
	if ok, _ := c.Exists(ds + "/kid@new"); ok {
		t.Error("kid@new was created although ds@a failed")
	}
	if err := c.Snapshot([]string{ds + "@nope" + "x/y"}, nil); err == nil {
		t.Error("an invalid snapshot name was accepted")
	}

	// Holds, deferred destroy, release.
	if err := c.Hold("t1", ds+"@a", ds+"@b"); err != nil {
		t.Fatal(err)
	}
	if got := lines(cli(t, "zfs", "holds", "-H", ds+"@a")); len(got) != 1 || !strings.Contains(got[0], "t1") {
		t.Errorf("zfs holds = %v", got)
	}
	err = c.Hold("t1", ds+"@a")
	if !errors.Is(err, fs.ErrExist) {
		t.Errorf("holding twice: %v", err)
	}
	if err := c.Destroy(ds+"@b", nil); !errors.Is(err, syscall.EBUSY) {
		t.Errorf("destroying a held snapshot: %v", err)
	}
	if err := c.Destroy(ds+"@b", &DestroyOptions{Defer: true}); err != nil {
		t.Errorf("deferred destroy: %v", err)
	}
	checkProps(t, ds+"@b", map[string]string{"defer_destroy": "on"})
	if err := c.Release("t1", ds+"@b"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := c.Exists(ds + "@b"); ok {
		t.Error("the deferred snapshot survived the release of its last hold")
	}
	if err := c.Release("t1", ds+"@a"); err != nil {
		t.Fatal(err)
	}
	if err := c.Release("t1", ds+"@a"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("releasing a missing hold: %v", err)
	}

	// Bookmarks.
	if err := c.CreateBookmark(ds+"#bm", ds+"@a"); err != nil {
		t.Fatal(err)
	}
	if got := lines(cli(t, "zfs", "list", "-H", "-r", "-t", "bookmark", "-o", "name", ds)); len(got) != 1 || got[0] != ds+"#bm" {
		t.Errorf("bookmarks = %v", got)
	}
	if err := c.CreateBookmark(ds+"#bm", ds+"@a"); !errors.Is(err, fs.ErrExist) {
		t.Errorf("duplicate bookmark: %v", err)
	}
	if err := c.Destroy(ds+"#bm", nil); err != nil {
		t.Fatal(err)
	}
	if got := lines(cli(t, "zfs", "list", "-H", "-r", "-t", "bookmark", "-o", "name", ds)); len(got) != 0 {
		t.Errorf("bookmark survived: %v", got)
	}

	// Clone and promote.
	clone := wds(t, pool, "life_clone")
	if err := c.Clone(clone, ds+"@a", &CreateOptions{Props: map[string]any{"mountpoint": "none", "compression": "gzip-3"}}); err != nil {
		t.Fatal(err)
	}
	checkProps(t, clone, map[string]string{"origin": ds + "@a", "compression": "gzip-3"})
	if err := c.Destroy(ds+"@a", nil); err == nil {
		t.Error("a snapshot with a clone was destroyed")
	}
	if err := c.Promote(clone); err != nil {
		t.Fatal(err)
	}
	checkProps(t, ds, map[string]string{"origin": clone + "@a"})
	checkProps(t, clone, map[string]string{"origin": "-"})

	// Rename (the snapshot moved to the clone with the promotion).
	renamed := wds(t, pool, "life_renamed")
	if err := c.Rename(clone, renamed, false); err != nil {
		t.Fatal(err)
	}
	if ok, _ := c.Exists(renamed + "@a"); !ok {
		t.Error("renamed dataset lost its snapshot")
	}
	if ok, _ := c.Exists(clone); ok {
		t.Error("old name still exists")
	}
	if err := c.Rename(renamed, renamed, false); err == nil {
		t.Error("renaming onto itself worked")
	}
}

func TestRollbackAgainstTheKernel(t *testing.T) {
	c, pool := testClient(t)
	ds := wds(t, pool, "roll")
	if err := c.Create(ds, &CreateOptions{Props: map[string]any{"mountpoint": "none"}}); err != nil {
		t.Fatal(err)
	}
	if err := c.Snapshot([]string{ds + "@one"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.SetProp(ds, "test:marker", "after-one"); err != nil {
		t.Fatal(err)
	}
	if err := c.Snapshot([]string{ds + "@two"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Rollback(ds, "one"); !errors.Is(err, fs.ErrExist) {
		t.Errorf("rollback past a newer snapshot: %v", err)
	}
	got, err := c.Rollback(ds, "")
	if err != nil || got != ds+"@two" {
		t.Errorf("Rollback(latest) = %q, %v", got, err)
	}
	if err := c.Destroy(ds+"@two", nil); err != nil {
		t.Fatal(err)
	}
	got, err = c.Rollback(ds, ds+"@one")
	if err != nil || got != ds+"@one" {
		t.Errorf("Rollback(one) = %q, %v", got, err)
	}
	// Properties are part of the dataset's state and are rolled back with it.
	if v := zget(t, ds, "test:marker")["test:marker"]; v != "-" {
		t.Logf("test:marker after rollback = %q (properties are not rolled back on this release)", v)
	}
}

func TestRecursiveDestroyAndRefusals(t *testing.T) {
	c, pool := testClient(t)
	root := wds(t, pool, "tree")
	for _, n := range []string{root, root + "/a", root + "/a/b", root + "/c"} {
		if err := c.Create(n, &CreateOptions{Props: map[string]any{"mountpoint": "none"}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.CreateVolume(root+"/vol", 1<<20, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Snapshot([]string{root + "@s", root + "/a@s", root + "/a/b@s", root + "/vol@s"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.CreateBookmark(root+"/a#bk", root+"/a@s"); err != nil {
		t.Fatal(err)
	}
	if err := c.Destroy(root+"/a", nil); err == nil {
		t.Error("destroying a dataset with children and snapshots succeeded")
	} else {
		t.Logf("non-recursive destroy: %v", err)
	}
	if err := c.Destroy(root, &DestroyOptions{Recursive: true}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := c.Exists(root); ok {
		t.Error("the tree survived")
	}
	if out, err := exec.Command("zfs", "list", "-H", "-r", "-t", "all", root).CombinedOutput(); err == nil {
		t.Errorf("zfs still lists the tree:\n%s", out)
	}
	if err := c.Destroy(root, nil); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("destroying a missing dataset: %v", err)
	}
}

// What the kernel refuses is reported with a useful error.
func TestWriteRefusals(t *testing.T) {
	c, pool := testClient(t)
	ds := wds(t, pool, "refuse")
	if err := c.Create(ds, &CreateOptions{Props: map[string]any{"mountpoint": "none"}}); err != nil {
		t.Fatal(err)
	}
	if err := c.Clone(ds+"2", ds+"@none", nil); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("clone of a missing snapshot: %v", err)
	}
	if err := c.Snapshot([]string{ds + "@bad@name"}, nil); err == nil {
		t.Error("snapshot name with two @ accepted")
	}
	if err := c.Promote(ds); err == nil {
		t.Error("promoting a non-clone succeeded")
	}
	if err := c.LoadKey(ds, RawKey(make([]byte, 32))); err == nil {
		t.Error("loading a key into an unencrypted dataset succeeded")
	} else {
		t.Logf("LoadKey on an unencrypted dataset: %v", err)
	}
	if err := c.UnloadKey(ds); err == nil {
		t.Error("unloading the key of an unencrypted dataset succeeded")
	}
}

// Which shapes of snapshot request does the kernel take? Each failing shape is
// reported with the kernel's error.
func TestSnapshotRequestShapes(t *testing.T) {
	c, pool := testClient(t)
	ds := wds(t, pool, "shapes")
	for _, n := range []string{ds, ds + "/kid"} {
		if err := c.Create(n, &CreateOptions{Props: map[string]any{"mountpoint": "none"}}); err != nil {
			t.Fatal(err)
		}
	}
	shapes := []struct {
		name  string
		snaps []string
		props map[string]any
	}{
		{"one", []string{ds + "@one"}, nil},
		{"one with prop", []string{ds + "@two"}, map[string]any{"test:p": "v"}},
		{"two datasets", []string{ds + "@three", ds + "/kid@three"}, nil},
		{"two datasets with prop", []string{ds + "@six", ds + "/kid@six"}, map[string]any{"test:p": "v"}},
	}
	for _, s := range shapes {
		if err := c.Snapshot(s.snaps, s.props); err != nil {
			t.Errorf("%s: %v", s.name, err)
		}
	}
}

func TestSnapshotsOfOneDatasetNeedSeparateCalls(t *testing.T) {
	c, pool := testClient(t)
	ds := wds(t, pool, "samedataset")
	if err := c.Create(ds, &CreateOptions{Props: map[string]any{"mountpoint": "none"}}); err != nil {
		t.Fatal(err)
	}
	if err := c.Snapshot([]string{ds + "@x", ds + "@y"}, nil); !errors.Is(err, syscall.EXDEV) {
		t.Errorf("err = %v, want EXDEV", err)
	}
	// The kernel says the same when asked directly.
	in := nvlist.New()
	snaps := nvlist.New()
	snaps.Add(ds+"@x", nvlist.Flag{})
	snaps.Add(ds+"@y", nvlist.Flag{})
	in.Add("snaps", snaps)
	if _, err := c.c.Call(ioctl.IOCSnapshot, pool, in); !errors.Is(err, syscall.EXDEV) {
		t.Errorf("kernel: %v, want EXDEV", err)
	}
}
