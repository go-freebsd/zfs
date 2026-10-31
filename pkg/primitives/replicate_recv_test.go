package primitives

import (
	"bytes"
	"context"
	"encoding/binary"
	"strings"
	"syscall"
	"testing"

	"github.com/go-freebsd/nvlist/pkg/nvlist"
	"github.com/go-freebsd/zfs/pkg/ioctl"
)

// pkgStream builds a replication stream of one dataset "pool/src" with one
// snapshot "s" (GUID 0xabcdef, as headerOrder gives it) whose header says
// what the sender had: a received property, a snapshot property and a hold.
func pkgStream(t *testing.T, features StreamFeature, extraProps map[string]any) []byte {
	t.Helper()
	props := nvlist.New()
	props.Add("compression", uint64(14))
	props.Add("keylocation", "prompt") // must not be received
	for k, v := range extraProps {
		props.Add(k, v)
	}
	sprops := nvlist.New()
	sprops.Add("note:snap", "hello")
	snapprops := nvlist.New()
	snapprops.Add("s", sprops)
	tags := nvlist.New()
	tags.Add("keep", uint64(1700000000))
	holds := nvlist.New()
	holds.Add("s", tags)
	snaps := nvlist.New()
	snaps.Add("s", uint64(0xabcdef))
	fs := nvlist.New()
	fs.Add("name", "pool/src")
	fs.Add("parentfromsnap", uint64(0))
	fs.Add("props", props)
	fs.Add("snaps", snaps)
	fs.Add("snapprops", snapprops)
	fs.Add("snapholds", holds)
	fss := nvlist.New()
	fss.Add("0x1", fs)
	hdr := nvlist.New()
	hdr.Add("tosnap", "s")
	hdr.Add("fss", fss)
	payload, err := nvlist.PackXDR(hdr)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := writePackageHeader(&b, binary.NativeEndian, "pool/src@s", features, payload); err != nil {
		t.Fatal(err)
	}
	body := []byte("12345678")
	b.Write(headerOrder(binary.NativeEndian, "pool/src@s", 0, FeatureLZ4, uint32(len(body)), hdrSubstream))
	b.Write(body)
	writeEnd(&b, binary.NativeEndian, nil)
	return b.Bytes()
}

func writesOf(k *fakeKernel, iocs ...ioctl.IOC) []writeCall {
	var out []writeCall
	for _, w := range k.writes {
		for _, i := range iocs {
			if w.ioc == i {
				out = append(out, w)
			}
		}
	}
	return out
}

// madeByReceive makes the datasets a real kernel would make.
func madeByReceive(k *fakeKernel) {
	k.after = func(w writeCall) {
		if w.ioc != ioctl.IOCRecvNew {
			return
		}
		snapName, _ := w.in.String("snapname")
		ds, tag, _ := strings.Cut(snapName, "@")
		if _, ok := k.ds[ds]; !ok {
			k.fs(ds, 50)
		}
		if _, ok := k.ds[snapName]; !ok {
			k.snap(ds, tag, 51)
		}
	}
}

func TestReceivePackageAppliesWhatTheHeaderSays(t *testing.T) {
	k := newFake(t)
	madeByReceive(k)
	c := newClient(k)
	got, err := c.Receive(context.Background(), bytes.NewReader(pkgStream(t, FeatureHolds, nil)), "tank/new", &ReceiveOptions{NoMount: true})
	if err != nil || len(got) != 1 {
		t.Fatalf("%d streams, %v", len(got), err)
	}
	w := writesOf(k, ioctl.IOCRecvNew, ioctl.IOCHold)
	if len(w) != 2 || w[0].ioc != ioctl.IOCRecvNew || w[1].ioc != ioctl.IOCHold {
		t.Fatalf("calls: %+v", w)
	}
	if len(k.sets) != 1 {
		t.Fatalf("SET_PROP calls: %+v", k.sets)
	}
	// The dataset's properties arrive as received values; keylocation does not.
	props, ok := w[0].in.List("props")
	if !ok {
		t.Fatalf("no props: %s", w[0].in.Format())
	}
	if v, _ := props.Uint64("compression"); v != 14 || props.Has("keylocation") {
		t.Errorf("props = %s", props.Format())
	}
	// A new, unencrypted filesystem is told not to inherit encryption.
	lp, _ := w[0].in.List("localprops")
	if lp == nil {
		t.Fatalf("no localprops: %s", w[0].in.Format())
	}
	if v, _ := lp.Uint64("encryption"); v != 2 {
		t.Errorf("localprops = %s", lp.Format())
	}
	// The snapshot's properties are set as received values (zc_cookie).
	if sp := k.sets[0]; sp.dataset != "tank/new@s" || sp.cookie != 1 || !sp.props.Has("note:snap") {
		t.Errorf("snapshot props: %+v", sp)
	}
	// And the holds come back.
	if hs, _ := w[1].in.List("holds"); hs == nil || !hs.Has("tank/new@s") {
		t.Errorf("holds = %s", w[1].in.Format())
	} else if tag, _ := hs.String("tank/new@s"); tag != "keep" {
		t.Errorf("hold tag %q", tag)
	}
}

func TestReceivePackageHoldsAndExclude(t *testing.T) {
	// SkipHolds drops the holds; an excluded property that cannot be
	// inherited is left out of the received values instead.
	k := newFake(t)
	madeByReceive(k)
	c := newClient(k)
	extra := map[string]any{"version": uint64(5), "atime": uint64(0)}
	_, err := c.Receive(context.Background(), bytes.NewReader(pkgStream(t, FeatureHolds, extra)), "tank/new",
		&ReceiveOptions{NoMount: true, SkipHolds: true, Exclude: []string{"version", "atime"}})
	if err != nil {
		t.Fatal(err)
	}
	if h := writesOf(k, ioctl.IOCHold); len(h) != 0 {
		t.Errorf("holds were put back: %+v", h)
	}
	w := writesOf(k, ioctl.IOCRecvNew)[0]
	props, _ := w.in.List("props")
	lp, _ := w.in.List("localprops")
	if props.Has("version") {
		t.Error("version is not inheritable: it must be left out of the received values")
	}
	if !props.Has("atime") || !lp.Has("atime") {
		t.Errorf("atime is inheritable: it stays in props and is excluded locally\nprops %s\nlocal %s", props.Format(), lp.Format())
	}
	if lp.Has("version") {
		t.Error("an exclusion was sent for a property that cannot be inherited")
	}
}

func TestReceivePackageSkipsASnapshotThatExists(t *testing.T) {
	k := newFake(t)
	c := newClient(k)
	k.fs("tank/new", 40)
	k.snap("tank/new", "s", 41).guid = 0xabcdef
	k.writeErr = map[ioctl.IOC]error{ioctl.IOCRecvNew: &ioctl.Error{Ioc: ioctl.IOCRecvNew, Name: "tank/new@s", Errno: syscall.EEXIST}}
	// A real stream has records and an END record after the header's payload.
	in := pkgStream(t, 0, nil)
	in = append(in[:len(in)-ioctl.ReplayRecordSize], append(zrec(binary.NativeEndian, drrEnd), in[len(in)-ioctl.ReplayRecordSize:]...)...)
	got, err := c.Receive(context.Background(), bytes.NewReader(in), "tank/new", &ReceiveOptions{NoMount: true, Force: true})
	if err != nil {
		t.Fatalf("%v (%+v)", err, got)
	}
	if len(got) != 1 || !got[0].Skipped {
		t.Errorf("received = %+v", got)
	}
	if len(k.sets) != 0 || len(writesOf(k, ioctl.IOCHold)) != 0 {
		t.Error("properties or holds were applied to a snapshot that was skipped")
	}
}

func TestReceivePackageDryRunAndLayout(t *testing.T) {
	k := newFake(t)
	c := newClient(k)
	in := pkgStream(t, 0, nil)
	// The dry run reads the header and walks the stream; nothing reaches the kernel.
	in = append(in[:len(in)-ioctl.ReplayRecordSize], append(zrec(binary.NativeEndian, drrEnd), in[len(in)-ioctl.ReplayRecordSize:]...)...)
	got, err := c.Receive(context.Background(), bytes.NewReader(in), "tank/new", &ReceiveOptions{DryRun: true})
	if err != nil || len(got) != 1 || !got[0].DryRun || got[0].Snapshot != "tank/new@s" {
		t.Fatalf("%+v %v", got, err)
	}
	if len(k.writes) != 0 {
		t.Errorf("the kernel was called: %+v", k.writes)
	}
	// A snapshot target for a stream of several datasets is refused up front.
	if _, err := c.Receive(context.Background(), bytes.NewReader(pkgStream(t, 0, nil)), "tank/new@x", nil); err == nil {
		t.Error("a snapshot name was accepted for a replication stream")
	}
	// A stream that stops before the closing record is not a complete package.
	cut := pkgStream(t, 0, nil)
	cut = cut[:len(cut)-ioctl.ReplayRecordSize]
	if _, err := c.Receive(context.Background(), bytes.NewReader(cut), "tank/other", &ReceiveOptions{NoMount: true}); err == nil {
		t.Error("a package without its closing record was accepted")
	}
}
