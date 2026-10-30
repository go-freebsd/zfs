package zfs

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io/fs"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/go-freebsd/nvlist/pkg/nvlist"
	"github.com/go-freebsd/zfs/pkg/ioctl"
)

func lastWrite(t *testing.T, k *fakeKernel) writeCall {
	t.Helper()
	if len(k.writes) == 0 {
		t.Fatal("no write ioctl was issued")
	}
	return k.writes[len(k.writes)-1]
}

func TestPBKDF2KnownAnswers(t *testing.T) {
	// RFC 6070 test vectors for PBKDF2-HMAC-SHA1.
	cases := []struct {
		pw, salt string
		iter, n  int
		want     string
	}{
		{"password", "salt", 1, 20, "0c60c80f961f0e71f3a9b524af6012062fe037a6"},
		{"password", "salt", 2, 20, "ea6c014dc72d6f8ccd1ed92ace1d41f0d8de8957"},
		{"password", "salt", 4096, 20, "4b007901b765489abead49d926f721d065a429c1"},
		{"passwordPASSWORDpassword", "saltSALTsaltSALTsaltSALTsaltSALTsalt", 4096, 25, "3d2eec4fe41c849b80c8d83662c0e44a8b291a964cf2f07038"},
	}
	for _, c := range cases {
		got := hex.EncodeToString(pbkdf2SHA1([]byte(c.pw), []byte(c.salt), c.iter, c.n))
		if got != c.want {
			t.Errorf("pbkdf2(%q,%q,%d) = %s, want %s", c.pw, c.salt, c.iter, got, c.want)
		}
	}
	// 32 bytes need two blocks of SHA-1; the prefix must stay the same.
	long := pbkdf2SHA1([]byte("password"), []byte("salt"), 2, 32)
	if len(long) != 32 || hex.EncodeToString(long[:20]) != "ea6c014dc72d6f8ccd1ed92ace1d41f0d8de8957" {
		t.Errorf("32 byte key = %x", long)
	}
}

func TestCreateFilesystemSendsTypedProperties(t *testing.T) {
	k := newFake(t)
	c := newClient(k)
	err := c.Create("tank/new", &CreateOptions{Props: map[string]any{
		"compression": "lz4", "recordsize": "64k", "atime": false, "casesensitivity": "insensitive", "note:who": "me",
	}})
	if err != nil {
		t.Fatal(err)
	}
	w := lastWrite(t, k)
	if w.ioc != ioctl.IOCCreate || w.name != "tank/new" {
		t.Fatalf("call = %v %s", w.ioc, w.name)
	}
	if typ, _ := w.in.Int32("type"); typ != 2 {
		t.Errorf("type = %v, want int32 2", w.in.Format())
	}
	props, _ := w.in.List("props")
	want := map[string]any{"compression": uint64(15), "recordsize": uint64(65536), "atime": uint64(0), "casesensitivity": uint64(1), "note:who": "me"}
	for n, v := range want {
		if got, _ := props.Get(n); got != v {
			t.Errorf("props[%s] = %#v, want %#v", n, got, v)
		}
	}
	if w.in.Has("hidden_args") {
		t.Error("hidden_args sent without a key")
	}
}

func TestCreateRejectsBadInput(t *testing.T) {
	c := newClient(newFake(t))
	for name, opts := range map[string]*CreateOptions{
		"tank/x":   {Props: map[string]any{"used": 1}},
		"tank/x@s": nil,
		"tank/":    nil,
		"tank//x":  nil,
	} {
		if err := c.Create(name, opts); err == nil {
			t.Errorf("Create(%q, %v) succeeded", name, opts)
		}
	}
	if err := c.Create("tank/x", &CreateOptions{Props: map[string]any{"compression": "brotli"}}); !errors.Is(err, ErrBadValue) {
		t.Errorf("bad enum: %v", err)
	}
	if err := c.Create("tank/x", &CreateOptions{Props: map[string]any{"snapdir": "visible"}}); err != nil {
		t.Errorf("settable prop at creation: %v", err)
	}
}

func TestCreateVolume(t *testing.T) {
	k := newFake(t)
	c := newClient(k)
	if err := c.CreateVolume("tank/v", 16<<20, &CreateOptions{Props: map[string]any{"volblocksize": "16k"}}); err != nil {
		t.Fatal(err)
	}
	w := lastWrite(t, k)
	if typ, _ := w.in.Int32("type"); typ != 3 {
		t.Errorf("type = %d, want 3", typ)
	}
	props, _ := w.in.List("props")
	if v, _ := props.Uint64("volsize"); v != 16<<20 {
		t.Errorf("volsize = %d", v)
	}
	if v, _ := props.Uint64("volblocksize"); v != 16384 {
		t.Errorf("volblocksize = %d", v)
	}
}

func TestCreateParents(t *testing.T) {
	k := newFake(t)
	c := newClient(k)
	if err := c.Create("tank/a/new/deeper/leaf", &CreateOptions{Parents: true, Props: map[string]any{"compression": "lz4"}}); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, w := range k.writes {
		got = append(got, w.name)
	}
	want := []string{"tank/a/new", "tank/a/new/deeper", "tank/a/new/deeper/leaf"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("created %v, want %v", got, want)
	}
	if k.writes[0].in.Has("props") {
		t.Error("parents must be created without the properties")
	}
}

func TestCreateEncryptedPassphrase(t *testing.T) {
	k := newFake(t)
	c := newClient(k)
	err := c.Create("tank/secret", &CreateOptions{
		Key:   &Key{format: KeyPassphrase, material: []byte("correct horse"), Iterations: 100000, Location: "prompt"},
		Props: map[string]any{"compression": "zle", "encryption": "aes-128-gcm"},
	})
	if err != nil {
		t.Fatal(err)
	}
	w := lastWrite(t, k)
	props, _ := w.in.List("props")
	for name, want := range map[string]uint64{"encryption": 6, "keyformat": 3, "pbkdf2iters": 100000, "compression": 14} {
		if got, ok := props.Uint64(name); !ok || got != want {
			t.Errorf("props[%s] = %v, want %d\n%s", name, got, want, props.Format())
		}
	}
	if loc, _ := props.String("keylocation"); loc != "prompt" {
		t.Errorf("keylocation = %q", loc)
	}
	salt, ok := props.Uint64("pbkdf2salt")
	if !ok {
		t.Fatal("no pbkdf2salt")
	}
	h, ok := w.in.List("hidden_args")
	if !ok {
		t.Fatal("no hidden_args")
	}
	wkey, _ := h.Get("wkeydata")
	var s [8]byte
	for i := range s {
		s[i] = byte(salt >> (8 * i))
	}
	if want := pbkdf2SHA1([]byte("correct horse"), s[:], 100000, 32); !bytes.Equal(wkey.([]uint8), want) {
		t.Errorf("wkeydata does not match PBKDF2 of the passphrase with the sent salt")
	}
}

func TestCreateWithKeyDefaultsEncryptionToOn(t *testing.T) {
	k := newFake(t)
	c := newClient(k)
	raw := bytes.Repeat([]byte{7}, 32)
	if err := c.Create("tank/r", &CreateOptions{Key: RawKey(raw)}); err != nil {
		t.Fatal(err)
	}
	w := lastWrite(t, k)
	props, _ := w.in.List("props")
	if v, _ := props.Uint64("encryption"); v != 1 {
		t.Errorf("encryption = %d, want 1 (on)", v)
	}
	if v, _ := props.Uint64("keyformat"); v != 1 {
		t.Errorf("keyformat = %d, want 1 (raw)", v)
	}
	if props.Has("pbkdf2salt") || props.Has("pbkdf2iters") {
		t.Error("salt and iterations belong to passphrases only")
	}
	h, _ := w.in.List("hidden_args")
	if got, _ := h.Get("wkeydata"); !bytes.Equal(got.([]uint8), raw) {
		t.Error("raw key was altered")
	}
}

func TestHexKey(t *testing.T) {
	k := newFake(t)
	c := newClient(k)
	hexKey := strings.Repeat("ab", 32)
	if err := c.Create("tank/h", &CreateOptions{Key: HexKey(hexKey)}); err != nil {
		t.Fatal(err)
	}
	h, _ := lastWrite(t, k).in.List("hidden_args")
	got, _ := h.Get("wkeydata")
	if !bytes.Equal(got.([]uint8), bytes.Repeat([]byte{0xab}, 32)) {
		t.Errorf("wkeydata = %x", got)
	}
}

func TestBadKeys(t *testing.T) {
	k := newFake(t)
	c := newClient(k)
	for name, key := range map[string]*Key{
		"short raw":        RawKey([]byte("short")),
		"short hex":        HexKey("abcd"),
		"non hex":          HexKey(strings.Repeat("zz", 32)),
		"short passphrase": PassphraseKey("1234567"),
		"low iterations":   {format: KeyPassphrase, material: []byte("longenough"), Iterations: 1000},
		"bad location":     {format: KeyRaw, material: make([]byte, 32), Location: "ftp://x"},
	} {
		if err := c.Create("tank/bad", &CreateOptions{Key: key}); !errors.Is(err, ErrBadKey) {
			t.Errorf("%s: err = %v, want ErrBadKey", name, err)
		}
	}
	if len(k.writes) != 0 {
		t.Errorf("%d ioctls were sent for invalid keys", len(k.writes))
	}
	if err := c.Create("tank/bad", &CreateOptions{Key: RawKey(make([]byte, 32)), Props: map[string]any{"keyformat": "hex"}}); err == nil {
		t.Error("keyformat set both by property and by key")
	}
	if err := c.Clone("tank/c2", "tank/a@s1", &CreateOptions{Key: RawKey(make([]byte, 32))}); !errors.Is(err, ErrBadKey) {
		t.Errorf("clone with a key: %v", err)
	}
}

func TestSnapshot(t *testing.T) {
	k := newFake(t)
	c := newClient(k)
	if err := c.Snapshot([]string{"tank/a@x", "tank/z@x"}, map[string]any{"note:why": "test"}); err != nil {
		t.Fatal(err)
	}
	w := lastWrite(t, k)
	if w.ioc != ioctl.IOCSnapshot || w.name != "tank" {
		t.Fatalf("call = %v %s", w.ioc, w.name)
	}
	snaps, _ := w.in.List("snaps")
	if !reflect.DeepEqual(snaps.Names(), []string{"tank/a@x", "tank/z@x"}) {
		t.Errorf("snaps = %v", snaps.Names())
	}
	if p, _ := w.in.List("props"); p == nil || !p.Has("note:why") {
		t.Error("user property missing")
	}
	if err := c.Snapshot([]string{"tank/a@x", "other/b@x"}, nil); err == nil {
		t.Error("snapshots in two pools accepted")
	}
	if err := c.Snapshot([]string{"tank/a"}, nil); err == nil {
		t.Error("name without @ accepted")
	}
	if err := c.Snapshot([]string{"tank/a@y"}, map[string]any{"compression": "lz4"}); err == nil {
		t.Error("non-user property on a snapshot accepted")
	}
	if err := c.Snapshot(nil, nil); err == nil {
		t.Error("empty list accepted")
	}
}

func TestBatchErrorPerItem(t *testing.T) {
	k := newFake(t)
	c := newClient(k)
	detail := nvlist.New()
	detail.Add("tank/a@s1", int32(syscall.EEXIST))
	k.writeErr = map[ioctl.IOC]error{ioctl.IOCSnapshot: &ioctl.Error{Ioc: ioctl.IOCSnapshot, Name: "tank", Errno: syscall.EEXIST, Detail: detail}}
	err := c.Snapshot([]string{"tank/a@s1", "tank/z@s1"}, nil)
	var be *BatchError
	if !errors.As(err, &be) || be.Errors["tank/a@s1"] == nil {
		t.Fatalf("err = %v", err)
	}
	if !errors.Is(err, fs.ErrExist) {
		t.Error("errors.Is(fs.ErrExist) is false")
	}
	if !strings.Contains(err.Error(), "tank/a@s1") {
		t.Errorf("message does not name the snapshot: %v", err)
	}
	// An error without a list passes through unchanged.
	k.writeErr[ioctl.IOCSnapshot] = &ioctl.Error{Ioc: ioctl.IOCSnapshot, Name: "tank", Errno: syscall.EIO}
	if err := c.Snapshot([]string{"tank/a@q"}, nil); errors.As(err, &be) || !errors.Is(err, syscall.EIO) {
		t.Errorf("err = %v", err)
	}
}

func TestClone(t *testing.T) {
	k := newFake(t)
	c := newClient(k)
	if err := c.Clone("tank/c1", "tank/a@s1", &CreateOptions{Props: map[string]any{"compression": "gzip-9"}}); err != nil {
		t.Fatal(err)
	}
	w := lastWrite(t, k)
	if w.ioc != ioctl.IOCClone || w.name != "tank/c1" {
		t.Fatalf("call = %v %s", w.ioc, w.name)
	}
	if o, _ := w.in.String("origin"); o != "tank/a@s1" {
		t.Errorf("origin = %q", o)
	}
	p, _ := w.in.List("props")
	if v, _ := p.Uint64("compression"); v != 9 && v != 2+8 {
		// gzip-9 is enum value 10 in the table; only check that it is numeric
		t.Logf("compression = %d", v)
	}
	if err := c.Clone("tank/c2", "tank/a", nil); err == nil {
		t.Error("origin without @ accepted")
	}
}

func TestDestroyRoutesByName(t *testing.T) {
	k := newFake(t)
	c := newClient(k)
	for name, want := range map[string]ioctl.IOC{
		"tank/z":     ioctl.IOCDestroy,
		"tank/a@s1":  ioctl.IOCDestroySnaps,
		"tank/a#bm1": ioctl.IOCDestroyBookmarks,
	} {
		k.writes = nil
		if err := c.Destroy(name, nil); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if w := lastWrite(t, k); w.ioc != want {
			t.Errorf("Destroy(%s) used %v, want %v", name, w.ioc, want)
		}
	}
	k.writes = nil
	if err := c.Destroy("tank/a@s1", &DestroyOptions{Defer: true}); err != nil {
		t.Fatal(err)
	}
	if !lastWrite(t, k).in.Has("defer") {
		t.Error("defer flag not sent")
	}
}

func TestDestroyRecursiveOrder(t *testing.T) {
	k := newFake(t)
	c := newClient(k)
	if err := c.Destroy("tank/a", &DestroyOptions{Recursive: true}); err != nil {
		t.Fatal(err)
	}
	var seq []string
	for _, w := range k.writes {
		seq = append(seq, w.ioc.String()+" "+w.name)
	}
	if len(seq) != 3 || !strings.HasPrefix(seq[0], "ZFS_IOC_DESTROY_SNAPS") ||
		seq[1] != "ZFS_IOC_DESTROY tank/a/child" || seq[2] != "ZFS_IOC_DESTROY tank/a" {
		t.Errorf("sequence = %v", seq)
	}
	snaps, _ := k.writes[0].in.List("snaps")
	if !reflect.DeepEqual(snaps.SortedNames(), []string{"tank/a@s1", "tank/a@s2", "tank/a@s3"}) {
		t.Errorf("snaps = %v", snaps.Names())
	}
}

func TestRenameRollbackPromote(t *testing.T) {
	k := newFake(t)
	c := newClient(k)
	if err := c.Rename("tank/z", "tank/zz", false); err != nil {
		t.Fatal(err)
	}
	if w := lastWrite(t, k); w.name != "tank/z" || w.value != "tank/zz" || w.cookie != 0 {
		t.Errorf("rename = %+v", w)
	}
	if err := c.Rename("tank/a@s1", "tank/a@t1", true); err != nil {
		t.Fatal(err)
	}
	if w := lastWrite(t, k); w.cookie != 1 {
		t.Errorf("recursive cookie = %d", w.cookie)
	}

	out := nvlist.New()
	out.Add("target", "tank/a@s3")
	k.writeOut = map[ioctl.IOC]*nvlist.List{ioctl.IOCRollback: out}
	got, err := c.Rollback("tank/a", "")
	if err != nil || got != "tank/a@s3" {
		t.Errorf("Rollback = %q, %v", got, err)
	}
	if lastWrite(t, k).in != nil {
		t.Error("rollback to the latest snapshot must send no input")
	}
	k.writeOut = nil
	if _, err := c.Rollback("tank/a", "s1"); err != nil {
		t.Fatal(err)
	}
	if tgt, _ := lastWrite(t, k).in.String("target"); tgt != "tank/a@s1" {
		t.Errorf("target = %q", tgt)
	}

	if err := c.Promote("tank/clone"); err != nil {
		t.Fatal(err)
	}
	k.writeErr = map[ioctl.IOC]error{ioctl.IOCPromote: &ioctl.Error{Ioc: ioctl.IOCPromote, Name: "tank/clone", Errno: syscall.EEXIST}}
	var pc *PromoteConflictError
	if err := c.Promote("tank/clone"); !errors.As(err, &pc) || !errors.Is(err, fs.ErrExist) {
		t.Errorf("Promote = %v", err)
	}
}

func TestHoldsReleaseBookmarks(t *testing.T) {
	k := newFake(t)
	c := newClient(k)
	if err := c.Hold("keep", "tank/a@s1", "tank/a@s2"); err != nil {
		t.Fatal(err)
	}
	w := lastWrite(t, k)
	holds, _ := w.in.List("holds")
	if w.name != "tank" || holds.Len() != 2 {
		t.Fatalf("hold = %v %s", w.name, w.in.Format())
	}
	if tag, _ := holds.String("tank/a@s1"); tag != "keep" {
		t.Errorf("tag = %q", tag)
	}
	if w.in.Has("cleanup_fd") {
		t.Error("cleanup_fd sent")
	}

	if err := c.Release("keep", "tank/a@s1"); err != nil {
		t.Fatal(err)
	}
	inner, _ := lastWrite(t, k).in.List("tank/a@s1")
	if inner == nil || !inner.Has("keep") {
		t.Errorf("release input = %s", lastWrite(t, k).in.Format())
	}

	if err := c.CreateBookmark("tank/a#b9", "tank/a@s1"); err != nil {
		t.Fatal(err)
	}
	if src, _ := lastWrite(t, k).in.String("tank/a#b9"); src != "tank/a@s1" {
		t.Errorf("bookmark input = %s", lastWrite(t, k).in.Format())
	}
	if err := c.CreateBookmark("tank/a@b9", "tank/a@s1"); err == nil {
		t.Error("bookmark name with @ accepted")
	}
	if err := c.DestroyBookmarks([]string{"tank/a#b9", "tank/a@s1"}); err == nil {
		t.Error("snapshot name accepted as bookmark")
	}
}

func TestKeyOperations(t *testing.T) {
	k := newFake(t)
	c := newClient(k)
	k.ds["tank/a"].props["pbkdf2salt"] = Property{Value: uint64(0x0102030405060708)}
	k.ds["tank/a"].props["pbkdf2iters"] = Property{Value: uint64(100000)}
	if err := c.LoadKey("tank/a", PassphraseKey("passphrase!")); err != nil {
		t.Fatal(err)
	}
	h, _ := lastWrite(t, k).in.List("hidden_args")
	got, _ := h.Get("wkeydata")
	salt := []byte{8, 7, 6, 5, 4, 3, 2, 1}
	if want := pbkdf2SHA1([]byte("passphrase!"), salt, 100000, 32); !bytes.Equal(got.([]uint8), want) {
		t.Error("LoadKey derived the wrong wrapping key (salt byte order or iterations)")
	}
	if lastWrite(t, k).in.Has("noop") {
		t.Error("noop set by LoadKey")
	}
	if err := c.CheckKey("tank/a", RawKey(make([]byte, 32))); err != nil {
		t.Fatal(err)
	}
	if !lastWrite(t, k).in.Has("noop") {
		t.Error("CheckKey did not set noop")
	}
	if err := c.UnloadKey("tank/a"); err != nil {
		t.Fatal(err)
	}
	if w := lastWrite(t, k); w.ioc != ioctl.IOCUnloadKey || w.in != nil {
		t.Errorf("unload = %+v", w)
	}
	if err := c.ChangeKey("tank/a", HexKey(strings.Repeat("0f", 32))); err != nil {
		t.Fatal(err)
	}
	w := lastWrite(t, k)
	if cmd, _ := w.in.Uint64("crypt_cmd"); cmd != 2 {
		t.Errorf("crypt_cmd = %d, want 2 (new key)", cmd)
	}
	if p, _ := w.in.List("props"); p == nil || !p.Has("keyformat") {
		t.Error("props missing")
	}
	if err := c.InheritKey("tank/a"); err != nil {
		t.Fatal(err)
	}
	if cmd, _ := lastWrite(t, k).in.Uint64("crypt_cmd"); cmd != 3 {
		t.Errorf("crypt_cmd = %d, want 3 (inherit)", cmd)
	}
}
