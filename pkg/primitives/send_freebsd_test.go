//go:build freebsd

package primitives

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Send and receive against a real pool. zfs(8) is the oracle in both
// directions: streams made here must be accepted by "zfs receive", streams
// made by "zfs send" must be accepted here, and the results must be the
// same snapshots (same GUID) with the same files.

// source makes a mounted filesystem with files and returns its mountpoint.
func source(t *testing.T, c *Client, pool, name string, props map[string]any, files map[string]string) (ds, dir string) {
	t.Helper()
	ds = wds(t, pool, name)
	dir = tdir(t)
	p := map[string]any{"mountpoint": dir}
	for k, v := range props {
		p[k] = v
	}
	if err := c.Create(ds, &CreateOptions{Props: p}); err != nil {
		t.Fatal(err)
	}
	cli(t, "zfs", "mount", ds)
	write(t, dir, files)
	return ds, dir
}

func write(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func sameDir(t *testing.T, a, b string) {
	t.Helper()
	read := func(dir string) map[string]string {
		m := map[string]string{}
		filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() {
				body, _ := os.ReadFile(p)
				rel, _ := filepath.Rel(dir, p)
				m[rel] = string(body)
			}
			return nil
		})
		return m
	}
	ma, mb := read(a), read(b)
	if len(ma) == 0 {
		t.Fatalf("%s is empty: the comparison would prove nothing", a)
	}
	if len(ma) != len(mb) {
		t.Errorf("%s has %d files, %s has %d", a, len(ma), b, len(mb))
	}
	for k, v := range ma {
		if mb[k] != v {
			t.Errorf("file %s differs", k)
		}
	}
}

func snap(t *testing.T, c *Client, name string) {
	t.Helper()
	if err := c.Snapshot([]string{name}, nil); err != nil {
		t.Fatal(err)
	}
}

func guid(t *testing.T, name string) string { return zget(t, name, "guid")["guid"] }

// ours sends with this library into a buffer.
func ours(t *testing.T, c *Client, snapshot string, o *SendOptions) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := c.Send(context.Background(), snapshot, &buf, o); err != nil {
		t.Fatalf("Send(%s): %v", snapshot, err)
	}
	return buf.Bytes()
}

// theirs is "zfs send" with the given arguments.
func theirs(t *testing.T, args ...string) []byte {
	t.Helper()
	out, err := exec.Command("zfs", append([]string{"send"}, args...)...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Fatalf("zfs send %v: %v\n%s", args, err, ee.Stderr)
		}
		t.Fatal(err)
	}
	return out
}

// zreceive feeds a stream to "zfs receive".
func zreceive(t *testing.T, stream []byte, args ...string) {
	t.Helper()
	cmd := exec.Command("zfs", append([]string{"receive"}, args...)...)
	cmd.Stdin = bytes.NewReader(stream)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("zfs receive %v: %v\n%s", args, err, out)
	}
}

func TestSendReceiveFullAndIncremental(t *testing.T) {
	c, pool := testClient(t)
	src, dir := source(t, c, pool, "sr_src", nil, map[string]string{"one": "first file", "two": strings.Repeat("x", 200000)})
	snap(t, c, src+"@a")
	write(t, dir, map[string]string{"two": "changed", "three": "added"})
	snap(t, c, src+"@b")
	write(t, dir, map[string]string{"four": "later"})
	snap(t, c, src+"@c")

	// A stream made here is the stream zfs makes, byte for byte.
	full := ours(t, c, src+"@a", nil)
	if want := theirs(t, src+"@a"); !bytes.Equal(full, want) {
		t.Errorf("full stream: %d bytes here, %d from zfs send", len(full), len(want))
	}
	incr := ours(t, c, src+"@b", &SendOptions{From: "@a"})
	if want := theirs(t, "-i", src+"@a", src+"@b"); !bytes.Equal(incr, want) {
		t.Errorf("incremental stream: %d bytes here, %d from zfs send -i", len(incr), len(want))
	}
	h, err := ParseStreamHeader(full)
	if err != nil || h.ToName != src+"@a" || h.Incremental() || h.Compound {
		t.Errorf("full header: %+v %v", h, err)
	}
	h, _ = ParseStreamHeader(incr)
	if !h.Incremental() || h.FromGUID == 0 {
		t.Errorf("incremental header: %+v", h)
	}

	// Received here from our own stream, then the increment on top.
	dst := wds(t, pool, "sr_dst")
	dstDir := tdir(t)
	got, err := c.Receive(context.Background(), bytes.NewReader(full), dst, &ReceiveOptions{Props: map[string]any{"mountpoint": dstDir}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Snapshot != dst+"@a" || got[0].Bytes == 0 {
		t.Errorf("received = %+v", got)
	}
	if guid(t, dst+"@a") != guid(t, src+"@a") {
		t.Errorf("guid of the received snapshot differs")
	}
	if _, err := c.Receive(context.Background(), bytes.NewReader(incr), dst, nil); err != nil {
		t.Fatalf("incremental: %v", err)
	}
	if guid(t, dst+"@b") != guid(t, src+"@b") {
		t.Errorf("guid of the incremental snapshot differs")
	}
	exec.Command("zfs", "mount", dst).Run() // already mounted by the mountpoint property
	// @b is the latest here: two = "changed", three exists.
	check := map[string]string{"one": "first file", "two": "changed", "three": "added"}
	for f, want := range check {
		if body, _ := os.ReadFile(filepath.Join(dstDir, f)); string(body) != want {
			t.Errorf("%s = %q, want %q", f, body, want)
		}
	}

	// And the other way round: zfs send into Receive, our stream into zfs receive.
	dst2 := wds(t, pool, "sr_dst2")
	if _, err := c.Receive(context.Background(), bytes.NewReader(theirs(t, src+"@a")), dst2, nil); err != nil {
		t.Fatalf("receiving zfs send output: %v", err)
	}
	if guid(t, dst2+"@a") != guid(t, src+"@a") {
		t.Error("guid differs (zfs send into Receive)")
	}
	dst3 := wds(t, pool, "sr_dst3")
	zreceive(t, full, dst3)
	zreceive(t, incr, dst3)
	if guid(t, dst3+"@b") != guid(t, src+"@b") {
		t.Error("guid differs (Send into zfs receive)")
	}

	// -I: both steps in one stream, received in one call.
	dst4 := wds(t, pool, "sr_dst4")
	// zfs send -I writes a replication stream (a header listing the snapshots
	// in front of the streams); this library writes the plain streams one after
	// the other. Both carry the same snapshots.
	inter := ours(t, c, src+"@c", &SendOptions{From: "@a", Intermediates: true})
	if h, _ := ParseStreamHeader(theirs(t, "-I", src+"@a", src+"@c")); h == nil || !h.Compound {
		t.Errorf("zfs send -I no longer writes a compound stream: %+v", h)
	}
	if _, err := c.Receive(context.Background(), bytes.NewReader(full), dst4, nil); err != nil {
		t.Fatal(err)
	}
	got, err = c.Receive(context.Background(), bytes.NewReader(inter), dst4, nil)
	if err != nil || len(got) != 2 {
		t.Fatalf("receiving -I: %d streams, %v", len(got), err)
	}
	for _, s := range []string{"@b", "@c"} {
		if guid(t, dst4+s) != guid(t, src+s) {
			t.Errorf("guid of %s differs", s)
		}
	}
	// zfs receive takes the consecutive streams too.
	dst5 := wds(t, pool, "sr_dst5")
	zreceive(t, full, dst5)
	cmd := exec.Command("zfs", "receive", dst5)
	cmd.Stdin = bytes.NewReader(inter)
	out, err := cmd.CombinedOutput()
	t.Logf("zfs receive of the consecutive streams: err=%v %s", err, out)
	if err != nil {
		t.Errorf("zfs receive rejects the consecutive streams of Send with Intermediates: %v\n%s", err, out)
	}
}

func TestSendReceiveFlagsAgainstZfs(t *testing.T) {
	c, pool := testClient(t)
	big := bytes.Repeat([]byte("compressible "), 40000)
	src, _ := source(t, c, pool, "fl_src", map[string]any{"recordsize": "1M", "compression": "lz4"}, map[string]string{"data": string(big), "small": "x"})
	snap(t, c, src+"@a")
	for _, tc := range []struct {
		name string
		o    SendOptions
		args []string
	}{
		{"large blocks", SendOptions{LargeBlocks: true}, []string{"-L"}},
		{"embedded", SendOptions{EmbedData: true}, []string{"-e"}},
		{"compressed", SendOptions{Compress: true}, []string{"-c"}},
		{"all three", SendOptions{LargeBlocks: true, EmbedData: true, Compress: true}, []string{"-L", "-e", "-c"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := ours(t, c, src+"@a", &tc.o)
			if want := theirs(t, append(tc.args, src+"@a")...); !bytes.Equal(stream, want) {
				t.Errorf("%d bytes here, %d from zfs send %v", len(stream), len(want), tc.args)
			}
			h, _ := ParseStreamHeader(stream)
			if tc.o.LargeBlocks != (h.Features&FeatureLargeBlocks != 0) || tc.o.Compress != (h.Features&FeatureCompressed != 0) || tc.o.EmbedData != (h.Features&FeatureEmbedData != 0) {
				t.Errorf("features of the stream: %s", h.Features)
			}
			dst := wds(t, pool, "fl_dst_"+strings.ReplaceAll(tc.name, " ", ""))
			if _, err := c.Receive(context.Background(), bytes.NewReader(stream), dst, nil); err != nil {
				t.Fatal(err)
			}
			if guid(t, dst+"@a") != guid(t, src+"@a") {
				t.Error("guid differs")
			}
		})
	}
	// A compressed stream is smaller than the plain one for compressible data.
	// (With large blocks on both: a 1M block can only be sent as it is on disk if the stream may carry it.)
	plain, comp := ours(t, c, src+"@a", &SendOptions{LargeBlocks: true}), ours(t, c, src+"@a", &SendOptions{LargeBlocks: true, Compress: true})
	if len(comp) >= len(plain) {
		t.Errorf("compressed stream %d bytes, plain %d", len(comp), len(plain))
	}
}

func TestSendSizeMatchesZfs(t *testing.T) {
	c, pool := testClient(t)
	src, dir := source(t, c, pool, "sz_src", map[string]any{"compression": "lz4"}, map[string]string{"data": strings.Repeat("abc", 300000)})
	snap(t, c, src+"@a")
	write(t, dir, map[string]string{"more": strings.Repeat("def", 100000)})
	snap(t, c, src+"@b")

	oracle := func(args ...string) uint64 {
		out := cli(t, "zfs", append([]string{"send", "-nP"}, args...)...)
		for _, l := range strings.Split(out, "\n") {
			if f := strings.Fields(l); len(f) == 2 && f[0] == "size" {
				n, _ := strconv.ParseUint(f[1], 10, 64)
				return n
			}
		}
		t.Fatalf("no size in %q", out)
		return 0
	}
	compared := 0
	for _, tc := range []struct {
		name string
		snap string
		o    *SendOptions
		args []string
	}{
		{"full", src + "@a", nil, []string{src + "@a"}},
		{"incremental", src + "@b", &SendOptions{From: "@a"}, []string{"-i", src + "@a", src + "@b"}},
		{"compressed", src + "@a", &SendOptions{Compress: true}, []string{"-c", src + "@a"}},
		{"second snapshot, full", src + "@b", nil, []string{src + "@b"}},
	} {
		got, err := c.SendSize(tc.snap, tc.o)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if want := oracle(tc.args...); got != want || got == 0 {
			t.Errorf("%s: SendSize = %d, zfs send -nP = %d", tc.name, got, want)
		}
		compared++
		// The estimate is the right order of magnitude for the stream itself.
		var actual int
		if tc.name != "" {
			actual = len(ours(t, c, tc.snap, tc.o))
			if r := float64(got) / float64(actual); r < 0.2 || r > 5 {
				t.Errorf("%s: estimate %d, stream %d", tc.name, got, actual)
			}
		}
	}
	if compared != 4 {
		t.Errorf("compared %d estimates", compared)
	}
	if _, err := c.SendSize(src+"@nope", nil); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("size of a missing snapshot: %v", err)
	}
}

func TestSendFromBookmark(t *testing.T) {
	c, pool := testClient(t)
	src, dir := source(t, c, pool, "bm_src", nil, map[string]string{"a": "1"})
	snap(t, c, src+"@a")
	if err := c.CreateBookmark(src+"#mark", src+"@a"); err != nil {
		t.Fatal(err)
	}
	write(t, dir, map[string]string{"b": "2"})
	snap(t, c, src+"@b")
	if err := c.Destroy(src+"@a", nil); err != nil { // only the bookmark is left
		t.Fatal(err)
	}
	stream := ours(t, c, src+"@b", &SendOptions{From: "#mark"})
	if want := theirs(t, "-i", src+"#mark", src+"@b"); !bytes.Equal(stream, want) {
		t.Errorf("%d bytes here, %d from zfs send -i #mark", len(stream), len(want))
	}
	if h, _ := ParseStreamHeader(stream); !h.Incremental() {
		t.Error("not incremental")
	}
}

func TestSendProgressReportsBytes(t *testing.T) {
	c, pool := testClient(t)
	data := make([]byte, 6<<20)
	rand.Read(data)
	src, _ := source(t, c, pool, "pg_src", map[string]any{"compression": "off"}, map[string]string{"blob": string(data)})
	snap(t, c, src+"@a")

	var mu sync.Mutex
	var seen []SendProgress
	slow := &slowWriter{delay: 20 * time.Millisecond}
	err := c.Send(context.Background(), src+"@a", slow, &SendOptions{
		ProgressInterval: 10 * time.Millisecond,
		Progress:         func(p SendProgress) { mu.Lock(); seen = append(seen, p); mu.Unlock() },
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatalf("no progress report during a send of %d bytes in %d writes", slow.n, slow.writes)
	}
	var last uint64
	for _, p := range seen {
		if p.Bytes < last {
			t.Errorf("progress went backwards: %d after %d", p.Bytes, last)
		}
		last = p.Bytes
	}
	if last == 0 || last > uint64(slow.n) {
		t.Errorf("last progress %d, stream %d", last, slow.n)
	}
	t.Logf("%d reports, last %d of %d bytes", len(seen), last, slow.n)
}

type slowWriter struct {
	delay  time.Duration
	n      int
	writes int
}

func (w *slowWriter) Write(p []byte) (int, error) {
	time.Sleep(w.delay)
	w.n += len(p)
	w.writes++
	return len(p), nil
}

func TestRawSendOfAnEncryptedDataset(t *testing.T) {
	c, pool := testClient(t)
	const pass = "correct horse battery staple"
	src := wds(t, pool, "raw_src")
	dir := tdir(t)
	if err := c.Create(src, &CreateOptions{Key: PassphraseKey(pass), Props: map[string]any{"mountpoint": dir}}); err != nil {
		t.Fatal(err)
	}
	cli(t, "zfs", "mount", src)
	write(t, dir, map[string]string{"secret": "the plaintext"})
	snap(t, c, src+"@a")

	stream := ours(t, c, src+"@a", &SendOptions{Raw: true})
	if want := theirs(t, "-w", src+"@a"); !bytes.Equal(stream, want) {
		t.Errorf("raw stream: %d bytes here, %d from zfs send -w", len(stream), len(want))
	}
	if bytes.Contains(stream, []byte("the plaintext")) {
		t.Error("the raw stream contains the plaintext")
	}
	h, _ := ParseStreamHeader(stream)
	if !h.Raw() {
		t.Errorf("features = %s", h.Features)
	}

	dst := wds(t, pool, "raw_dst")
	if _, err := c.Receive(context.Background(), bytes.NewReader(stream), dst, nil); err != nil {
		t.Fatal(err)
	}
	check := zget(t, dst, "encryption", "keyformat", "keystatus", "encryptionroot")
	if check["encryption"] != "aes-256-gcm" || check["keyformat"] != "passphrase" || check["keystatus"] != "unavailable" || check["encryptionroot"] != dst {
		t.Errorf("received raw dataset: %v", check)
	}
	if guid(t, dst+"@a") != guid(t, src+"@a") {
		t.Error("guid differs")
	}
	// The same passphrase opens it: the key came with the stream.
	if err := c.LoadKey(dst, PassphraseKey(pass)); err != nil {
		t.Fatalf("LoadKey on the received dataset: %v", err)
	}
	// A non-raw send of the encrypted dataset is plaintext and cannot be sent raw twice.
	plain := ours(t, c, src+"@a", nil)
	if h, _ := ParseStreamHeader(plain); h.Raw() {
		t.Error("a normal send of an encrypted dataset is marked raw")
	}
}

func TestReceiveOptionsOnARealKernel(t *testing.T) {
	c, pool := testClient(t)
	src, dir := source(t, c, pool, "op_src", nil, map[string]string{"f": "v1"})
	snap(t, c, src+"@a")
	write(t, dir, map[string]string{"f": "v2"})
	snap(t, c, src+"@b")
	full := ours(t, c, src+"@a", nil)
	incr := ours(t, c, src+"@b", &SendOptions{From: "@a"})

	// Props are set locally; Force is needed for a modified target.
	dst := wds(t, pool, "op_dst")
	if _, err := c.Receive(context.Background(), bytes.NewReader(full), dst, &ReceiveOptions{Props: map[string]any{"compression": "zle", "note:got": "yes", "mountpoint": "none"}}); err != nil {
		t.Fatal(err)
	}
	check := zget(t, dst, "compression", "note:got", "mountpoint")
	if check["compression"] != "zle" || check["note:got"] != "yes" || check["mountpoint"] != "none" {
		t.Errorf("local properties: %v", check)
	}
	if _, err := c.Receive(context.Background(), bytes.NewReader(full), dst, nil); !errors.Is(err, fs.ErrExist) {
		t.Errorf("receiving a full stream into an existing dataset: %v", err)
	}
	// Writing to the target makes the increment refuse without Force.
	cli(t, "zfs", "set", "mountpoint="+tdir(t), dst)
	exec.Command("zfs", "mount", dst).Run() // changing the mountpoint remounts
	mp := zget(t, dst, "mountpoint")["mountpoint"]
	os.WriteFile(filepath.Join(mp, "intruder"), []byte("modified"), 0o644)
	if _, err := c.Receive(context.Background(), bytes.NewReader(incr), dst, nil); !errors.Is(err, syscall.ENODEV) {
		t.Errorf("incremental into a modified target without Force: %v", err)
	}
	if _, err := c.Receive(context.Background(), bytes.NewReader(incr), dst, &ReceiveOptions{Force: true}); err != nil {
		t.Fatalf("with Force: %v", err)
	}
	if guid(t, dst+"@b") != guid(t, src+"@b") {
		t.Error("guid differs")
	}

	// Layouts, checked against zfs receive -d and -e.
	deep := wds(t, pool, "op_deep")
	if err := c.Create(deep+"/x/y", &CreateOptions{Parents: true, Props: map[string]any{"mountpoint": "none"}}); err != nil {
		t.Fatal(err)
	}
	snap(t, c, deep+"/x/y@s")
	stream := ours(t, c, deep+"/x/y@s", nil)
	ref := wds(t, pool, "op_ref")
	cli(t, "zfs", "create", ref)
	cli(t, "zfs", "create", ref+"/d")
	cli(t, "zfs", "create", ref+"/e")
	zreceive(t, stream, "-d", ref+"/d")
	zreceive(t, stream, "-e", ref+"/e")
	mine := wds(t, pool, "op_mine")
	cli(t, "zfs", "create", mine)
	cli(t, "zfs", "create", mine+"/d")
	cli(t, "zfs", "create", mine+"/e")
	if _, err := c.Receive(context.Background(), bytes.NewReader(stream), mine+"/d", &ReceiveOptions{Layout: LayoutDiscardPool, Parents: true}); err != nil {
		t.Fatalf("LayoutDiscardPool: %v", err)
	}
	if _, err := c.Receive(context.Background(), bytes.NewReader(stream), mine+"/e", &ReceiveOptions{Layout: LayoutLastElement}); err != nil {
		t.Fatalf("LayoutLastElement: %v", err)
	}
	rel := func(root string) []string {
		var out []string
		for _, l := range strings.Split(cli(t, "zfs", "list", "-H", "-r", "-t", "all", "-o", "name", root), "\n") {
			out = append(out, strings.TrimPrefix(l, root))
		}
		return out
	}
	if a, b := strings.Join(rel(ref), "|"), strings.Join(rel(mine), "|"); a != b {
		t.Errorf("layouts: zfs receive -d/-e made\n%s\nhere:\n%s", a, b)
	}
}

func TestReceiveRefusesReplicationStreams(t *testing.T) {
	c, pool := testClient(t)
	src, _ := source(t, c, pool, "rep_src", nil, map[string]string{"a": "1"})
	snap(t, c, src+"@a")
	for _, args := range [][]string{{"-R", src + "@a"}, {"-p", src + "@a"}} {
		stream := theirs(t, args...)
		h, err := ParseStreamHeader(stream)
		if err != nil || !h.Compound {
			t.Fatalf("zfs send %v: header %+v %v", args, h, err)
		}
		dst := wds(t, pool, "rep_dst")
		if _, err := c.Receive(context.Background(), bytes.NewReader(stream), dst, nil); !errors.Is(err, ErrCompoundStream) {
			t.Errorf("zfs send %v: %v", args, err)
		}
		if ok, _ := c.Exists(dst); ok {
			t.Errorf("%v: a dataset was created from a refused stream", args)
		}
	}
}

// An interrupted resumable receive leaves a token; ResumeSend and Receive
// finish the transfer.
func TestResumableReceiveAndResumeSend(t *testing.T) {
	c, pool := testClient(t)
	data := make([]byte, 12<<20)
	rand.Read(data)
	src, _ := source(t, c, pool, "rs_src", map[string]any{"compression": "off", "recordsize": "128k"}, map[string]string{"blob": string(data)})
	snap(t, c, src+"@a")
	stream := ours(t, c, src+"@a", nil)
	if len(stream) < 12<<20 {
		t.Fatalf("stream is only %d bytes", len(stream))
	}

	dst := wds(t, pool, "rs_dst")
	cut := len(stream) / 3
	got, err := c.Receive(context.Background(), bytes.NewReader(stream[:cut]), dst, &ReceiveOptions{Resumable: true})
	if err == nil {
		t.Fatal("a truncated stream was received without error")
	}
	t.Logf("truncated receive: %v (%d streams)", err, len(got))
	token, ok, err := c.ResumeToken(dst)
	if err != nil || !ok {
		t.Fatalf("no resume token after an interrupted resumable receive: %v %v", ok, err)
	}
	tok, err := ParseResumeToken(token)
	if err != nil {
		t.Fatal(err)
	}
	if tok.ToName != src+"@a" || tok.ToGUID == 0 || tok.Bytes == 0 || tok.FromGUID != 0 {
		t.Errorf("token = %+v", tok)
	}
	// zfs agrees about the token's meaning: it resumes to the same bytes.
	want := theirs(t, "-t", token)
	var rest bytes.Buffer
	if err := c.ResumeSend(context.Background(), &rest, token, nil); err != nil {
		t.Fatalf("ResumeSend: %v", err)
	}
	if !bytes.Equal(rest.Bytes(), want) {
		t.Errorf("resumed stream: %d bytes here, %d from zfs send -t", rest.Len(), len(want))
	}
	h, _ := ParseStreamHeader(rest.Bytes())
	if !h.Resuming {
		t.Errorf("the resumed stream does not say so: %s", h.Features)
	}
	if rest.Len() >= len(stream) {
		t.Errorf("the resumed stream (%d) is not shorter than the whole (%d)", rest.Len(), len(stream))
	}

	if _, err := c.Receive(context.Background(), &rest, dst, &ReceiveOptions{Resumable: true}); err != nil {
		t.Fatalf("receiving the rest: %v", err)
	}
	if guid(t, dst+"@a") != guid(t, src+"@a") {
		t.Error("guid differs after resuming")
	}
	if _, ok, _ := c.ResumeToken(dst); ok {
		t.Error("the token survived a completed receive")
	}

	// Abort: a second interrupted receive can be discarded.
	dst2 := wds(t, pool, "rs_dst2")
	c.Receive(context.Background(), bytes.NewReader(stream[:cut]), dst2, &ReceiveOptions{Resumable: true})
	if _, ok, _ := c.ResumeToken(dst2); !ok {
		t.Fatal("no token for the second interrupted receive")
	}
	if err := c.AbortResume(dst2); err != nil {
		t.Fatalf("AbortResume: %v", err)
	}
	if _, ok, _ := c.ResumeToken(dst2); ok {
		t.Error("the token survived AbortResume")
	}

	// A token made by zfs receive works here too.
	dst3 := wds(t, pool, "rs_dst3")
	cmd := exec.Command("zfs", "receive", "-s", dst3)
	cmd.Stdin = bytes.NewReader(stream[:cut])
	cmd.Run() // fails: truncated
	token3, ok, err := c.ResumeToken(dst3)
	if err != nil || !ok {
		t.Fatalf("no token from zfs receive -s: %v %v", ok, err)
	}
	var rest3 bytes.Buffer
	if err := c.ResumeSend(context.Background(), &rest3, token3, nil); err != nil {
		t.Fatal(err)
	}
	zreceive(t, rest3.Bytes(), "-s", dst3)
	if guid(t, dst3+"@a") != guid(t, src+"@a") {
		t.Error("guid differs (resume send into zfs receive)")
	}
}

func TestRedactedSend(t *testing.T) {
	c, pool := testClient(t)
	src, dir := source(t, c, pool, "rd_src", nil, map[string]string{"public": "ok", "secret": "classified"})
	snap(t, c, src+"@a")
	clone := wds(t, pool, "rd_clone")
	cdir := tdir(t)
	if err := c.Clone(clone, src+"@a", &CreateOptions{Props: map[string]any{"mountpoint": cdir}}); err != nil {
		t.Fatal(err)
	}
	cli(t, "zfs", "mount", clone)
	os.WriteFile(filepath.Join(cdir, "secret"), []byte("REDACTED"), 0o644) // the blocks that differ are left out
	snap(t, c, clone+"@r")
	_ = dir

	if err := c.Redact(src+"@a", "#red", []string{clone + "@r"}); err != nil {
		t.Fatalf("Redact: %v", err)
	}
	if got := strings.Fields(cli(t, "zfs", "list", "-H", "-t", "bookmark", "-o", "name", src)); len(got) != 1 || got[0] != src+"#red" {
		t.Errorf("bookmarks after Redact: %v", got)
	}
	stream := ours(t, c, src+"@a", &SendOptions{RedactBookmark: "#red"})
	h, err := ParseStreamHeader(stream)
	if err != nil || h.Features&FeatureRedacted == 0 {
		t.Fatalf("header %+v %v", h, err)
	}
	if bytes.Contains(stream, []byte("classified")) {
		t.Error("the redacted stream contains the redacted data")
	}
	if want := theirs(t, "--redact", "red", src+"@a"); !bytes.Equal(stream, want) {
		t.Errorf("redacted stream: %d bytes here, %d from zfs send --redact", len(stream), len(want))
	}
	dst := wds(t, pool, "rd_dst")
	if _, err := c.Receive(context.Background(), bytes.NewReader(stream), dst, nil); err != nil {
		t.Fatal(err)
	}
	if got := zget(t, dst+"@a", "redact_snaps")["redact_snaps"]; got == "-" || got == "" {
		t.Errorf("the received snapshot is not marked redacted: %q", got)
	}
}

func TestSendWriterFailureAndCancelOnARealKernel(t *testing.T) {
	c, pool := testClient(t)
	data := make([]byte, 8<<20)
	rand.Read(data)
	src, _ := source(t, c, pool, "wf_src", map[string]any{"compression": "off"}, map[string]string{"blob": string(data)})
	snap(t, c, src+"@a")

	start := time.Now()
	err := c.Send(context.Background(), src+"@a", &failAfter{n: 1 << 20}, nil)
	if err == nil || !strings.Contains(err.Error(), "broken sink") {
		t.Errorf("err = %v, want the writer's error", err)
	}
	if time.Since(start) > 20*time.Second {
		t.Error("the send did not stop when the writer failed")
	}

	ctx, cancel := context.WithCancel(context.Background())
	slow := &cancelAfter{cancel: cancel, after: 1 << 20}
	if err := c.Send(ctx, src+"@a", slow, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("cancel: %v", err)
	}
	// And the library still works afterwards.
	if _, err := c.SendSize(src+"@a", nil); err != nil {
		t.Error(err)
	}

	// A reader that fails in the middle of a stream.
	stream := ours(t, c, src+"@a", nil)
	dst := wds(t, pool, "wf_dst")
	_, err = c.Receive(context.Background(), io.MultiReader(bytes.NewReader(stream[:len(stream)/2]), errReader{}), dst, nil)
	if err == nil {
		t.Error("a failing reader was not reported")
	}
	if ok, _ := c.Exists(dst + "@a"); ok {
		t.Error("a snapshot exists from a failed receive")
	}
}

type failAfter struct{ n int }

func (w *failAfter) Write(p []byte) (int, error) {
	if w.n -= len(p); w.n < 0 {
		return 0, errors.New("broken sink")
	}
	return len(p), nil
}

type cancelAfter struct {
	cancel func()
	after  int
}

func (w *cancelAfter) Write(p []byte) (int, error) {
	if w.after -= len(p); w.after < 0 {
		w.cancel()
		time.Sleep(50 * time.Millisecond)
	}
	return len(p), nil
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("network down") }
