//go:build freebsd

package zfs

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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

// Send and receive through the handle API, with zfs(8) as the oracle.

func guidOf(t *testing.T, name string) string { return zget(t, name, "guid")["guid"] }

// mountedSource makes a filesystem with files, mounted below a temp directory.
func mountedSource(t *testing.T, pool *Dataset, name string, files map[string]string, opts ...Option) *Dataset {
	t.Helper()
	dir := t.TempDir()
	ds, err := pool.CreateFilesystem(scratch(t, pool, name), append([]Option{WithMountpoint(dir)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	cli(t, "zfs", "mount", ds.Name())
	for f, body := range files {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return ds
}

func TestScenarioSendReceive(t *testing.T) {
	c, pool := testClient(t)
	ctx := context.Background()
	src := mountedSource(t, pool, "sr_src", map[string]string{"one": "first", "two": strings.Repeat("y", 100000)}, WithCompression(LZ4))
	a, err := src.Snapshot("a")
	if err != nil {
		t.Fatal(err)
	}
	cli(t, "sh", "-c", "echo changed > "+zget(t, src.Name(), "mountpoint")["mountpoint"]+"/one")
	b, err := src.Snapshot("b")
	if err != nil {
		t.Fatal(err)
	}

	// Full and incremental, into a buffer and back.
	var full, incr bytes.Buffer
	if err := a.Send(ctx, &full); err != nil {
		t.Fatal(err)
	}
	if err := b.Send(ctx, &incr, SendFrom(a), Compressed()); err != nil {
		t.Fatal(err)
	}
	h, err := ReadStreamHeader(bytes.NewReader(full.Bytes()))
	if err != nil || h.ToName != a.Name() || h.Incremental() {
		t.Fatalf("header %+v %v", h, err)
	}
	dst := pool.Child("sr_dst")
	t.Cleanup(func() { exec.Command("zfs", "destroy", "-rR", dst.Name()).Run() })
	got, err := dst.Receive(ctx, &full, WithMountpoint("none"), WithProp("note:from", "src"))
	if err != nil || len(got) != 1 || got[0].Snapshot != dst.Name()+"@a" || got[0].Bytes == 0 {
		t.Fatalf("Receive = %+v, %v", got, err)
	}
	if _, err := dst.Receive(ctx, &incr); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"a", "b"} {
		if guidOf(t, dst.Name()+"@"+s) != guidOf(t, src.Name()+"@"+s) {
			t.Errorf("guid of @%s differs", s)
		}
	}
	check(t, dst.Name(), map[string]string{"note:from": "src", "mountpoint": "none"})
	if _, err := dst.Receive(ctx, bytes.NewReader(full.Bytes())); !errors.Is(err, ErrExists) {
		t.Errorf("full stream into an existing dataset: %v", err)
	}

	// Estimates are what zfs send -n says.
	for _, tc := range []struct {
		opts []Option
		args []string
	}{
		{nil, []string{b.Name()}},
		{[]Option{SendFrom(a)}, []string{"-i", a.Name(), b.Name()}},
		{[]Option{Compressed(), LargeBlocks()}, []string{"-c", "-L", b.Name()}},
	} {
		n, err := b.SendSize(tc.opts...)
		if err != nil {
			t.Fatal(err)
		}
		out := cli(t, "zfs", append([]string{"send", "-nP"}, tc.args...)...)
		if !strings.Contains(out, "size\t"+strconv.FormatUint(n, 10)) || n == 0 {
			t.Errorf("SendSize(%v) = %d, zfs send -nP says %q", tc.args, n, out)
		}
	}

	// Through files: the kernel reads and writes the descriptors directly.
	path := filepath.Join(t.TempDir(), "stream")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Send(ctx, f); err != nil {
		t.Fatal(err)
	}
	f.Close()
	in, _ := os.Open(path)
	defer in.Close()
	dst2 := pool.Child("sr_dst2")
	t.Cleanup(func() { exec.Command("zfs", "destroy", "-rR", dst2.Name()).Run() })
	if _, err := dst2.Receive(ctx, in); err != nil {
		t.Fatal(err)
	}
	if guidOf(t, dst2.Name()+"@a") != guidOf(t, a.Name()) {
		t.Error("guid differs through files")
	}

	// Layouts: the stream's path rebuilt below a parent.
	parent := scratch(t, pool, "sr_parent")
	if _, err := pool.CreateFilesystem(parent, WithMountpoint("none")); err != nil {
		t.Fatal(err)
	}
	var again bytes.Buffer
	if err := a.Send(ctx, &again); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Child(parent).Receive(ctx, &again, DiscardPool(), Parents()); err != nil {
		t.Fatal(err)
	}
	// "<pool>/sr_src@a" below the parent without the pool name.
	if ok, _ := c.Dataset(pool.Name() + "/" + parent + "/sr_src").Exists(); !ok {
		t.Errorf("DiscardPool did not rebuild the path below %s: %v", parent, zlist(t, "-r", pool.Name()+"/"+parent))
	}
}

func TestScenarioSendIntermediatesAndReplicationStreams(t *testing.T) {
	c, pool := testClient(t)
	ctx := context.Background()
	src := mountedSource(t, pool, "im_src", map[string]string{"f": "0"})
	var snaps []*Snapshot
	for _, n := range []string{"a", "b", "c", "d"} {
		os.WriteFile(filepath.Join(zget(t, src.Name(), "mountpoint")["mountpoint"], "f"), []byte(n), 0o644)
		s, err := src.Snapshot(n)
		if err != nil {
			t.Fatal(err)
		}
		snaps = append(snaps, s)
	}
	var base, rest bytes.Buffer
	if err := snaps[0].Send(ctx, &base); err != nil {
		t.Fatal(err)
	}
	if err := snaps[3].Send(ctx, &rest, SendIntermediates(snaps[0])); err != nil {
		t.Fatal(err)
	}
	dst := pool.Child("im_dst")
	t.Cleanup(func() { exec.Command("zfs", "destroy", "-rR", dst.Name()).Run() })
	if _, err := dst.Receive(ctx, &base); err != nil {
		t.Fatal(err)
	}
	got, err := dst.Receive(ctx, &rest)
	if err != nil || len(got) != 3 {
		t.Fatalf("received %d streams, %v", len(got), err)
	}
	for _, s := range snaps {
		if guidOf(t, dst.Name()+"@"+s.ShortName()) != guidOf(t, s.Name()) {
			t.Errorf("@%s differs", s.ShortName())
		}
	}

	// What zfs send -I and -R write is refused, and nothing is created.
	for _, args := range [][]string{{"-I", snaps[0].Name(), snaps[3].Name()}, {"-R", snaps[0].Name()}} {
		out, err := exec.Command("zfs", append([]string{"send"}, args...)...).Output()
		if err != nil {
			t.Fatalf("zfs send %v: %v", args, err)
		}
		target := pool.Child("im_refused")
		if _, err := target.Receive(ctx, bytes.NewReader(out)); !errors.Is(err, ErrReplicationStream) {
			t.Errorf("zfs send %v: %v", args, err)
		}
		if ok, _ := target.Exists(); ok {
			t.Error("a dataset was made from a refused stream")
		}
	}
	// And garbage is not a stream.
	if _, err := pool.Child("im_garbage").Receive(ctx, strings.NewReader(strings.Repeat("not a stream ", 100))); !errors.Is(err, ErrNotAStream) {
		t.Errorf("garbage: %v", err)
	}
	_ = c
}

func TestScenarioRawEncryptedSend(t *testing.T) {
	c, pool := testClient(t)
	ctx := context.Background()
	const pass = "correct horse battery staple"
	src, err := pool.CreateFilesystem(scratch(t, pool, "rw_src"), WithEncryption(AES256GCM, PassphraseKey(pass)), WithMountpoint("none"))
	if err != nil {
		t.Fatal(err)
	}
	snap, err := src.Snapshot("a")
	if err != nil {
		t.Fatal(err)
	}
	var raw bytes.Buffer
	if err := snap.Send(ctx, &raw, Raw()); err != nil {
		t.Fatal(err)
	}
	if h, _ := ReadStreamHeader(bytes.NewReader(raw.Bytes())); h == nil || !h.Raw() {
		t.Fatalf("not a raw stream: %+v", h)
	}
	dst := pool.Child("rw_dst")
	t.Cleanup(func() { exec.Command("zfs", "destroy", "-rR", dst.Name()).Run() })
	if _, err := dst.Receive(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	info, err := dst.Info()
	if err != nil || !info.Encrypted() || info.KeyStatus != KeyUnavailable || info.Encryption != AES256GCM {
		t.Fatalf("received: %+v %v", info, err)
	}
	if err := dst.LoadKey(PassphraseKey(pass)); err != nil {
		t.Fatalf("LoadKey of a raw received dataset: %v", err)
	}
	_ = c
}

func TestScenarioResumeAndProgress(t *testing.T) {
	c, pool := testClient(t)
	ctx := context.Background()
	data := make([]byte, 12<<20)
	rand.Read(data)
	src := mountedSource(t, pool, "rp_src", map[string]string{"blob": string(data)}, WithCompression(CompressionOff))
	snap, err := src.Snapshot("a")
	if err != nil {
		t.Fatal(err)
	}

	// Progress while sending to a slow writer.
	var reports int
	var last SendProgress
	var whole bytes.Buffer
	if err := snap.Send(ctx, &slowBuf{Buffer: &whole}, OnProgress(10*time.Millisecond, func(p SendProgress) { reports++; last = p })); err != nil {
		t.Fatal(err)
	}
	if reports == 0 || last.Bytes == 0 || last.Bytes > uint64(whole.Len()) {
		t.Errorf("progress: %d reports, last %+v, stream %d", reports, last, whole.Len())
	}

	// Interrupted resumable receive, token, resumed send, rest received.
	dst := pool.Child("rp_dst")
	t.Cleanup(func() { exec.Command("zfs", "destroy", "-rR", dst.Name()).Run() })
	cut := whole.Len() / 3
	if _, err := dst.Receive(ctx, bytes.NewReader(whole.Bytes()[:cut]), Resumable()); err == nil || !errors.Is(err, ErrStreamTruncated) {
		t.Fatalf("truncated receive: %v", err)
	}
	token, ok, err := dst.ResumeToken()
	if err != nil || !ok {
		t.Fatalf("ResumeToken: %v %v", ok, err)
	}
	if tok, err := ParseResumeToken(token); err != nil || tok.ToName != snap.Name() {
		t.Errorf("token %+v %v", tok, err)
	}
	var rest bytes.Buffer
	if err := c.ResumeSend(ctx, &rest, token); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.Receive(ctx, &rest, Resumable()); err != nil {
		t.Fatal(err)
	}
	if guidOf(t, dst.Name()+"@a") != guidOf(t, snap.Name()) {
		t.Error("guid differs after resuming")
	}

	// Abort drops the partial state.
	dst2 := pool.Child("rp_dst2")
	t.Cleanup(func() { exec.Command("zfs", "destroy", "-rR", dst2.Name()).Run() })
	dst2.Receive(ctx, bytes.NewReader(whole.Bytes()[:cut]), Resumable())
	if err := dst2.AbortResume(); err != nil {
		t.Fatal(err)
	}
	if ok, _ := dst2.Exists(); ok {
		t.Error("the half received dataset survived AbortResume")
	}
	if err := dst2.AbortResume(); !errors.Is(err, ErrNotFound) && err == nil {
		t.Errorf("aborting with nothing to abort: %v", err)
	}
}

type slowBuf struct{ *bytes.Buffer }

func (w *slowBuf) Write(p []byte) (int, error) {
	time.Sleep(15 * time.Millisecond)
	return w.Buffer.Write(p)
}

func TestScenarioRedactedSend(t *testing.T) {
	c, pool := testClient(t)
	ctx := context.Background()
	src := mountedSource(t, pool, "rd_src", map[string]string{"public": "ok", "secret": "classified"})
	snap, err := src.Snapshot("a")
	if err != nil {
		t.Fatal(err)
	}
	cdir := t.TempDir()
	clone, err := snap.Clone(pool.Name()+"/hl_rd_clone", WithMountpoint(cdir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { exec.Command("zfs", "destroy", "-rR", clone.Name()).Run() })
	cli(t, "zfs", "mount", clone.Name())
	os.WriteFile(filepath.Join(cdir, "secret"), []byte("REDACTED"), 0o644)
	rs, err := clone.Snapshot("r")
	if err != nil {
		t.Fatal(err)
	}
	bm, err := snap.Redact("red", rs)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := snap.Send(ctx, &out, RedactedWith(bm)); err != nil {
		t.Fatal(err)
	}
	h, _ := ReadStreamHeader(bytes.NewReader(out.Bytes()))
	if h == nil || h.Features&primitives.FeatureRedacted == 0 || bytes.Contains(out.Bytes(), []byte("classified")) {
		t.Errorf("not a redacted stream: %+v", h)
	}
	dst := pool.Child("rd_dst")
	t.Cleanup(func() { exec.Command("zfs", "destroy", "-rR", dst.Name()).Run() })
	if _, err := dst.Receive(ctx, &out); err != nil {
		t.Fatal(err)
	}
	if got := zget(t, dst.Name()+"@a", "redact_snaps")["redact_snaps"]; got == "-" || got == "" {
		t.Errorf("the received snapshot is not marked redacted")
	}
	_ = c
}

func TestScenarioSendWriterFailsOrContextEnds(t *testing.T) {
	_, pool := testClient(t)
	data := make([]byte, 8<<20)
	rand.Read(data)
	src := mountedSource(t, pool, "wf_src", map[string]string{"blob": string(data)}, WithCompression(CompressionOff))
	snap, err := src.Snapshot("a")
	if err != nil {
		t.Fatal(err)
	}
	if err := snap.Send(context.Background(), &brokenWriter{after: 1 << 20}); err == nil || !strings.Contains(err.Error(), "sink is broken") {
		t.Errorf("failing writer: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := snap.Send(ctx, &stuckWriter{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("stuck writer: %v", err)
	}
}

type brokenWriter struct{ after int }

func (w *brokenWriter) Write(p []byte) (int, error) {
	if w.after -= len(p); w.after < 0 {
		return 0, errors.New("sink is broken")
	}
	return len(p), nil
}

// stuckWriter blocks until the test ends, like a network peer that stopped reading.
type stuckWriter struct{}

func (stuckWriter) Write(p []byte) (int, error) { time.Sleep(time.Hour); return len(p), nil }
