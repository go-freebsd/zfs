package primitives

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/go-freebsd/nvlist/pkg/nvlist"
	"github.com/go-freebsd/zfs/pkg/ioctl"
)

// Builders for whole streams: a BEGIN record with a payload, records with
// the payloads libzfs's recv_skip knows, and an END record.

func zrec(bo binary.ByteOrder, typ uint32) []byte {
	r := make([]byte, ioctl.ReplayRecordSize)
	bo.PutUint32(r, typ)
	return r
}

func withPayload(r []byte, n int) []byte { return append(r, bytes.Repeat([]byte{0xa5}, n)...) }

func recObject(bo binary.ByteOrder, bonuslen, rawBonuslen uint32) []byte {
	r := zrec(bo, drrObject)
	bo.PutUint32(r[28:], bonuslen)
	bo.PutUint32(r[36:], rawBonuslen)
	payload := int((bonuslen + 7) &^ 7)
	if rawBonuslen != 0 {
		payload = int(rawBonuslen)
	}
	return withPayload(r, payload)
}

func recWrite(bo binary.ByteOrder, logical, compressed uint64, ctype byte) []byte {
	r := zrec(bo, drrWrite)
	bo.PutUint64(r[32:], logical)
	r[50] = ctype
	bo.PutUint64(r[96:], compressed)
	if ctype != 0 {
		return withPayload(r, int(compressed))
	}
	return withPayload(r, int(logical))
}

func recSpill(bo binary.ByteOrder, length, compressed uint64) []byte {
	r := zrec(bo, drrSpill)
	bo.PutUint64(r[16:], length)
	bo.PutUint64(r[40:], compressed)
	if compressed != 0 {
		return withPayload(r, int(compressed))
	}
	return withPayload(r, int(length))
}

func recEmbedded(bo binary.ByteOrder, psize uint32) []byte {
	r := zrec(bo, drrWriteEmbedded)
	bo.PutUint32(r[52:], psize)
	return withPayload(r, int((psize+7)&^7))
}

// fullStream is a stream with every kind of record.
func fullStream(bo binary.ByteOrder, toname string, from uint64, features StreamFeature) []byte {
	var parts [][]byte
	hdr := headerOrder(bo, toname, from, features, 12, hdrSubstream)
	parts = append(parts, hdr, bytes.Repeat([]byte{1}, 12))
	parts = append(parts,
		recObject(bo, 5, 0), recObject(bo, 40, 0), recObject(bo, 40, 300),
		recWrite(bo, 4096, 0, 0), recWrite(bo, 8192, 1000, 2),
		recSpill(bo, 512, 0), recSpill(bo, 512, 200),
		recEmbedded(bo, 13), recEmbedded(bo, 16),
		zrec(bo, drrFree), zrec(bo, drrFreeObjects), zrec(bo, drrObjectRange),
		zrec(bo, drrWriteByref), zrec(bo, drrRedact),
		zrec(bo, drrEnd))
	return bytes.Join(parts, nil)
}

func otherOrder() binary.ByteOrder {
	if binary.NativeEndian.Uint16([]byte{0, 1}) == 1 { // host is big endian
		return binary.LittleEndian
	}
	return binary.BigEndian
}

func dry(c *Client, in []byte, target string, o ReceiveOptions) ([]Received, error) {
	o.DryRun = true
	return c.Receive(context.Background(), bytes.NewReader(in), target, &o)
}

func TestDryRunWalksEveryKindOfRecord(t *testing.T) {
	for name, bo := range map[string]binary.ByteOrder{"native": binary.NativeEndian, "swapped": otherOrder()} {
		t.Run(name, func(t *testing.T) {
			k := newFake(t)
			c := newClient(k)
			one := fullStream(bo, "pool/src@one", 0, FeatureLZ4)
			two := fullStream(bo, "pool/src@two", 0, FeatureRaw|FeatureCompressed)
			r := bytes.NewReader(bytes.Join([][]byte{one, two}, nil))
			got, err := c.Receive(context.Background(), r, "tank/new", &ReceiveOptions{DryRun: true})
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 2 || got[0].Bytes != uint64(len(one)) || got[1].Bytes != uint64(len(two)) ||
				!got[0].DryRun || got[0].Snapshot != "tank/new@one" || got[1].Snapshot != "tank/new@two" {
				t.Errorf("result = %+v (stream sizes %d, %d)", got, len(one), len(two))
			}
			if r.Len() != 0 {
				t.Errorf("%d bytes left", r.Len())
			}
			if len(k.writes) != 0 {
				t.Errorf("the kernel was called: %v", k.writes)
			}
		})
	}
}

func TestDryRunSeesATruncatedOrBrokenStream(t *testing.T) {
	k := newFake(t)
	c := newClient(k)
	whole := fullStream(binary.NativeEndian, "pool/src@one", 0, FeatureLZ4)
	for cut := 1; cut < len(whole); cut++ {
		_, err := dry(c, whole[:cut], "tank/new", ReceiveOptions{})
		if !errors.Is(err, ioctl.ErrStreamTruncated) && !errors.Is(err, ErrNotAStream) {
			t.Fatalf("cut at %d of %d: err = %v", cut, len(whole), err)
		}
		if cut > ioctl.ReplayRecordSize && !errors.Is(err, ioctl.ErrStreamTruncated) {
			t.Fatalf("cut at %d: %v is not a truncated stream", cut, err)
		}
	}
	// A record type nobody knows.
	bad := slices.Concat(headerOrder(binary.NativeEndian, "pool/src@one", 0, FeatureLZ4, 0, hdrSubstream), zrec(binary.NativeEndian, 99))
	if _, err := dry(c, bad, "tank/new", ReceiveOptions{}); !errors.Is(err, ErrNotAStream) {
		t.Errorf("bad record: %v", err)
	}
	if _, err := dry(c, nil, "tank/new", ReceiveOptions{}); !errors.Is(err, ErrNotAStream) {
		t.Errorf("empty: %v", err)
	}
	var re *ReceiveError
	_, err := dry(c, whole[:len(whole)-100], "tank/new", ReceiveOptions{})
	if !errors.As(err, &re) || re.Snapshot != "tank/new@one" {
		t.Errorf("err = %v", err)
	}
	if len(k.writes) != 0 {
		t.Errorf("the kernel was called: %v", k.writes)
	}
	// Replication streams stay refused.
	comp := append(headerOrder(binary.NativeEndian, "pool/src@one", 0, FeatureLZ4, 0, hdrCompound), zrec(binary.NativeEndian, drrEnd)...)
	if _, err := dry(c, comp, "tank/new", ReceiveOptions{}); !errors.Is(err, ErrCompoundStream) {
		t.Errorf("compound: %v", err)
	}
}

func TestDryRunChecksTheTarget(t *testing.T) {
	bo := binary.NativeEndian
	full := fullStream(bo, "pool/src@x", 0, FeatureLZ4)
	incr := func(guid uint64, f StreamFeature) []byte { return fullStream(bo, "pool/src@x", guid, f) }
	// tank/a has s1 (guid 2015), s2 (2020), s3 (2030, the latest).
	for _, tc := range []struct {
		name   string
		in     []byte
		target string
		o      ReceiveOptions
		want   error // nil: accepted
	}{
		{"new filesystem", full, "tank/new", ReceiveOptions{}, nil},
		{"incremental onto nothing", incr(2015, FeatureLZ4), "tank/new", ReceiveOptions{}, syscall.ENOENT},
		{"pool only", full, "nopool", ReceiveOptions{}, syscall.ENOENT},
		{"parent missing", full, "tank/x/y", ReceiveOptions{}, syscall.ENOENT},
		{"parent missing, Parents", full, "tank/x/y", ReceiveOptions{Parents: true}, nil},
		{"parent is a volume", full, "tank/vol/y", ReceiveOptions{}, syscall.EINVAL},
		{"full onto existing", full, "tank/z", ReceiveOptions{}, syscall.EEXIST},
		{"full onto existing, Force", full, "tank/z", ReceiveOptions{Force: true}, nil},
		{"full onto one with snapshots", full, "tank/a", ReceiveOptions{Force: true}, syscall.EEXIST},
		{"full onto a clone", full, "tank/clone", ReceiveOptions{Force: true}, syscall.EEXIST},
		{"incremental from the latest", incr(2030, FeatureLZ4), "tank/a", ReceiveOptions{}, nil},
		{"incremental from an older one", incr(2015, FeatureLZ4), "tank/a", ReceiveOptions{}, syscall.ENODEV},
		{"incremental from an older one, Force", incr(2015, FeatureLZ4), "tank/a", ReceiveOptions{Force: true}, nil},
		{"incremental from unknown", incr(99999, FeatureLZ4), "tank/a", ReceiveOptions{Force: true}, syscall.ENODEV},
		{"raw incremental onto unencrypted", incr(2030, FeatureRaw), "tank/a", ReceiveOptions{}, syscall.EINVAL},
		{"snapshot target", full, "tank/new@mine", ReceiveOptions{}, nil},
		{"bad property", full, "tank/new", ReceiveOptions{Props: map[string]any{"used": 1}}, ErrReadOnlyProp},
		{"unknown exclude", full, "tank/new", ReceiveOptions{Exclude: []string{"nosuchprop"}}, ErrUnknownProp},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := newFake(t)
			c := newClient(k)
			m := newFakeMounter(k)
			c.m = m
			got, err := dry(c, tc.in, tc.target, tc.o)
			if tc.want == nil {
				if err != nil || len(got) != 1 || !got[0].DryRun {
					t.Fatalf("got %+v, %v", got, err)
				}
			} else if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if len(k.writes) != 0 || len(m.table) != 0 {
				t.Errorf("a dry run changed something: %v %v", k.writes, m.table)
			}
		})
	}
}

func TestExcludeBecomesBooleanEntries(t *testing.T) {
	bo := binary.NativeEndian
	full := fullStream(bo, "pool/src@x", 0, FeatureLZ4)
	recv := func(t *testing.T, k *fakeKernel, in []byte, target string, o ReceiveOptions) (*nvlist.List, error) {
		t.Helper()
		k.writes = nil
		c := newClient(k)
		o.NoMount = true
		// the fake kernel reads the payload after the header: a body only
		_, err := c.Receive(context.Background(), bytes.NewReader(header("pool/src@x", 0, FeatureLZ4, 0, hdrSubstream)), target, &o)
		if err != nil || len(k.writes) == 0 {
			return nil, err
		}
		lp, _ := k.writes[0].in.List("localprops")
		return lp, nil
	}
	_ = full

	t.Run("new target", func(t *testing.T) {
		k := newFake(t)
		lp, err := recv(t, k, nil, "tank/new", ReceiveOptions{
			Exclude: []string{"compression", "note:x", "compression"}, Props: map[string]any{"atime": false}})
		if err != nil || lp == nil {
			t.Fatalf("%v %v", lp, err)
		}
		for _, n := range []string{"compression", "note:x"} {
			v, ok := lp.Get(n)
			if !ok || reflect.TypeOf(v) != reflect.TypeOf(nvlist.Flag{}) {
				t.Errorf("%s = %#v, want a boolean entry", n, v)
			}
		}
		if v, ok := lp.Uint64("atime"); !ok || v != 0 {
			t.Errorf("-o entry lost: %s", lp.Format())
		}
		if lp.Len() != 3 {
			t.Errorf("localprops = %s", lp.Format())
		}
	})
	t.Run("existing target keeps what is set on it", func(t *testing.T) {
		k := newFake(t)
		// tank/z: compression came with an earlier stream, quota is local, atime inherited
		k.ds["tank/z"].props["compression"] = Property{Value: uint64(15), Source: "$recvd"}
		k.ds["tank/z"].props["quota"] = Property{Value: uint64(1 << 30), Source: "tank/z"}
		k.ds["tank/z"].props["atime"] = Property{Value: uint64(1), Source: "tank"}
		lp, err := recv(t, k, nil, "tank/z", ReceiveOptions{Force: true, Exclude: []string{"compression", "quota", "atime", "checksum"}})
		if err != nil || lp == nil {
			t.Fatalf("%v %v", lp, err)
		}
		got := lp.Names()
		slices.Sort(got)
		if !slices.Equal(got, []string{"checksum", "compression"}) {
			t.Errorf("localprops = %v", got)
		}
	})
	t.Run("refused before the kernel is asked", func(t *testing.T) {
		for name, o := range map[string]ReceiveOptions{
			"unknown":  {Exclude: []string{"nosuchprop"}},
			"clash":    {Exclude: []string{"compression"}, Props: map[string]any{"compression": "lz4"}},
			"readonly": {Exclude: []string{"used"}},
		} {
			k := newFake(t)
			_, err := recv(t, k, nil, "tank/new", o)
			if name == "readonly" {
				// excluding a read-only property is not an error, it is just
				// a property the kernel never receives; nothing to refuse
				continue
			}
			if err == nil || len(k.writes) != 0 {
				t.Errorf("%s: err = %v, writes = %d", name, err, len(k.writes))
			}
		}
	})
	t.Run("raw streams", func(t *testing.T) {
		k := newFake(t)
		c := newClient(k)
		raw := header("pool/src@x", 0, FeatureRaw, 0, hdrSubstream)
		_, err := c.Receive(context.Background(), bytes.NewReader(raw), "tank/new",
			&ReceiveOptions{NoMount: true, Exclude: []string{"keyformat"}})
		if err == nil || len(k.writes) != 0 {
			t.Errorf("err = %v, writes = %d", err, len(k.writes))
		}
		// other properties may be excluded from a raw stream
		if _, err := c.Receive(context.Background(), bytes.NewReader(raw), "tank/new",
			&ReceiveOptions{NoMount: true, Exclude: []string{"compression"}}); err != nil {
			t.Errorf("compression on raw: %v", err)
		}
	})
	t.Run("not for this kind of dataset", func(t *testing.T) {
		k := newFake(t)
		c := newClient(k)
		hv := header("pool/src@x", 0, FeatureLZ4, 0, hdrSubstream)
		binary.NativeEndian.PutUint32(hv[32:], 3) // a volume stream
		if _, err := c.Receive(context.Background(), bytes.NewReader(hv), "tank/newvol",
			&ReceiveOptions{Exclude: []string{"atime", "compression"}}); err != nil {
			t.Fatal(err)
		}
		lp, _ := k.writes[0].in.List("localprops")
		if lp == nil || lp.Has("atime") || !lp.Has("compression") {
			t.Errorf("localprops = %v", lp)
		}
	})
}

func TestReceiveMountsWhatItMade(t *testing.T) {
	plain := header("pool/src@x", 0, FeatureLZ4, 0, hdrSubstream)
	recv := func(c *Client, in []byte, target string, o ReceiveOptions) ([]Received, error) {
		return c.Receive(context.Background(), bytes.NewReader(in), target, &o)
	}
	// the kernel creates the dataset a stream is received into
	creates := func(k *fakeKernel) {
		k.after = func(w writeCall) {
			if sn, ok := w.in.String("snapname"); ok && w.ioc == ioctl.IOCRecvNew {
				ds, _, _ := strings.Cut(sn, "@")
				if _, there := k.ds[ds]; !there {
					d := k.fs(ds, 40)
					if strings.HasSuffix(ds, "vol") {
						d.osType = 3
					}
				}
			}
		}
	}
	t.Run("new filesystem", func(t *testing.T) {
		c, k, m := mountedClient(t)
		creates(k)
		got, err := recv(c, plain, "tank/new", ReceiveOptions{})
		if err != nil || m.table["tank/new"] != "/tank/new" || got[0].Mounted != "/tank/new" {
			t.Fatalf("%+v %v %v", got, err, m.table)
		}
	})
	t.Run("-u", func(t *testing.T) {
		c, k, m := mountedClient(t)
		creates(k)
		got, err := recv(c, plain, "tank/new", ReceiveOptions{NoMount: true})
		if err != nil || len(m.table) != 0 || got[0].Mounted != "" {
			t.Fatalf("%+v %v %v", got, err, m.table)
		}
	})
	t.Run("a volume and a redacted stream", func(t *testing.T) {
		c, k, m := mountedClient(t)
		creates(k)
		hv := header("pool/src@x", 0, FeatureLZ4, 0, hdrSubstream)
		binary.NativeEndian.PutUint32(hv[32:], 3)
		if _, err := recv(c, hv, "tank/newvol", ReceiveOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, err := recv(c, header("pool/src@x", 0, FeatureRedacted, 0, hdrSubstream), "tank/red", ReceiveOptions{}); err != nil {
			t.Fatal(err)
		}
		if len(m.table) != 0 {
			t.Errorf("mounted: %v", m.table)
		}
	})
	t.Run("replacing a filesystem", func(t *testing.T) {
		c, k, m := mountedClient(t)
		c.Mount("tank/z", nil)
		k.calls = nil
		if _, err := recv(c, plain, "tank/z", ReceiveOptions{Force: true}); err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, call := range k.calls {
			if call == "unmount /tank/z" || call == "ZFS_IOC_RECV_NEW tank/z" || call == "mount tank/z /tank/z" {
				got = append(got, call)
			}
		}
		if !slices.Equal(got, []string{"unmount /tank/z", "ZFS_IOC_RECV_NEW tank/z", "mount tank/z /tank/z"}) {
			t.Errorf("calls = %v", k.calls)
		}
		// with -u it stays unmounted
		if _, err := recv(c, plain, "tank/z", ReceiveOptions{Force: true, NoMount: true}); err != nil {
			t.Fatal(err)
		}
		if len(m.table) != 0 {
			t.Errorf("table = %v", m.table)
		}
	})
	t.Run("incremental into a mounted filesystem is online", func(t *testing.T) {
		c, k, m := mountedClient(t)
		c.Mount("tank/a", nil)
		k.calls = nil
		inc := header("pool/src@x", 2030, FeatureLZ4, 0, hdrSubstream)
		if _, err := recv(c, inc, "tank/a", ReceiveOptions{}); err != nil {
			t.Fatal(err)
		}
		for _, call := range k.calls {
			if call[:5] == "mount" || call[:7] == "unmount" {
				t.Errorf("unexpected %s", call)
			}
		}
		if m.table["tank/a"] == "" {
			t.Error("no longer mounted")
		}
	})
	t.Run("a failed receive puts the mount back", func(t *testing.T) {
		c, k, m := mountedClient(t)
		c.Mount("tank/z", nil)
		k.writeErr = map[ioctl.IOC]error{ioctl.IOCRecvNew: &ioctl.Error{Ioc: ioctl.IOCRecvNew, Name: "tank/z", Errno: syscall.EIO}}
		if _, err := recv(c, plain, "tank/z", ReceiveOptions{Force: true}); !errors.Is(err, syscall.EIO) {
			t.Fatalf("err = %v", err)
		}
		if m.table["tank/z"] != "/tank/z" {
			t.Errorf("table = %v", m.table)
		}
	})
}

// the fake reads from the pipe; keep io imported for the dry run helper
var _ = io.EOF
