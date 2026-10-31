package zfs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-freebsd/zfs/pkg/ioctl"
	"github.com/go-freebsd/zfs/pkg/primitives"
)

func TestHandlesAreNames(t *testing.T) {
	c, s := newStub()
	d := c.Dataset("tank/home/alice")
	if d.Name() != "tank/home/alice" || d.Pool() != "tank" || d.Parent().Name() != "tank/home" || d.Parent().Parent().Parent() != nil {
		t.Errorf("handle names wrong: %s %s %v", d.Name(), d.Pool(), d.Parent())
	}
	if d.Child("x/y").Name() != "tank/home/alice/x/y" {
		t.Error("Child")
	}
	snap := c.Snapshot("tank/home@daily")
	if snap.ShortName() != "daily" || snap.Dataset().Name() != "tank/home" {
		t.Errorf("snapshot handle: %s %s", snap.ShortName(), snap.Dataset())
	}
	if c.Bookmark("tank/home#bm").Dataset().Name() != "tank/home" {
		t.Error("bookmark handle")
	}
	if len(s.calls) != 0 {
		t.Errorf("making handles touched the kernel: %v", s.calls)
	}
	pools, err := c.Pools()
	if err != nil || len(pools) != 2 || pools[1].Name() != "zroot" {
		t.Errorf("Pools = %v, %v", pools, err)
	}
}

func TestCreateFilesystemMapsOptions(t *testing.T) {
	c, s := newStub()
	fsys, err := c.Dataset("tank").CreateFilesystem("home/alice",
		WithCompression(ZSTDLevel(5)), WithQuota(10*GiB), WithRecordSize(64*KiB), WithMountpoint("/home/alice"),
		WithAtime(false), WithChecksum(SHA512), WithSync(SyncAlways), WithCopies(2), WithReadOnly(false),
		WithProp("note:owner", "ops"), WithCaseSensitivity("insensitive"), WithUTF8Only(true), Parents())
	if err != nil {
		t.Fatal(err)
	}
	if fsys.Name() != "tank/home/alice" || s.last() != "Create tank/home/alice" {
		t.Errorf("created %s via %q", fsys, s.last())
	}
	want := map[string]any{
		"compression": "zstd-5", "quota": 10 * GiB, "recordsize": 64 * KiB, "mountpoint": "/home/alice",
		"atime": false, "checksum": "sha512", "sync": "always", "copies": "2", "readonly": false,
		"note:owner": "ops", "casesensitivity": "insensitive", "utf8only": true,
	}
	if !reflect.DeepEqual(s.createOpts.Props, want) {
		t.Errorf("props = %v\nwant    %v", s.createOpts.Props, want)
	}
	if !s.createOpts.Parents || s.createOpts.Key != nil {
		t.Errorf("opts = %+v", s.createOpts)
	}
}

func TestCreateVolumeAndEncryption(t *testing.T) {
	c, s := newStub()
	key := PassphraseKey("correct horse battery")
	if _, err := c.Dataset("tank").CreateVolume("swap", 4*GiB, WithVolBlockSize(16*KiB), WithEncryption(AES256GCM, key)); err != nil {
		t.Fatal(err)
	}
	if s.last() != "CreateVolume tank/swap 4294967296" {
		t.Errorf("call = %q", s.last())
	}
	if s.createOpts.Props["encryption"] != "aes-256-gcm" || s.createOpts.Props["volblocksize"] != 16*KiB || s.createOpts.Key != key {
		t.Errorf("opts = %+v", s.createOpts)
	}
}

func TestSetInheritDestroyRename(t *testing.T) {
	c, s := newStub()
	d := c.Dataset("tank/a")
	if err := d.Set(WithCompression(LZ4), WithAtime(false)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s.setProps, map[string]any{"compression": "lz4", "atime": false}) {
		t.Errorf("props = %v", s.setProps)
	}
	if err := d.Inherit("compression", "atime"); err != nil || strings.Join(s.calls[len(s.calls)-2:], "|") != "InheritProp tank/a compression|InheritProp tank/a atime" {
		t.Errorf("inherit: %v %v", err, s.calls)
	}
	if err := d.Destroy(Recursive()); err != nil || !s.destroyOpt.Recursive || s.last() != "Destroy tank/a" {
		t.Errorf("destroy: %v %+v", err, s.destroyOpt)
	}
	if err := d.Destroy(); err != nil || s.destroyOpt.Recursive {
		t.Errorf("plain destroy: %+v", s.destroyOpt)
	}
	n, err := d.Rename("tank/b/a")
	if err != nil || n.Name() != "tank/b/a" || s.last() != "Rename tank/a tank/b/a false" {
		t.Errorf("rename: %v %v %q", n, err, s.last())
	}
	if err := d.Promote(); err != nil || s.last() != "Promote tank/a" {
		t.Errorf("promote: %v", err)
	}
}

func TestSnapshotLifecycleMapping(t *testing.T) {
	c, s := newStub()
	d := c.Dataset("tank/a")
	snap, err := d.Snapshot("one", WithProp("note:why", "backup"))
	if err != nil || snap.Name() != "tank/a@one" || !reflect.DeepEqual(s.snapNames, []string{"tank/a@one"}) || s.snapProps["note:why"] != "backup" {
		t.Fatalf("snapshot: %v %v %v", snap, err, s.snapNames)
	}
	if err := snap.Hold("keep"); err != nil || s.last() != "Hold keep [tank/a@one]" {
		t.Errorf("hold: %q", s.last())
	}
	if err := snap.Release("keep"); err != nil || s.last() != "Release keep [tank/a@one]" {
		t.Errorf("release: %q", s.last())
	}
	if err := snap.Destroy(Deferred()); err != nil || !s.destroyOpt.Defer || s.last() != "Destroy tank/a@one" {
		t.Errorf("destroy: %+v", s.destroyOpt)
	}
	if err := snap.Rollback(); err != nil || s.last() != "Rollback tank/a tank/a@one" {
		t.Errorf("rollback: %q", s.last())
	}
	clone, err := snap.Clone("tank/clone", WithCompression(LZ4), Parents())
	if err != nil || clone.Name() != "tank/clone" || s.last() != "Clone tank/clone tank/a@one" || s.createOpts.Props["compression"] != "lz4" || !s.createOpts.Parents {
		t.Errorf("clone: %v %q %+v", err, s.last(), s.createOpts)
	}
	bm, err := snap.Bookmark("bm")
	if err != nil || bm.Name() != "tank/a#bm" || s.last() != "CreateBookmark tank/a#bm tank/a@one" {
		t.Errorf("bookmark: %v %q", err, s.last())
	}
	s.space = primitives.Space{Written: 42}
	sp, err := snap.WrittenSince(bm)
	if err != nil || sp.Written != 42 || s.last() != "SpaceWritten tank/a@one tank/a#bm" {
		t.Errorf("written: %+v %v %q", sp, err, s.last())
	}
	if err := bm.Destroy(); err != nil || s.last() != "Destroy tank/a#bm" {
		t.Errorf("destroy bookmark: %q", s.last())
	}
}

func TestSnapshotAllTakesTheWholeTreeAtOnce(t *testing.T) {
	c, s := newStub()
	s.tree = []*primitives.Dataset{ds("tank/a", primitives.Filesystem, nil), ds("tank/a/b", primitives.Filesystem, nil), ds("tank/a/v", primitives.Volume, nil)}
	snaps, err := c.Dataset("tank/a").SnapshotAll(context.Background(), "nightly")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"tank/a@nightly", "tank/a/b@nightly", "tank/a/v@nightly"}
	if !reflect.DeepEqual(s.snapNames, want) || len(snaps) != 3 || snaps[2].Name() != want[2] {
		t.Errorf("names = %v, handles = %v", s.snapNames, snaps)
	}
	n := 0
	for _, call := range s.calls {
		if strings.HasPrefix(call, "Snapshot ") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d Snapshot calls, want one atomic call", n)
	}
}

func TestOptionMisuseIsRefusedBeforeAnyCall(t *testing.T) {
	c, s := newStub()
	d, snap := c.Dataset("tank/a"), c.Snapshot("tank/a@x")
	key := RawKey(make([]byte, 32))
	ctx := context.Background()
	cases := map[string]func() error{
		"Parents to Set":          func() error { return d.Set(Parents()) },
		"encryption to Set":       func() error { return d.Set(WithEncryption(AES256GCM, key)) },
		"volblocksize to Set":     func() error { return d.Set(WithVolBlockSize(4096)) },
		"Recursive to Create":     func() error { _, err := d.CreateFilesystem("x", Recursive()); return err },
		"Deferred to dataset":     func() error { return d.Destroy(Deferred()) },
		"Recursive to snapshot":   func() error { return snap.Destroy(Recursive()) },
		"Compression to Snapshot": func() error { _, err := d.Snapshot("y", WithCompression(LZ4)); return err },
		"Quota to Snapshot":       func() error { _, err := d.Snapshot("y", WithQuota(1)); return err },
		"Depth to Create":         func() error { _, err := d.CreateVolume("v", 1, Depth(2)); return err },
		"encryption to Clone":     func() error { _, err := snap.Clone("tank/c", WithEncryption(AES256GCM, key)); return err },
		"Compression to SnapAll":  func() error { _, err := d.SnapshotAll(ctx, "z", WithCompression(LZ4)); return err },
		"zero Option":             func() error { return d.Set(Option{}) },
	}
	for name, f := range cases {
		err := f()
		if !errors.Is(err, ErrInvalidOption) {
			t.Errorf("%s: err = %v, want ErrInvalidOption", name, err)
		}
		var oe *OpError
		if !errors.As(err, &oe) {
			t.Errorf("%s: not an *OpError: %v", name, err)
		}
	}
	for it, err := range d.Children(ctx, WithCompression(LZ4)) {
		if it != nil || !errors.Is(err, ErrInvalidOption) {
			t.Errorf("listing with a property option: %v %v", it, err)
		}
	}
	if len(s.calls) != 0 {
		t.Errorf("calls were made: %v", s.calls)
	}
}

func TestErrorsClassify(t *testing.T) {
	c, s := newStub()
	d := c.Dataset("tank/a")
	for _, tc := range []struct {
		errno  syscall.Errno
		target error
	}{
		{syscall.ENOENT, ErrNotFound}, {syscall.EEXIST, ErrExists}, {syscall.EBUSY, ErrBusy},
		{syscall.EPERM, ErrPermission}, {syscall.EACCES, ErrPermission}, {syscall.ENOSPC, ErrNoSpace},
	} {
		s.err = &ioctl.Error{Ioc: ioctl.IOCDestroy, Name: "tank/a", Errno: tc.errno}
		err := d.Destroy()
		if !errors.Is(err, tc.target) {
			t.Errorf("%v does not match %v: %v", tc.errno, tc.target, err)
		}
		var ie *ioctl.Error
		var oe *OpError
		if !errors.As(err, &ie) || !errors.As(err, &oe) || oe.Op != "destroy" || oe.Name != "tank/a" {
			t.Errorf("error chain broken: %v", err)
		}
	}
	if !errors.Is(&OpError{Err: syscall.ENOENT}, fs.ErrNotExist) {
		t.Error("ErrNotFound is not fs.ErrNotExist")
	}
	s.err = nil
	if err := d.Set(WithQuota(1)); err != nil {
		t.Errorf("success wrapped as error: %v", err)
	}
}

func TestKeyErrorsCarryErrKey(t *testing.T) {
	c, s := newStub()
	d := c.Dataset("tank/enc")
	key := PassphraseKey("long enough!")
	if err := d.LoadKey(key); err != nil || s.key != key {
		t.Fatalf("LoadKey: %v", err)
	}
	for name, f := range map[string]func() error{
		"load": func() error { return d.LoadKey(key) }, "check": func() error { return d.CheckKey(key) },
		"unload": d.UnloadKey, "change": func() error { return d.ChangeKey(key) }, "inherit": d.InheritKey,
	} {
		s.err = &ioctl.Error{Name: "tank/enc", Errno: syscall.EACCES}
		err := f()
		if !errors.Is(err, ErrKey) || !errors.Is(err, syscall.EACCES) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestInfoReadsTypedFields(t *testing.T) {
	c, s := newStub()
	s.get = ds("tank/enc", primitives.Filesystem, map[string]primitives.Property{
		"used": {Value: uint64(1000)}, "available": {Value: uint64(9000)}, "referenced": {Value: uint64(500)},
		"creation": {Value: uint64(1700000000)}, "quota": {Value: uint64(10 << 30), Source: "tank/enc"},
		"compression": {Value: uint64(15), Source: "tank"}, "checksum": {Value: uint64(13)},
		"mountpoint": {Value: "/tank/enc", Source: "default"}, "recordsize": {Value: uint64(131072)},
		"readonly": {Value: uint64(0)}, "atime": {Value: uint64(1)},
		"encryption": {Value: uint64(8)}, "keystatus": {Value: uint64(2)}, "encryptionroot": {Value: "tank/enc"},
		"compressratio": {Value: uint64(150)}, "note:x": {Value: "hi"},
	})
	info, err := c.Dataset("tank/enc").Info()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"Used": uint64(1000), "Available": uint64(9000), "Quota": uint64(10 << 30), "RecordSize": uint64(131072),
		"Compression": LZ4, "Checksum": Edonr, "Encryption": AES256GCM, "KeyStatus": KeyAvailable,
		"EncryptionRoot": "tank/enc", "Mountpoint": "/tank/enc", "Atime": true, "ReadOnly": false, "Kind": KindFilesystem,
	}
	rv := reflect.ValueOf(info).Elem()
	for f, w := range want {
		if got := rv.FieldByName(f).Interface(); got != w {
			t.Errorf("%s = %#v, want %#v", f, got, w)
		}
	}
	if !info.Encrypted() || !info.Created.Equal(time.Unix(1700000000, 0)) || info.CompressRatio != 1.5 {
		t.Errorf("Encrypted=%v Created=%v ratio=%v", info.Encrypted(), info.Created, info.CompressRatio)
	}
	if v, ok := info.Prop("note:x"); !ok || v != "hi" {
		t.Errorf("Prop = %v %v", v, ok)
	}
	if info.Dataset().Name() != "tank/enc" || info.Raw() == nil {
		t.Error("handles from Info")
	}

	// A snapshot has no filesystem settings.
	s.get = ds("tank/enc@s", primitives.Snapshot, map[string]primitives.Property{"used": {Value: uint64(5)}, "creation": {Value: uint64(1700000001)}})
	si, err := c.Snapshot("tank/enc@s").Info()
	if err != nil || si.Used != 5 || si.Compression != "" || si.Encrypted() || si.Snapshot().Name() != "tank/enc@s" {
		t.Errorf("snapshot info: %+v %v", si, err)
	}
}

func TestIterationSkipsRootStopsOnBreakAndContext(t *testing.T) {
	c, s := newStub()
	s.tree = []*primitives.Dataset{ds("tank/a", primitives.Filesystem, nil), ds("tank/a/b", primitives.Filesystem, nil), ds("tank/a/c", primitives.Volume, nil)}
	d := c.Dataset("tank/a")

	var got []string
	for in, err := range d.Descendants(context.Background()) {
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, in.Name)
	}
	if !reflect.DeepEqual(got, []string{"tank/a/b", "tank/a/c"}) {
		t.Errorf("descendants = %v (the root must not be included)", got)
	}
	if !strings.Contains(strings.Join(s.calls, "|"), "Walk tank/a types=filesystem,volume depth=") {
		t.Errorf("walk call = %v", s.calls)
	}

	s.walked = 0
	for range d.Descendants(context.Background()) {
		break
	}
	if s.walked > 2 {
		t.Errorf("the walk went on after break: %d datasets visited", s.walked)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n, sawErr := 0, error(nil)
	for _, err := range d.Descendants(ctx) {
		if err != nil {
			sawErr = err
			break
		}
		n++
		cancel()
	}
	if n != 1 || !errors.Is(sawErr, context.Canceled) {
		t.Errorf("after cancel: %d items, err %v", n, sawErr)
	}

	s.calls = nil
	for range d.Children(context.Background(), OnlyKinds(KindVolume)) {
	}
	if got := s.calls[0]; got != "Walk tank/a types=volume depth=1" {
		t.Errorf("children call = %q", got)
	}
	s.calls = nil
	for range d.Snapshots(context.Background()) {
	}
	if got := s.calls[0]; got != "Walk tank/a types=snapshot depth=0" {
		t.Errorf("snapshots call = %q", got)
	}

	s.err = &ioctl.Error{Errno: syscall.ENOENT, Name: "tank/a"}
	errs := 0
	for _, err := range d.Descendants(context.Background()) {
		if errors.Is(err, ErrNotFound) {
			errs++
		}
	}
	if errs != 1 {
		t.Errorf("a failing walk reported %d errors, want 1", errs)
	}
}

// Every typed constant must be a value the property table knows, or the
// kernel call it leads to would be refused.
func TestTypedConstantsAreInTheTable(t *testing.T) {
	check := func(prop string, values ...string) {
		t.Helper()
		info, ok := primitives.LookupProp(prop)
		if !ok {
			t.Fatalf("%s is not in the table", prop)
		}
		known := map[string]bool{}
		for _, v := range info.Values {
			known[v.Name] = true
		}
		for _, v := range values {
			if !known[v] {
				t.Errorf("%s=%s is not a value of the property", prop, v)
			}
		}
	}
	comp := []Compression{CompressionOn, CompressionOff, LZJB, LZ4, ZLE, GZIP, ZSTD}
	for n := 1; n <= 9; n++ {
		comp = append(comp, GZIPLevel(n))
	}
	for n := 1; n <= 19; n++ {
		comp = append(comp, ZSTDLevel(n))
	}
	var names []string
	for _, v := range comp {
		names = append(names, string(v))
	}
	check("compression", names...)
	check("checksum", string(ChecksumOn), string(ChecksumOff), string(Fletcher2), string(Fletcher4), string(SHA256), string(SHA512), string(Skein), string(Edonr), string(Blake3), string(NoParity))
	check("sync", string(SyncStandard), string(SyncAlways), string(SyncDisabled))
	check("encryption", string(EncryptionOff), string(EncryptionOn), string(AES128CCM), string(AES192CCM), string(AES256CCM), string(AES128GCM), string(AES192GCM), string(AES256GCM))
	check("keystatus", string(KeyNone), string(KeyUnavailable), string(KeyAvailable))
	if GZIPLevel(6) != GZIP || GIB() != 1<<30 || PiB != 1<<50 {
		t.Error("constants")
	}
}

func GIB() uint64 { return GiB }

func TestSendOptionsMapToTheStream(t *testing.T) {
	c, s := newStub()
	snap, base := c.Snapshot("tank/a@new"), c.Snapshot("tank/a@old")
	var buf bytes.Buffer
	var got []SendProgress
	err := snap.Send(context.Background(), &buf, SendFrom(base), LargeBlocks(), EmbeddedData(), Compressed(), Raw(),
		RedactedWith(c.Bookmark("tank/a#red")), OnProgress(50*time.Millisecond, func(p SendProgress) { got = append(got, p) }))
	if err != nil {
		t.Fatal(err)
	}
	o := s.sendOpts
	if s.last() != "Send tank/a@new" || s.writer != &buf {
		t.Errorf("call = %q", s.last())
	}
	if o.From != "tank/a@old" || o.Intermediates || !o.LargeBlocks || !o.EmbedData || !o.Compress || !o.Raw || o.Saved ||
		o.RedactBookmark != "tank/a#red" || o.ProgressInterval != 50*time.Millisecond || o.Progress == nil {
		t.Errorf("options = %+v", o)
	}
	o.Progress(SendProgress{Bytes: 7})
	if len(got) != 1 || got[0].Bytes != 7 {
		t.Error("the progress callback is not the caller's")
	}

	if err := snap.Send(context.Background(), &buf, SendIntermediates(base)); err != nil {
		t.Fatal(err)
	}
	if o := s.sendOpts; o.From != "tank/a@old" || !o.Intermediates {
		t.Errorf("intermediates = %+v", o)
	}
	if err := snap.Send(context.Background(), &buf, Replicate(), SendProps(), SendHolds(), SkipMissing(), SendFrom(base)); err != nil {
		t.Fatal(err)
	}
	if o := s.sendOpts; !o.Replicate || !o.Props || !o.Holds || !o.SkipMissing || o.From != "tank/a@old" || o.Intermediates {
		t.Errorf("replication options = %+v", o)
	}
	if _, err := c.Dataset("tank/b").Receive(context.Background(), strings.NewReader(""), SkipHolds()); err != nil {
		t.Fatal(err)
	}
	if !s.recvOpts.SkipHolds {
		t.Errorf("SkipHolds did not reach the receive: %+v", s.recvOpts)
	}
	if err := snap.Send(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	if o := s.sendOpts; !reflect.DeepEqual(*o, primitives.SendOptions{}) {
		t.Errorf("a plain send carries options: %+v", o)
	}

	n, err := snap.SendSize(SendFrom(base), Compressed())
	if err != nil || n != 4242 || s.last() != "SendSize tank/a@new" || s.sendOpts.From != "tank/a@old" || !s.sendOpts.Compress {
		t.Errorf("SendSize = %d, %v, %+v", n, err, s.sendOpts)
	}

	if err := c.Dataset("tank/a").SendSaved(context.Background(), &buf, Compressed()); err != nil {
		t.Fatal(err)
	}
	if s.last() != "Send tank/a" || !s.sendOpts.Saved || !s.sendOpts.Compress {
		t.Errorf("SendSaved: %q %+v", s.last(), s.sendOpts)
	}

	if err := c.ResumeSend(context.Background(), &buf, "1-abc-2-00", Compressed()); err != nil {
		t.Fatal(err)
	}
	if s.last() != "ResumeSend 1-abc-2-00" || !s.sendOpts.Compress {
		t.Errorf("ResumeSend: %q", s.last())
	}
}

func TestRedactNamesTheBookmark(t *testing.T) {
	c, s := newStub()
	bm, err := c.Snapshot("tank/a@new").Redact("red", c.Snapshot("tank/clone@r1"), c.Snapshot("tank/clone2@r2"))
	if err != nil || bm.Name() != "tank/a#red" {
		t.Fatalf("Redact = %v, %v", bm, err)
	}
	if s.last() != "Redact tank/a@new tank/a#red [tank/clone@r1 tank/clone2@r2]" {
		t.Errorf("call = %q", s.last())
	}
	if bm, _ := c.Snapshot("tank/a@new").Redact("#r2", c.Snapshot("tank/clone@r1")); bm.Name() != "tank/a#r2" {
		t.Errorf("a # prefix is not stripped: %s", bm.Name())
	}
}

func TestReceiveOptionsMapToTheKernel(t *testing.T) {
	c, s := newStub()
	got, err := c.Dataset("tank/backup/a").Receive(context.Background(), strings.NewReader("x"),
		Force(), Resumable(), Heal(), WithOrigin(c.Snapshot("tank/a@o")), WithCompression(LZ4), WithMountpoint("none"),
		WithProp("note:from", "x"), Parents(), DiscardPool())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Snapshot != "tank/backup/a@got" || s.last() != "Receive tank/backup/a" {
		t.Errorf("result %+v, call %q", got, s.last())
	}
	o := s.recvOpts
	if !o.Force || !o.Resumable || !o.Heal || o.Origin != "tank/a@o" || !o.Parents || o.Layout != primitives.LayoutDiscardPool {
		t.Errorf("options = %+v", o)
	}
	want := map[string]any{"compression": "lz4", "mountpoint": "none", "note:from": "x"}
	if !reflect.DeepEqual(o.Props, want) {
		t.Errorf("props = %v", o.Props)
	}
	if _, err := c.Dataset("tank/backup/b").Receive(context.Background(), strings.NewReader("x"), LastElement()); err != nil || s.recvOpts.Layout != primitives.LayoutLastElement {
		t.Errorf("LastElement: %v %+v", err, s.recvOpts)
	}
	if _, err := c.Snapshot("tank/backup/c@mine").Receive(context.Background(), strings.NewReader("x")); err != nil || s.last() != "Receive tank/backup/c@mine" {
		t.Errorf("snapshot target: %v %q", err, s.last())
	}

	s.token = "1-ab-1-00"
	if tok, ok, err := c.Dataset("tank/backup/a").ResumeToken(); !ok || err != nil || tok != "1-ab-1-00" {
		t.Errorf("ResumeToken = %q %v %v", tok, ok, err)
	}
	if err := c.Dataset("tank/backup/a").AbortResume(); err != nil || s.last() != "AbortResume tank/backup/a" {
		t.Errorf("AbortResume: %v %q", err, s.last())
	}
}

func TestStreamOptionMisuse(t *testing.T) {
	c, s := newStub()
	ctx := context.Background()
	d, snap := c.Dataset("tank/a"), c.Snapshot("tank/a@x")
	key := RawKey(make([]byte, 32))
	cases := map[string]func() error{
		"send option to Receive": func() error { _, err := d.Receive(ctx, strings.NewReader(""), Compressed()); return err },
		"receive option to Send": func() error { return snap.Send(ctx, io.Discard, Force()) },
		"Resumable to Send":      func() error { return snap.Send(ctx, io.Discard, Resumable()) },
		"send option to Set":     func() error { return d.Set(Raw()) },
		"Replicate to Receive":   func() error { _, err := d.Receive(ctx, strings.NewReader(""), Replicate()); return err },
		"SkipHolds to Send":      func() error { return snap.Send(ctx, io.Discard, SkipHolds()) },
		"SendHolds to Create":    func() error { _, err := d.CreateFilesystem("x", SendHolds()); return err },
		"send option to Create":  func() error { _, err := d.CreateFilesystem("x", LargeBlocks()); return err },
		"encryption to Receive": func() error {
			_, err := d.Receive(ctx, strings.NewReader(""), WithEncryption(AES256GCM, key))
			return err
		},
		"Recursive to Receive":      func() error { _, err := d.Receive(ctx, strings.NewReader(""), Recursive()); return err },
		"property to Send":          func() error { return snap.Send(ctx, io.Discard, WithCompression(LZ4)) },
		"layout to SendSize":        func() error { _, err := snap.SendSize(DiscardPool()); return err },
		"send option to ResumeSend": func() error { return c.ResumeSend(ctx, io.Discard, "x", Force()) },
	}
	for name, f := range cases {
		if err := f(); !errors.Is(err, ErrInvalidOption) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(s.calls) != 0 {
		t.Errorf("calls were made: %v", s.calls)
	}
}

func TestStreamErrorsClassify(t *testing.T) {
	c, s := newStub()
	s.err = primitives.ErrCompoundStream
	_, err := c.Dataset("tank/a").Receive(context.Background(), strings.NewReader(""))
	if !errors.Is(err, ErrReplicationStream) {
		t.Errorf("replication stream: %v", err)
	}
	s.err = &ioctl.Error{Ioc: ioctl.IOCRecvNew, Name: "tank", Errno: ioctl.ErrStreamTruncated}
	err = c.Snapshot("tank/a@x").Send(context.Background(), io.Discard)
	if !errors.Is(err, ErrStreamTruncated) {
		t.Errorf("truncated: %v", err)
	}
	var oe *OpError
	if !errors.As(err, &oe) || oe.Op != "send" || !strings.Contains(err.Error(), "truncated") {
		t.Errorf("error = %v", err)
	}
	s.err = &ioctl.Error{Ioc: ioctl.IOCRecvNew, Name: "tank", Errno: syscall.EEXIST}
	if _, err := c.Dataset("tank/a").Receive(context.Background(), strings.NewReader("")); !errors.Is(err, ErrExists) {
		t.Errorf("exists: %v", err)
	}
}

func TestMountingOptionsMapToTheKernel(t *testing.T) {
	c, s := newStub()
	tank := c.Dataset("tank")
	if _, err := tank.CreateFilesystem("quiet", NoMount()); err != nil || !s.createOpts.NoMount {
		t.Errorf("create: %+v %v", s.createOpts, err)
	}
	if _, err := tank.CreateFilesystem("loud"); err != nil || s.createOpts.NoMount {
		t.Errorf("default create mounts: %+v %v", s.createOpts, err)
	}
	if _, err := c.Snapshot("tank/a@s").Clone("tank/c", NoMount()); err != nil || !s.createOpts.NoMount {
		t.Errorf("clone: %+v %v", s.createOpts, err)
	}
	if err := tank.Destroy(Recursive(), ForceUnmount()); err != nil || !s.destroyOpt.Force || !s.destroyOpt.Recursive {
		t.Errorf("destroy: %+v %v", s.destroyOpt, err)
	}

	if _, err := tank.Receive(context.Background(), strings.NewReader(""), NoMount(), DryRun(), Exclude("compression", "note:x"), Exclude("quota")); err != nil {
		t.Fatal(err)
	}
	o := s.recvOpts
	if !o.NoMount || !o.DryRun || !reflect.DeepEqual(o.Exclude, []string{"compression", "note:x", "quota"}) {
		t.Errorf("receive options = %+v", o)
	}

	if err := tank.Mount(Overlay(), Remount(), WithMountOptions("ro", "nosuid"), WithMountKey(PassphraseKey("x")), MountRedacted()); err != nil {
		t.Fatal(err)
	}
	m := s.mountOpts
	if !m.Overlay || !m.Remount || !m.Force || m.Key == nil || !reflect.DeepEqual(m.Options, []string{"ro", "nosuid"}) {
		t.Errorf("mount options = %+v", m)
	}
	if err := tank.MountAll(Overlay()); err != nil || s.last() != "MountAll tank" {
		t.Errorf("MountAll: %v %q", err, s.last())
	}
	if err := tank.Unmount(ForceUnmount(), AndUnloadKey()); err != nil || !s.unmountOpts.Force || !s.unmountOpts.UnloadKey {
		t.Errorf("unmount options = %+v %v", s.unmountOpts, err)
	}
	if err := tank.UnmountAll(); err != nil || s.last() != "UnmountAll tank" {
		t.Errorf("UnmountAll: %v %q", err, s.last())
	}
	if w, ok, err := tank.IsMounted(); err != nil || !ok || w != "/tank" {
		t.Errorf("IsMounted = %q %v %v", w, ok, err)
	}
	if p, err := tank.MountPoint(); err != nil || p != "/tank" {
		t.Errorf("MountPoint = %q %v", p, err)
	}
	if ms, err := c.Mounts(); err != nil || len(ms) != 1 || ms[0].Mountpoint != "/tank/a" {
		t.Errorf("Mounts = %v %v", ms, err)
	}
}

func TestMountingOptionMisuse(t *testing.T) {
	c, s := newStub()
	tank := c.Dataset("tank")
	snap := c.Snapshot("tank/a@s")
	var buf bytes.Buffer
	for name, f := range map[string]func() error{
		"NoMount on Set":          func() error { return tank.Set(NoMount()) },
		"NoMount on Send":         func() error { return snap.Send(context.Background(), &buf, NoMount()) },
		"Overlay on Create":       func() error { _, err := tank.CreateFilesystem("x", Overlay()); return err },
		"Overlay on Unmount":      func() error { return tank.Unmount(Overlay()) },
		"AndUnloadKey on Mount":   func() error { return tank.Mount(AndUnloadKey()) },
		"ForceUnmount on Mount":   func() error { return tank.Mount(ForceUnmount()) },
		"ForceUnmount on snap":    func() error { return snap.Destroy(ForceUnmount()) },
		"Exclude on Send":         func() error { return snap.Send(context.Background(), &buf, Exclude("a")) },
		"Exclude on Create":       func() error { _, err := tank.CreateFilesystem("x", Exclude("a")); return err },
		"DryRun on Send":          func() error { return snap.Send(context.Background(), &buf, DryRun()) },
		"DryRun on Create":        func() error { _, err := tank.CreateFilesystem("x", DryRun()); return err },
		"WithMountKey on Receive": func() error { _, err := tank.Receive(context.Background(), &buf, WithMountKey(nil)); return err },
	} {
		if err := f(); !errors.Is(err, ErrInvalidOption) {
			t.Errorf("%s: err = %v, want ErrInvalidOption", name, err)
		}
	}
	if len(s.calls) != 0 {
		t.Errorf("misused options reached the backend: %v", s.calls)
	}
}

func TestMountErrorsClassify(t *testing.T) {
	c, s := newStub()
	tank := c.Dataset("tank")
	s.err = &primitives.NotMountableError{Dataset: "tank", Reason: "mountpoint is none"}
	if err := tank.Mount(); !errors.Is(err, ErrNotMountable) {
		t.Errorf("not mountable: %v", err)
	}
	s.err = &primitives.MountError{Op: "mount", Dataset: "tank", Mountpoint: "/tank", Err: primitives.ErrMountpointNotEmpty}
	if err := tank.Mount(); !errors.Is(err, ErrMountpointNotEmpty) {
		t.Errorf("not empty: %v", err)
	}
	s.err = &primitives.MountError{Op: "unmount", Dataset: "tank", Mountpoint: "/tank", Err: syscall.EBUSY}
	if err := tank.Unmount(); !errors.Is(err, ErrBusy) {
		t.Errorf("busy: %v", err)
	}
	s.err = &primitives.MountError{Op: "mount", Dataset: "tank", Mountpoint: "/tank", Err: primitives.ErrKeyNotLoaded}
	if err := tank.Mount(); !errors.Is(err, ErrKey) {
		t.Errorf("key: %v", err)
	}
	var oe *OpError
	if err := tank.Mount(); !errors.As(err, &oe) || oe.Op != "mount" {
		t.Errorf("err = %v", err)
	}
}

func TestReadOnlyPoolClassifies(t *testing.T) {
	c, s := newStub()
	s.err = &ioctl.Error{Ioc: ioctl.IOCRecvNew, Name: "tank", Errno: syscall.EROFS}
	_, err := c.Dataset("tank/a").Receive(context.Background(), strings.NewReader(""))
	if !errors.Is(err, ErrReadOnlyPool) {
		t.Errorf("receive: %v", err)
	}
}

func TestSnapshotDiffPassesTheNamesAndTheEntries(t *testing.T) {
	c, s := newStub()
	a, b := c.Snapshot("tank/a@s1"), c.Snapshot("tank/a@s2")
	n := 0
	for e, err := range a.Diff(context.Background(), b) {
		if err != nil || e.Change != DiffAdded || e.Path != "/x" {
			t.Fatalf("%+v %v", e, err)
		}
		n++
	}
	for range a.DiffNow(context.Background()) {
	}
	if n != 1 || s.calls[len(s.calls)-2] != "Diff tank/a@s1 tank/a@s2" || s.last() != "Diff tank/a@s1 " {
		t.Errorf("calls %v", s.calls)
	}
}
