package primitives

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/go-freebsd/nvlist/pkg/nvlist"
	"github.com/go-freebsd/zfs/pkg/ioctl"
)

func TestFletcher4Vectors(t *testing.T) {
	// Worked by hand: words 1 and 2 give a=3, b=1+3, c=1+4, d=1+5.
	var z fletcher4
	z.write(binary.LittleEndian, []byte{1, 0, 0, 0, 2, 0, 0, 0})
	if z != (fletcher4{3, 4, 5, 6}) {
		t.Fatalf("got %v", z)
	}
	// Incremental calls equal one call.
	var a, b fletcher4
	data := make([]byte, 64)
	for i := range data {
		data[i] = byte(i*7 + 3)
	}
	a.write(binary.BigEndian, data)
	b.write(binary.BigEndian, data[:24])
	b.write(binary.BigEndian, data[24:])
	if a != b {
		t.Fatal("not incremental")
	}
	// The byte order matters.
	var c fletcher4
	c.write(binary.LittleEndian, data)
	if c == a {
		t.Fatal("byte order ignored")
	}
}

func testPackage(t *testing.T) (*nvlist.List, []byte) {
	t.Helper()
	snaps := nvlist.New()
	snaps.Add("a", uint64(11))
	snaps.Add("b", uint64(12))
	fs := nvlist.New()
	fs.Add("name", "tank/fs")
	fs.Add("parentfromsnap", uint64(0))
	fs.Add("snaps", snaps)
	fss := nvlist.New()
	fss.Add("0x5", fs)
	hdr := nvlist.New()
	hdr.Add("fromsnap", "a")
	hdr.Add("tosnap", "b")
	hdr.Add("fss", fss)
	b, err := nvlist.PackXDR(hdr)
	if err != nil {
		t.Fatal(err)
	}
	return hdr, b
}

func TestPackageHeaderRoundTrip(t *testing.T) {
	_, payload := testPackage(t)
	for name, order := range map[string]binary.ByteOrder{"native": binary.NativeEndian, "swapped": otherOrder()} {
		var buf bytes.Buffer
		if err := writePackageHeader(&buf, order, "tank/fs@b", FeatureSASpill|FeatureHolds, payload); err != nil {
			t.Fatal(err)
		}
		writeEnd(&buf, order, nil)
		stream := buf.Bytes()
		r := bytes.NewReader(stream)
		h, end, err := readHeaderOrEnd(r)
		if err != nil || end {
			t.Fatalf("%s: %v %v", name, err, end)
		}
		if !h.Compound || h.ToName != "tank/fs@b" || h.Features != FeatureSASpill|FeatureHolds || h.PayloadLen != uint32(len(payload)) {
			t.Fatalf("%s: header %+v", name, h)
		}
		p, err := readPackage(r, h)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if p.FromSnap != "a" || p.ToSnap != "b" || !p.Recursive() || p.Raw {
			t.Errorf("%s: %+v", name, p)
		}
		fs, snap, ok := p.find(12)
		if !ok || snap != "b" {
			t.Fatalf("%s: find: %v %q", name, ok, snap)
		}
		if n, _ := fs.String("name"); n != "tank/fs" {
			t.Errorf("%s: fs %q", name, n)
		}
		if _, _, ok := p.find(99); ok {
			t.Errorf("%s: found a missing GUID", name)
		}
		if _, end, err := readHeaderOrEnd(r); err != nil || !end {
			t.Errorf("%s: closing record: end=%v err=%v", name, end, err)
		}
		if _, err := r.ReadByte(); err != io.EOF {
			t.Errorf("%s: trailing data", name)
		}
	}
}

func TestPackageHeaderWithoutPayload(t *testing.T) {
	var buf bytes.Buffer
	if err := writePackageHeader(&buf, binary.NativeEndian, "tank/fs@b", 0, nil); err != nil {
		t.Fatal(err)
	}
	r := bytes.NewReader(buf.Bytes())
	h, _, err := readHeaderOrEnd(r)
	if err != nil {
		t.Fatal(err)
	}
	p, err := readPackage(r, h)
	if err != nil || p.FSS != nil || len(p.Filesystems()) != 0 {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestPackageHeaderDamage(t *testing.T) {
	_, payload := testPackage(t)
	var buf bytes.Buffer
	writePackageHeader(&buf, binary.NativeEndian, "tank/fs@b", 0, payload)
	good := buf.Bytes()

	read := func(b []byte) error {
		r := bytes.NewReader(b)
		h, _, err := readHeaderOrEnd(r)
		if err != nil {
			return err
		}
		_, err = readPackage(r, h)
		return err
	}
	if err := read(good); err != nil {
		t.Fatal(err)
	}
	// Every flipped byte of the payload or the END checksum is noticed.
	for i := ioctl.ReplayRecordSize; i < len(good); i++ {
		if i >= ioctl.ReplayRecordSize+len(payload) && i < ioctl.ReplayRecordSize+len(payload)+endRecordChecksumOff {
			continue // record type and padding of END: other errors
		}
		if i >= ioctl.ReplayRecordSize+len(payload)+endRecordChecksumOff+32 {
			continue // after the checksum field
		}
		b := append([]byte(nil), good...)
		b[i] ^= 0x40
		if err := read(b); err == nil {
			t.Fatalf("flipped byte %d went unnoticed", i)
		}
	}
	// Every prefix is a truncated stream, never a panic or a success.
	for i := 0; i < len(good); i++ {
		err := read(good[:i])
		if err == nil {
			t.Fatalf("prefix of %d bytes accepted", i)
		}
		if i >= ioctl.ReplayRecordSize && !errors.Is(err, ioctl.ErrStreamTruncated) {
			t.Fatalf("prefix of %d bytes: %v", i, err)
		}
	}
	// A missing END record.
	b := append([]byte(nil), good...)
	binary.NativeEndian.PutUint32(b[ioctl.ReplayRecordSize+len(payload):], drrWrite)
	if err := read(b); !errors.Is(err, ErrBadPackage) {
		t.Fatalf("no END: %v", err)
	}
	// A header that claims too much.
	big := append([]byte(nil), good...)
	binary.NativeEndian.PutUint32(big[4:], maxPackageNvlist+4)
	if err := read(big); !errors.Is(err, ErrBadPackage) {
		t.Fatalf("huge payload: %v", err)
	}
}

func TestGUIDKey(t *testing.T) {
	if k := guidKey(0xdeadbeef); k != "0xdeadbeef" {
		t.Fatal(k)
	}
	if g, ok := parseGUIDKey("0xdeadbeef"); !ok || g != 0xdeadbeef {
		t.Fatal(g, ok)
	}
	if _, ok := parseGUIDKey("deadbeef"); ok {
		t.Fatal("no prefix accepted")
	}
}
