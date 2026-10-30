//go:build freebsd

package zfs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-freebsd/zfs/pkg/primitives"
)

// Scenario tests of the handle API against a real pool, checked with
// zfs(8) and zpool(8). They need root, the ZFS module and ZFS_TEST_POOL.

func testClient(t *testing.T) (*Client, *Dataset) {
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
	return c, c.Dataset(pool)
}

// scratch returns a name below the pool, destroyed with all dependents when
// the test ends.
func scratch(t *testing.T, pool *Dataset, name string) string {
	t.Helper()
	n := pool.Name() + "/" + name
	exec.Command("zfs", "destroy", "-rR", n).Run()
	t.Cleanup(func() { exec.Command("zfs", "destroy", "-rR", n).Run() })
	return name
}

func cli(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return strings.TrimRight(string(out), "\n")
}

func zget(t *testing.T, ds string, props ...string) map[string]string {
	t.Helper()
	m := map[string]string{}
	for _, l := range strings.Split(cli(t, "zfs", "get", "-Hp", "-o", "property,value", strings.Join(props, ","), ds), "\n") {
		if f := strings.Split(l, "\t"); len(f) == 2 {
			m[f[0]] = f[1]
		}
	}
	return m
}

func zlist(t *testing.T, args ...string) []string {
	t.Helper()
	out := cli(t, "zfs", append([]string{"list", "-H", "-o", "name"}, args...)...)
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

func check(t *testing.T, ds string, want map[string]string) {
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

func collect2[T any](t *testing.T, seq func(func(T, error) bool)) []T {
	t.Helper()
	var out []T
	for v, err := range seq {
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	return out
}

func names(in []*Info) []string {
	out := make([]string, len(in))
	for i, x := range in {
		out[i] = x.Name
	}
	return out
}

func TestPoolsMatchZpool(t *testing.T) {
	c, _ := testClient(t)
	pools, err := c.Pools()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range pools {
		got = append(got, p.Name())
	}
	want := strings.Fields(cli(t, "zpool", "list", "-H", "-o", "name"))
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, " ") != strings.Join(want, " ") || len(got) == 0 {
		t.Errorf("Pools = %v, zpool list = %v", got, want)
	}
}

func TestScenarioCreateInspectChange(t *testing.T) {
	_, pool := testClient(t)
	home, err := pool.CreateFilesystem(scratch(t, pool, "hl_home"),
		WithCompression(ZSTDLevel(5)), WithQuota(10*MiB), WithRecordSize(64*KiB), WithMountpoint("none"),
		WithChecksum(SHA512), WithAtime(false), WithProp("note:owner", "ops"))
	if err != nil {
		t.Fatal(err)
	}
	check(t, home.Name(), map[string]string{"type": "filesystem", "compression": "zstd-5", "quota": "10485760",
		"recordsize": "65536", "checksum": "sha512", "atime": "off", "note:owner": "ops", "mountpoint": "none"})

	info, err := home.Info()
	if err != nil {
		t.Fatal(err)
	}
	z := zget(t, home.Name(), "guid", "creation", "used", "referenced", "available", "createtxg")
	wantGUID, _ := strconv.ParseUint(z["guid"], 10, 64)
	wantTime, _ := strconv.ParseInt(z["creation"], 10, 64)
	wantTxg, _ := strconv.ParseUint(z["createtxg"], 10, 64)
	if info.Kind != KindFilesystem || info.GUID != wantGUID || info.Created.Unix() != wantTime || info.CreationTxg != wantTxg {
		t.Errorf("identity: %+v vs guid=%d creation=%d txg=%d", info, wantGUID, wantTime, wantTxg)
	}
	if info.Compression != ZSTDLevel(5) || info.Checksum != SHA512 || info.Quota != 10*MiB || info.RecordSize != 64*KiB ||
		info.Atime || info.Mountpoint != "none" || info.Encrypted() || info.KeyStatus != KeyNone && info.KeyStatus != "" {
		t.Errorf("typed fields: compression=%q checksum=%q quota=%d recordsize=%d atime=%v mountpoint=%q enc=%q key=%q",
			info.Compression, info.Checksum, info.Quota, info.RecordSize, info.Atime, info.Mountpoint, info.Encryption, info.KeyStatus)
	}
	if v, ok := info.Prop("note:owner"); !ok || v != "ops" {
		t.Errorf("Prop = %v %v", v, ok)
	}
	if src, ok := info.Source("compression"); !ok || src.Kind != primitives.SourceLocal {
		t.Errorf("source = %v", src)
	}

	// Set, then inherit back.
	if err := home.Set(WithCompression(LZ4), WithQuota(0), WithSync(SyncAlways), WithCopies(2)); err != nil {
		t.Fatal(err)
	}
	check(t, home.Name(), map[string]string{"compression": "lz4", "quota": "0", "sync": "always", "copies": "2"})
	if err := home.Inherit("compression", "sync"); err != nil {
		t.Fatal(err)
	}
	info, _ = home.Info()
	if src, _ := info.Source("compression"); src.Kind != primitives.SourceInherited && src.Kind != primitives.SourceDefault {
		t.Errorf("compression source after Inherit = %v", src)
	}
	check(t, home.Name(), map[string]string{"sync": zget(t, pool.Name(), "sync")["sync"]})

	// Parents and volumes.
	leaf, err := home.CreateFilesystem("a/b/c", Parents(), WithMountpoint("none"))
	if err != nil {
		t.Fatal(err)
	}
	vol, err := home.CreateVolume("disk", 4*MiB, WithVolBlockSize(16*KiB), WithCompression(ZLE))
	if err != nil {
		t.Fatal(err)
	}
	check(t, vol.Name(), map[string]string{"type": "volume", "volsize": "4194304", "volblocksize": "16384", "compression": "zle"})
	vi, _ := vol.Info()
	if vi.Kind != KindVolume || vi.VolSize != 4*MiB || vi.VolBlockSize != 16*KiB || vi.Mountpoint != "" {
		t.Errorf("volume info: %+v", vi)
	}
	if ok, err := leaf.Exists(); err != nil || !ok {
		t.Errorf("leaf exists: %v %v", ok, err)
	}
	if leaf.Parent().Parent().Name() != home.Name()+"/a" {
		t.Error("Parent")
	}

	// Listing agrees with zfs list, in the same parent-first order.
	ctx := context.Background()
	desc := names(collect2[*Info](t, home.Descendants(ctx)))
	if want := zlist(t, "-r", "-t", "filesystem,volume", "-S", "name", home.Name()); len(desc) != len(want)-1 {
		t.Errorf("Descendants = %v, zfs list = %v", desc, want)
	}
	for _, n := range desc {
		if n == home.Name() {
			t.Error("Descendants includes the root")
		}
	}
	if kids := names(collect2[*Info](t, home.Children(ctx))); strings.Join(kids, " ") != strings.Join([]string{home.Name() + "/a", vol.Name()}, " ") {
		t.Errorf("Children = %v", kids)
	}
	if vols := names(collect2[*Info](t, home.Descendants(ctx, OnlyKinds(KindVolume)))); len(vols) != 1 || vols[0] != vol.Name() {
		t.Errorf("OnlyKinds(volume) = %v", vols)
	}
	if two := names(collect2[*Info](t, home.Descendants(ctx, Depth(1)))); len(two) != 2 {
		t.Errorf("Depth(1) = %v", two)
	}
	n := 0
	for _, err := range home.Descendants(ctx) {
		if err != nil {
			t.Fatal(err)
		}
		if n++; n == 1 {
			break
		}
	}

	// Rename moves it; Destroy(Recursive()) removes everything.
	moved, err := leaf.Rename(home.Name() + "/moved")
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := leaf.Exists(); ok {
		t.Error("old name still exists")
	}
	if ok, _ := moved.Exists(); !ok {
		t.Error("new name missing")
	}
	if err := home.Destroy(); err == nil {
		t.Error("destroying a dataset with children without Recursive succeeded")
	}
	if err := home.Destroy(Recursive()); err != nil {
		t.Fatal(err)
	}
	if _, err := home.Info(); !errors.Is(err, ErrNotFound) {
		t.Errorf("Info after destroy: %v", err)
	}
}

func TestScenarioSnapshots(t *testing.T) {
	c, pool := testClient(t)
	ctx := context.Background()
	ds, err := pool.CreateFilesystem(scratch(t, pool, "hl_snap"), WithMountpoint("none"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ds.CreateFilesystem("kid", WithMountpoint("none")); err != nil {
		t.Fatal(err)
	}

	snaps, err := ds.SnapshotAll(ctx, "nightly", WithProp("note:why", "backup"))
	if err != nil || len(snaps) != 2 {
		t.Fatalf("SnapshotAll = %v, %v", snaps, err)
	}
	if got := zlist(t, "-r", "-t", "snapshot", "-S", "name", ds.Name()); len(got) != 2 {
		t.Errorf("zfs list snapshots = %v", got)
	}
	check(t, snaps[1].Name(), map[string]string{"note:why": "backup"})
	if _, err := ds.Snapshot("nightly"); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate snapshot: %v", err)
	}

	second, err := ds.Snapshot("second")
	if err != nil {
		t.Fatal(err)
	}
	listed := names(collect2[*Info](t, ds.Snapshots(ctx)))
	if want := []string{ds.Name() + "@nightly", ds.Name() + "@second"}; strings.Join(listed, " ") != strings.Join(want, " ") {
		t.Errorf("Snapshots = %v, want creation order %v", listed, want)
	}
	si, err := second.Info()
	if err != nil || si.Kind != KindSnapshot || si.GUID == 0 || si.Created.IsZero() || second.Dataset().Name() != ds.Name() {
		t.Errorf("snapshot info: %+v %v", si, err)
	}
	first := snaps[0]

	// Holds, deferred destroy and release.
	if err := first.Hold("keep"); err != nil {
		t.Fatal(err)
	}
	if h, err := first.Holds(); err != nil || len(h) != 1 || h[0].Tag != "keep" || time.Since(h[0].Created) > time.Hour {
		t.Errorf("Holds = %+v, %v", h, err)
	}
	if err := first.Destroy(); !errors.Is(err, ErrBusy) {
		t.Errorf("destroying a held snapshot: %v", err)
	}
	if err := first.Destroy(Deferred()); err != nil {
		t.Fatal(err)
	}
	if err := first.Release("keep"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := first.Exists(); ok {
		t.Error("deferred snapshot survived the last release")
	}

	// Bookmark and space written since.
	bm, err := second.Bookmark("mark")
	if err != nil {
		t.Fatal(err)
	}
	bms, err := ds.Bookmarks()
	if err != nil || len(bms) != 1 || bms[0].Name != bm.Name() {
		t.Errorf("Bookmarks = %v, %v", bms, err)
	}
	third, err := ds.Snapshot("third")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := third.WrittenSince(bm); err != nil {
		t.Fatalf("WrittenSince(bookmark): %v", err)
	}
	sp, err := third.WrittenSince(second)
	if err != nil {
		t.Fatalf("WrittenSince(snapshot): %v", err)
	}
	if want := zget(t, third.Name(), "written@second")["written@second"]; strconv.FormatUint(sp.Written, 10) != want {
		t.Errorf("written = %d, zfs get written@second = %s", sp.Written, want)
	}
	if err := bm.Destroy(); err != nil {
		t.Fatal(err)
	}

	// Clone, promote, rename, rollback.
	clone, err := second.Clone(pool.Name()+"/hl_snap_clone", WithCompression(GZIPLevel(3)), WithMountpoint("none"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { exec.Command("zfs", "destroy", "-rR", clone.Name()).Run() })
	check(t, clone.Name(), map[string]string{"origin": second.Name(), "compression": "gzip-3"})
	if ci, _ := clone.Info(); ci.Origin != second.Name() {
		t.Errorf("Info.Origin = %q", ci.Origin)
	}
	if si, _ := second.Info(); len(si.Clones) != 1 || si.Clones[0] != clone.Name() {
		t.Errorf("Info.Clones = %v", si.Clones)
	}
	if err := clone.Promote(); err != nil {
		t.Fatal(err)
	}
	check(t, clone.Name(), map[string]string{"origin": "-"})
	check(t, ds.Name(), map[string]string{"origin": clone.Name() + "@second"})

	if err := third.Rollback(); err != nil {
		t.Errorf("rollback to the latest snapshot: %v", err)
	}
	if err := ds.Destroy(Recursive()); err != nil {
		t.Logf("destroy of the promoted origin: %v", err)
	}
	_ = c
}

func TestScenarioEncryption(t *testing.T) {
	c, pool := testClient(t)
	_ = c
	const pass = "correct horse battery staple"
	enc, err := pool.CreateFilesystem(scratch(t, pool, "hl_enc"),
		WithEncryption(AES256GCM, PassphraseKey(pass)), WithCompression(ZSTD), WithMountpoint("none"))
	if err != nil {
		t.Fatal(err)
	}
	info, _ := enc.Info()
	if !info.Encrypted() || info.Encryption != AES256GCM || info.KeyStatus != KeyAvailable || info.EncryptionRoot != enc.Name() || info.Compression != ZSTD {
		t.Errorf("info: enc=%q key=%q root=%q compression=%q", info.Encryption, info.KeyStatus, info.EncryptionRoot, info.Compression)
	}
	check(t, enc.Name(), map[string]string{"encryption": "aes-256-gcm", "keyformat": "passphrase", "keystatus": "available"})

	child, err := enc.CreateFilesystem("child", WithMountpoint("none"))
	if err != nil {
		t.Fatal(err)
	}
	if ci, _ := child.Info(); ci.EncryptionRoot != enc.Name() || ci.KeyStatus != KeyAvailable {
		t.Errorf("child: root=%q key=%q", ci.EncryptionRoot, ci.KeyStatus)
	}

	if err := child.UnloadKey(); !errors.Is(err, ErrKey) {
		t.Errorf("unloading the key of a child that is not an encryption root: %v", err)
	}
	if err := enc.UnloadKey(); err != nil {
		t.Fatal(err)
	}
	if i, _ := enc.Info(); i.KeyStatus != KeyUnavailable {
		t.Errorf("key status after unload = %q", i.KeyStatus)
	}
	if err := enc.LoadKey(PassphraseKey("not the passphrase")); !errors.Is(err, ErrKey) {
		t.Errorf("wrong passphrase: %v", err)
	}
	if err := enc.CheckKey(PassphraseKey(pass)); err != nil {
		t.Errorf("CheckKey: %v", err)
	}
	if err := enc.LoadKey(PassphraseKey(pass)); err != nil {
		t.Fatal(err)
	}
	if i, _ := enc.Info(); i.KeyStatus != KeyAvailable {
		t.Errorf("key status after load = %q", i.KeyStatus)
	}

	// The key we created is the key zfs derives, and after ChangeKey the new one.
	if err := enc.ChangeKey(HexKey(strings.Repeat("ab", 32))); err != nil {
		t.Fatal(err)
	}
	cli(t, "zfs", "unload-key", "-r", enc.Name())
	cmd := exec.Command("zfs", "load-key", enc.Name())
	cmd.Stdin = bytes.NewReader([]byte(strings.Repeat("ab", 32) + "\n"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("zfs load-key with the changed key: %v\n%s", err, out)
	}

	// Key options belong to Create only.
	if err := enc.Set(WithEncryption(AES128CCM, RawKey(make([]byte, 32)))); !errors.Is(err, ErrInvalidOption) {
		t.Errorf("Set(WithEncryption) = %v", err)
	}
	// A key for a dataset that is not encrypted.
	plain, err := pool.CreateFilesystem(scratch(t, pool, "hl_plain"), WithMountpoint("none"))
	if err != nil {
		t.Fatal(err)
	}
	if err := plain.LoadKey(RawKey(make([]byte, 32))); !errors.Is(err, ErrKey) {
		t.Errorf("LoadKey on a plain dataset: %v", err)
	}
}

func TestScenarioErrors(t *testing.T) {
	c, pool := testClient(t)
	ds, err := pool.CreateFilesystem(scratch(t, pool, "hl_err"), WithMountpoint("none"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.CreateFilesystem("hl_err"); !errors.Is(err, ErrExists) {
		t.Errorf("create twice: %v", err)
	}
	if _, err := c.Dataset(pool.Name() + "/hl_missing/x").CreateFilesystem("y"); !errors.Is(err, ErrNotFound) {
		t.Errorf("create below a missing parent: %v", err)
	}
	if _, err := c.Dataset(pool.Name() + "/hl_missing").Info(); !errors.Is(err, ErrNotFound) {
		t.Errorf("Info of a missing dataset: %v", err)
	}
	if ok, err := c.Dataset(pool.Name() + "/hl_missing").Exists(); ok || err != nil {
		t.Errorf("Exists = %v, %v", ok, err)
	}
	if err := ds.Set(WithCompression("brotli")); !errors.Is(err, primitives.ErrBadValue) {
		t.Errorf("bad compression: %v", err)
	}
	if err := ds.Set(WithRecordSize(3000)); !errors.Is(err, primitives.ErrBadValue) {
		t.Errorf("bad recordsize: %v", err)
	}
	if err := ds.Set(WithQuota(1 << 40)); err != nil {
		t.Errorf("a quota above the pool size is allowed: %v", err)
	}
	var oe *OpError
	if err := ds.Set(Parents()); !errors.As(err, &oe) || oe.Op != "set" || oe.Name != ds.Name() {
		t.Errorf("OpError = %v", err)
	}
	if _, err := ds.Snapshot("x", WithQuota(1)); !errors.Is(err, ErrInvalidOption) {
		t.Errorf("snapshot with a property option: %v", err)
	}
}
