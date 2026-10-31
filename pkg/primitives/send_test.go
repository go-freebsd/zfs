package primitives

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/go-freebsd/nvlist/pkg/nvlist"
	"github.com/go-freebsd/zfs/pkg/ioctl"
)

// header builds a stream's first record, in the host byte order.
func header(toname string, from uint64, features StreamFeature, payload uint32, hdrType uint64) []byte {
	return headerOrder(binary.NativeEndian, toname, from, features, payload, hdrType)
}

func headerOrder(bo binary.ByteOrder, toname string, from uint64, features StreamFeature, payload uint32, hdrType uint64) []byte {
	rec := make([]byte, ioctl.ReplayRecordSize)
	bo.PutUint32(rec[4:], payload)
	bo.PutUint64(rec[8:], streamMagic)
	bo.PutUint64(rec[16:], uint64(features)<<2|hdrType)
	bo.PutUint64(rec[24:], 1700000000)
	bo.PutUint32(rec[32:], 2)
	bo.PutUint64(rec[40:], 0xabcdef)
	bo.PutUint64(rec[48:], from)
	copy(rec[56:], toname)
	return rec
}

func stream(toname string, body string) []byte {
	return append(header(toname, 0, FeatureLZ4|FeatureLargeBlocks, uint32(len(body)), hdrSubstream), body...)
}

func TestParseStreamHeader(t *testing.T) {
	h, err := ParseStreamHeader(header("tank/fs@snap", 77, FeatureRaw|FeatureCompressed|FeatureResuming, 12, hdrSubstream))
	if err != nil {
		t.Fatal(err)
	}
	if h.ToName != "tank/fs@snap" || h.Dataset() != "tank/fs" || h.Snapshot() != "snap" || h.FromGUID != 77 || h.ToGUID != 0xabcdef ||
		!h.Incremental() || !h.Raw() || !h.Resuming || h.Compound || h.PayloadLen != 12 || h.Created != 1700000000 || h.Type != Filesystem {
		t.Errorf("header = %+v", h)
	}
	if got := h.Features.String(); got != "compressed,raw,resuming" && got != "resuming,compressed,raw" {
		t.Logf("features = %s", got)
	}
	for _, want := range []string{"raw", "compressed", "resuming"} {
		if !strings.Contains(h.Features.String(), want) {
			t.Errorf("features %q lack %s", h.Features, want)
		}
	}
	if !bytes.Equal(h.Bytes(), header("tank/fs@snap", 77, FeatureRaw|FeatureCompressed|FeatureResuming, 12, hdrSubstream)) {
		t.Error("Bytes() does not return the record as read")
	}

	c, err := ParseStreamHeader(header("tank@x", 0, FeatureSASpill, 100, hdrCompound))
	if err != nil || !c.Compound {
		t.Errorf("compound: %+v %v", c, err)
	}

	// A stream written on a machine of the other byte order.
	var other binary.ByteOrder = binary.BigEndian
	if binary.NativeEndian.Uint16([]byte{0, 1}) == 1 { // host is big endian
		other = binary.LittleEndian
	}
	sw, err := ParseStreamHeader(headerOrder(other, "pool/v@s", 5, FeatureEmbedData, 0, hdrSubstream))
	if err != nil || sw.ToName != "pool/v@s" || sw.FromGUID != 5 || sw.Features != FeatureEmbedData || sw.ToGUID != 0xabcdef {
		t.Errorf("swapped: %+v %v", sw, err)
	}

	if _, err := ParseStreamHeader(make([]byte, ioctl.ReplayRecordSize)); !errors.Is(err, ErrNotAStream) {
		t.Errorf("zero record: %v", err)
	}
	if _, err := ParseStreamHeader(make([]byte, 10)); !errors.Is(err, ErrNotAStream) {
		t.Errorf("short record: %v", err)
	}
	bad := header("a@b", 0, 0, 0, hdrSubstream)
	bad[0] = 3 // not DRR_BEGIN
	if _, err := ParseStreamHeader(bad); !errors.Is(err, ErrNotAStream) {
		t.Errorf("not a begin record: %v", err)
	}
	if _, err := ReadStreamHeader(strings.NewReader("short")); !errors.Is(err, ErrNotAStream) {
		t.Errorf("truncated: %v", err)
	}
	if _, err := ReadStreamHeader(strings.NewReader("")); err != io.EOF {
		t.Errorf("empty: %v", err)
	}
}

func makeToken(t *testing.T, l *nvlist.List, mutate func(sum *uint64, plen *uint64, version *string, payload *string)) string {
	t.Helper()
	packed, err := nvlist.Pack(l)
	if err != nil {
		t.Fatal(err)
	}
	var zb bytes.Buffer
	zw := zlib.NewWriter(&zb)
	zw.Write(packed)
	zw.Close()
	sum, plen, version, payload := fletcher4Word0(zb.Bytes()), uint64(len(packed)), "1", hex.EncodeToString(zb.Bytes())
	if mutate != nil {
		mutate(&sum, &plen, &version, &payload)
	}
	return fmt.Sprintf("%s-%x-%x-%s", version, sum, plen, payload)
}

func tokenList() *nvlist.List {
	l := nvlist.New()
	l.Add("fromguid", uint64(2015))
	l.Add("object", uint64(48))
	l.Add("offset", uint64(131072))
	l.Add("bytes", uint64(262144))
	l.Add("toguid", uint64(2030))
	l.Add("toname", "tank/a@s3")
	l.Add("compressok", nvlist.Flag{})
	l.Add("embedok", nvlist.Flag{})
	return l
}

func TestResumeTokenRoundTrip(t *testing.T) {
	tok, err := ParseResumeToken(makeToken(t, tokenList(), nil))
	if err != nil {
		t.Fatal(err)
	}
	want := ResumeToken{ToName: "tank/a@s3", ToGUID: 2030, FromGUID: 2015, Object: 48, Offset: 131072, Bytes: 262144, Compress: true, EmbedData: true}
	tok.List = nil
	if !reflect.DeepEqual(*tok, want) {
		t.Errorf("token = %+v\nwant    %+v", *tok, want)
	}

	for name, mut := range map[string]func(*uint64, *uint64, *string, *string){
		"checksum": func(s *uint64, _ *uint64, _ *string, _ *string) { *s++ },
		"version":  func(_ *uint64, _ *uint64, v *string, _ *string) { *v = "2" },
		"length":   func(_ *uint64, p *uint64, _ *string, _ *string) { *p += 3 },
		"not hex":  func(_ *uint64, _ *uint64, _ *string, p *string) { *p = "zz" },
	} {
		if _, err := ParseResumeToken(makeToken(t, tokenList(), mut)); err == nil || !strings.Contains(err.Error(), "corrupt") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	for _, bad := range []string{"", "1-2-3", "garbage", "1-xyz-3-00"} {
		if _, err := ParseResumeToken(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	l := nvlist.New()
	l.Add("toname", "x@y")
	if _, err := ParseResumeToken(makeToken(t, l, nil)); err == nil {
		t.Error("a token without object, offset and bytes was accepted")
	}
}

func TestSendToWriterPassesFlagsAndBytes(t *testing.T) {
	k := newFake(t)
	c := newClient(k)
	k.sendData = map[string][]byte{
		"tank/a@s2":           stream("tank/a@s2", "full"),
		"tank/a@s1>tank/a@s2": stream("tank/a@s2", "incr"),
	}
	var buf bytes.Buffer
	if err := c.Send(context.Background(), "tank/a@s2", &buf, &SendOptions{LargeBlocks: true, EmbedData: true, Compress: true, Raw: true}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), k.sendData["tank/a@s2"]) {
		t.Errorf("wrote %d bytes, want the stream", buf.Len())
	}
	w := lastWrite(t, k)
	if w.ioc != ioctl.IOCSendNew || w.name != "tank/a@s2" {
		t.Fatalf("call = %v %s", w.ioc, w.name)
	}
	for _, f := range []string{"largeblockok", "embedok", "compressok", "rawok"} {
		if !w.in.Has(f) {
			t.Errorf("flag %s missing: %s", f, w.in.Format())
		}
	}
	if w.in.Has("savedok") || w.in.Has("fromsnap") || w.in.Has("resume_object") || w.in.Has("redactbook") {
		t.Errorf("unrequested keys sent: %s", w.in.Format())
	}
	if fd, ok := w.in.Int32("fd"); !ok || fd <= 2 {
		t.Errorf("fd = %v", fd)
	}

	buf.Reset()
	if err := c.Send(context.Background(), "tank/a@s2", &buf, &SendOptions{From: "@s1", ResumeObject: 9, ResumeOffset: 4096, RedactBookmark: "tank/a#rb"}); err != nil {
		t.Fatal(err)
	}
	w = lastWrite(t, k)
	if from, _ := w.in.String("fromsnap"); from != "tank/a@s1" {
		t.Errorf("fromsnap = %q (a short name must be expanded)", from)
	}
	if o, _ := w.in.Uint64("resume_object"); o != 9 {
		t.Errorf("resume_object = %d", o)
	}
	if rb, _ := w.in.String("redactbook"); rb != "rb" {
		t.Errorf("redactbook = %q", rb)
	}
	if buf.String() != string(k.sendData["tank/a@s1>tank/a@s2"]) {
		t.Error("incremental stream not written")
	}
	if err := c.Send(context.Background(), "tank/a@s2", &buf, &SendOptions{From: "s1"}); err == nil {
		t.Error("a from without @ or # was accepted")
	}
}

func TestSendToFileUsesTheDescriptorDirectly(t *testing.T) {
	k := newFake(t)
	c := newClient(k)
	k.sendData = map[string][]byte{"tank/a@s1": stream("tank/a@s1", "to a file")}
	f, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := c.Send(context.Background(), "tank/a@s1", f, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(f.Name())
	if !bytes.Equal(got, k.sendData["tank/a@s1"]) {
		t.Errorf("file holds %d bytes", len(got))
	}
}

type failWriter struct{ after int }

func (w *failWriter) Write(p []byte) (int, error) {
	if w.after -= len(p); w.after < 0 {
		return 0, errors.New("disk full")
	}
	return len(p), nil
}

func TestSendStopsWhenTheWriterFails(t *testing.T) {
	k := newFake(t)
	c := newClient(k)
	k.sendData = map[string][]byte{"tank/a@s1": bytes.Repeat([]byte("x"), 4<<20)} // more than a pipe holds
	err := c.Send(context.Background(), "tank/a@s1", &failWriter{after: 1000}, nil)
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err = %v, want the writer's error", err)
	}
}

func TestSendCancelledByContext(t *testing.T) {
	k := newFake(t)
	c := newClient(k)
	k.sendData = map[string][]byte{"tank/a@s1": bytes.Repeat([]byte("x"), 4<<20)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pr, pw := io.Pipe() // a reader that never reads: the send would block forever
	defer pr.Close()
	go func() { cancel() }()
	err := c.Send(ctx, "tank/a@s1", pw, nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if err := c.Send(ctx, "tank/a@s1", io.Discard, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("send with a cancelled context: %v", err)
	}
}

func TestSendIntermediatesSendsEachStep(t *testing.T) {
	k := newFake(t)
	c := newClient(k)
	k.sendData = map[string][]byte{
		"tank/a@s1>tank/a@s2": []byte("[1>2]"),
		"tank/a@s2>tank/a@s3": []byte("[2>3]"),
	}
	var buf bytes.Buffer
	if err := c.Send(context.Background(), "tank/a@s3", &buf, &SendOptions{From: "@s1", Intermediates: true, Compress: true}); err != nil {
		t.Fatal(err)
	}
	// zfs send -I writes a replication stream: the compound header (no
	// payload for -I alone), the streams of the steps, and the empty END.
	rd := bytes.NewReader(buf.Bytes())
	h, end, err := readHeaderOrEnd(rd)
	if err != nil || end || !h.Compound || h.ToName != "tank/a@s3" || h.PayloadLen != 0 {
		t.Fatalf("header %+v end=%v err=%v", h, end, err)
	}
	if _, err := readPackage(rd, h); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, rd.Len())
	rd.Read(body)
	if len(body) != len("[1>2][2>3]")+ioctl.ReplayRecordSize || string(body[:10]) != "[1>2][2>3]" ||
		binary.NativeEndian.Uint32(body[10:]) != drrEnd || !allZero(body[14:]) {
		t.Errorf("after the header: %q", body)
	}
	var froms []string
	for _, w := range k.writes {
		if w.ioc == ioctl.IOCSendNew {
			f, _ := w.in.String("fromsnap")
			froms = append(froms, f+"->"+w.name)
			if !w.in.Has("compressok") {
				t.Error("flags not carried to every step")
			}
		}
	}
	if !reflect.DeepEqual(froms, []string{"tank/a@s1->tank/a@s2", "tank/a@s2->tank/a@s3"}) {
		t.Errorf("steps = %v", froms)
	}
	for _, o := range []*SendOptions{
		{From: "@s3", Intermediates: true}, {From: "@nope", Intermediates: true}, {From: "#bm1", Intermediates: true}, {Intermediates: true},
	} {
		if err := c.Send(context.Background(), "tank/a@s3", io.Discard, o); err == nil {
			t.Errorf("%+v accepted", o)
		}
	}
}

func TestSendSizeProgressRedact(t *testing.T) {
	k := newFake(t)
	c := newClient(k)
	out := nvlist.New()
	out.Add("space", uint64(123456))
	k.writeOut = map[ioctl.IOC]*nvlist.List{ioctl.IOCSendSpace: out}
	n, err := c.SendSize("tank/a@s2", &SendOptions{From: "@s1", Compress: true, Saved: true, ResumeObject: 3, ResumeOffset: 4, ResumeBytes: 100})
	if err != nil || n != 123456 {
		t.Fatalf("SendSize = %d, %v", n, err)
	}
	in := lastWrite(t, k).in
	if from, _ := in.String("from"); from != "tank/a@s1" || !in.Has("compressok") || in.Has("savedok") || in.Has("fromsnap") {
		t.Errorf("space request = %s", in.Format())
	}
	if b, _ := in.Uint64("bytes"); b != 100 {
		t.Errorf("bytes = %d", b)
	}

	k.progress = 8192
	p, err := c.SendProgress("tank/a@s2", 7)
	if err != nil || p.Bytes != 8192 || p.Blocks != 16 {
		t.Errorf("progress = %+v %v", p, err)
	}
	if w := lastWrite(t, k); w.cookie != 7 {
		t.Errorf("fd sent as %d", w.cookie)
	}

	if err := c.Redact("tank/a@s2", "#red", []string{"tank/a@s1"}); err != nil {
		t.Fatal(err)
	}
	r := lastWrite(t, k)
	if r.ioc != ioctl.IOCRedact || r.name != "tank/a@s2" {
		t.Fatalf("redact call = %+v", r)
	}
	if b, _ := r.in.String("bookname"); b != "red" {
		t.Errorf("bookname = %q", b)
	}
	if sn, _ := r.in.List("snapnv"); sn == nil || !sn.Has("tank/a@s1") {
		t.Errorf("snapnv = %s", r.in.Format())
	}
}

func TestResumeSendFindsSnapshotsByGUID(t *testing.T) {
	k := newFake(t)
	c := newClient(k)
	// tank/a@s3 has guid 2030 and tank/a@s1 guid 2015 in the fake.
	k.sendData = map[string][]byte{"tank/a@s1>tank/a@s3": []byte("resumed")}
	var buf bytes.Buffer
	if err := c.ResumeSend(context.Background(), &buf, makeToken(t, tokenList(), nil), nil); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "resumed" {
		t.Errorf("stream = %q", buf.String())
	}
	w := lastWrite(t, k)
	if w.name != "tank/a@s3" {
		t.Errorf("sent %s", w.name)
	}
	if from, _ := w.in.String("fromsnap"); from != "tank/a@s1" {
		t.Errorf("from = %q", from)
	}
	if o, _ := w.in.Uint64("resume_object"); o != 48 {
		t.Errorf("resume_object = %d", o)
	}
	if off, _ := w.in.Uint64("resume_offset"); off != 131072 {
		t.Errorf("resume_offset = %d", off)
	}
	if !w.in.Has("compressok") || !w.in.Has("embedok") || w.in.Has("rawok") {
		t.Errorf("flags from the token: %s", w.in.Format())
	}

	l := tokenList()
	l.Add("toguid", uint64(99999))
	err := c.ResumeSend(context.Background(), &buf, makeToken(t, l, nil), nil)
	if !errors.Is(err, ErrGUIDNotFound) {
		t.Errorf("a token whose snapshot is gone: %v", err)
	}
	if err := c.ResumeSend(context.Background(), &buf, "1-0-0-", nil); err == nil {
		t.Error("garbage token accepted")
	}
}

func recvInput(streams ...[]byte) io.Reader { return bytes.NewReader(bytes.Join(streams, nil)) }

func TestReceiveReadsSeveralStreams(t *testing.T) {
	for _, mode := range []string{"reader", "file"} {
		t.Run(mode, func(t *testing.T) {
			k := newFake(t)
			c := newClient(k)
			in := bytes.Join([][]byte{stream("pool/src@one", "hello"), stream("pool/src@two", "second stream is longer")}, nil)
			var r io.Reader = bytes.NewReader(in)
			if mode == "file" {
				p := filepath.Join(t.TempDir(), "in")
				os.WriteFile(p, in, 0o600)
				f, _ := os.Open(p)
				defer f.Close()
				r = f
			}
			got, err := c.Receive(context.Background(), r, "tank/new", nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 2 || got[0].Snapshot != "tank/new@one" || got[1].Snapshot != "tank/new@two" {
				t.Fatalf("received = %+v", got)
			}
			if string(k.received[0]) != "hello" || string(k.received[1]) != "second stream is longer" {
				t.Errorf("the kernel read %q", k.received)
			}
			if got[0].Bytes != 5+312 || got[0].Header.ToName != "pool/src@one" {
				t.Errorf("result = %+v", got[0])
			}
			w := k.writes[0]
			if w.ioc != ioctl.IOCRecvNew || w.name != "tank" { // tank/new does not exist: its parent is used
				t.Errorf("first call = %v %s", w.ioc, w.name)
			}
			if sn, _ := w.in.String("snapname"); sn != "tank/new@one" {
				t.Errorf("snapname = %q", sn)
			}
			rec, _ := w.in.Get("begin_record")
			if n := len(rec.([]nvlist.Byte)); n != 312 {
				t.Errorf("begin_record is %d bytes", n)
			}
		})
	}
}

func TestReceiveIntoAnExistingDatasetUsesItAsTheFilesystem(t *testing.T) {
	k := newFake(t)
	c := newClient(k)
	if _, err := c.Receive(context.Background(), recvInput(stream("pool/src@x", "")), "tank/a", nil); err != nil {
		t.Fatal(err)
	}
	if w := k.writes[0]; w.name != "tank/a" {
		t.Errorf("fsname = %q, want the existing dataset", w.name)
	}
}

func TestReceiveOptionsReachTheKernel(t *testing.T) {
	k := newFake(t)
	c := newClient(k)
	_, err := c.Receive(context.Background(), recvInput(stream("pool/src@x", "")), "tank/new", &ReceiveOptions{
		Force: true, Resumable: true, Heal: true, Origin: "tank/a@s1",
		Props: map[string]any{"compression": "lz4", "note:from": "backup"},
	})
	if err != nil {
		t.Fatal(err)
	}
	in := k.writes[0].in
	for _, f := range []string{"force", "resumable", "heal"} {
		if !in.Has(f) {
			t.Errorf("flag %s missing", f)
		}
	}
	if o, _ := in.String("origin"); o != "tank/a@s1" {
		t.Errorf("origin = %q", o)
	}
	lp, _ := in.List("localprops")
	if v, _ := lp.Uint64("compression"); v != 15 {
		t.Errorf("localprops = %s", in.Format())
	}
	if in.Has("props") {
		t.Error("received props sent for a plain stream")
	}
	if _, err := c.Receive(context.Background(), recvInput(stream("pool/src@x", "")), "tank/new",
		&ReceiveOptions{Props: map[string]any{"used": 1}}); err == nil {
		t.Error("a read-only property was accepted")
	}
}

func TestReceiveParentsAreCreated(t *testing.T) {
	k := newFake(t)
	c := newClient(k)
	if _, err := c.Receive(context.Background(), recvInput(stream("pool/src@x", "")), "tank/a/deep/er/leaf", &ReceiveOptions{Parents: true}); err != nil {
		t.Fatal(err)
	}
	var created []string
	for _, w := range k.writes {
		if w.ioc == ioctl.IOCCreate {
			created = append(created, w.name)
		}
	}
	if !reflect.DeepEqual(created, []string{"tank/a/deep", "tank/a/deep/er"}) {
		t.Errorf("created %v", created)
	}
}

func TestReceiveNames(t *testing.T) {
	h := &StreamHeader{ToName: "pool/a/b@snap"}
	root := &StreamHeader{ToName: "pool@snap"}
	for _, c := range []struct {
		h      *StreamHeader
		target string
		layout Layout
		want   string
	}{
		{h, "tank/bk", LayoutExact, "tank/bk@snap"},
		{h, "tank/bk@mine", LayoutExact, "tank/bk@mine"},
		{h, "tank/bk", LayoutDiscardPool, "tank/bk/a/b@snap"},
		{h, "tank/bk", LayoutLastElement, "tank/bk/b@snap"},
		{root, "tank/bk", LayoutDiscardPool, "tank/bk@snap"},
		{root, "tank/bk", LayoutLastElement, "tank/bk/pool@snap"},
	} {
		got, err := receiveName(c.target, c.h, c.layout)
		if err != nil || got != c.want {
			t.Errorf("receiveName(%q, %q, %d) = %q, %v; want %q", c.target, c.h.ToName, c.layout, got, err, c.want)
		}
	}
	if _, err := receiveName("tank/bk@x", h, LayoutDiscardPool); err == nil {
		t.Error("a snapshot target with a layout was accepted")
	}
	if _, err := receiveName("tank/bk", &StreamHeader{ToName: "pool/a"}, LayoutExact); !errors.Is(err, ErrNotAStream) {
		t.Errorf("a stream without a snapshot name: %v", err)
	}
}

func TestReceiveRefusesWhatItCannotDo(t *testing.T) {
	k := newFake(t)
	c := newClient(k)
	ctx := context.Background()
	compound := header("pool@x", 0, FeatureSASpill, 20, hdrCompound)
	// A replication header that is cut short or fails its checksum is an error
	// and reaches no dataset.
	if _, err := c.Receive(ctx, bytes.NewReader(compound), "tank/new", nil); !errors.Is(err, ioctl.ErrStreamTruncated) {
		t.Errorf("compound: %v", err)
	}
	if len(k.writes) != 0 {
		t.Errorf("the kernel was called: %v", k.writes)
	}
	if _, err := c.Receive(ctx, strings.NewReader(strings.Repeat("x", 400)), "tank/new", nil); !errors.Is(err, ErrNotAStream) {
		t.Errorf("garbage: %v", err)
	}
	if _, err := c.Receive(ctx, strings.NewReader(""), "tank/new", nil); !errors.Is(err, ErrNotAStream) {
		t.Errorf("empty: %v", err)
	}
	two := bytes.Join([][]byte{stream("pool/s@a", ""), stream("pool/s@b", "")}, nil)
	got, err := c.Receive(ctx, bytes.NewReader(two), "tank/new@only", nil)
	if err == nil || len(got) != 1 {
		t.Errorf("two streams into a snapshot target: %d received, %v", len(got), err)
	}
	if len(k.writes) != 1 {
		t.Errorf("%d ioctls for refused input", len(k.writes))
	}
}

func TestReceiveReportsKernelDetail(t *testing.T) {
	k := newFake(t)
	c := newClient(k)
	detail := nvlist.New()
	detail.Add("read_bytes", uint64(1000))
	detail.Add("error_flags", uint64(1))
	errs := nvlist.New()
	errs.Add("compression", int32(syscall.EINVAL))
	detail.Add("errors", errs)
	k.writeErr = map[ioctl.IOC]error{ioctl.IOCRecvNew: &ioctl.Error{Ioc: ioctl.IOCRecvNew, Name: "tank", Errno: syscall.ENODEV, Detail: detail}}
	got, err := c.Receive(context.Background(), recvInput(stream("pool/s@a", "")), "tank/new", nil)
	var re *ReceiveError
	if !errors.As(err, &re) || !errors.Is(err, syscall.ENODEV) {
		t.Fatalf("err = %v", err)
	}
	if re.Bytes != 1000 || re.ErrorFlags != 1 || re.PropErrors["compression"] != syscall.EINVAL || len(got) != 1 {
		t.Errorf("ReceiveError = %+v", re)
	}
}

func TestResumeTokenPropertyAndAbort(t *testing.T) {
	k := newFake(t)
	c := newClient(k)
	if _, ok, err := c.ResumeToken("tank/a"); ok || err != nil {
		t.Errorf("no token: %v %v", ok, err)
	}
	k.ds["tank/a"].props["receive_resume_token"] = Property{Value: "1-abc-1-00"}
	if tok, ok, err := c.ResumeToken("tank/a"); !ok || err != nil || tok != "1-abc-1-00" {
		t.Errorf("token = %q %v %v", tok, ok, err)
	}
	k.ds["tank/a"].props["receive_resume_token"] = Property{Value: "-"}
	if _, ok, _ := c.ResumeToken("tank/a"); ok {
		t.Error(`"-" is no token`)
	}
	// Without a %recv child and without an inconsistent dataset there is nothing to abort.
	if err := c.AbortResume("tank/a"); !errors.Is(err, ErrNoResumeState) {
		t.Errorf("abort without state: %v", err)
	}
	k.add(&fakeDS{name: "tank/a/%recv", osType: 2})
	if err := c.AbortResume("tank/a"); err != nil {
		t.Fatal(err)
	}
	if w := lastWrite(t, k); w.ioc != ioctl.IOCDestroy || w.name != "tank/a/%recv" {
		t.Errorf("abort = %+v", w)
	}
}

func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}
