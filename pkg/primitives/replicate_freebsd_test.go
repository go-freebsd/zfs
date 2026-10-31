//go:build freebsd

package primitives

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-freebsd/nvlist/pkg/nvlist"
	"github.com/go-freebsd/zfs/pkg/ioctl"
)

// replTree makes src with a child, two snapshots on each and a clone, and returns
// the names.
func replTree(t *testing.T, c *Client, pool, name string) (src string) {
	t.Helper()
	src, _ = source(t, c, pool, name, map[string]any{"compression": "zle"}, map[string]string{"a": "one"})
	child := src + "/kid"
	if err := c.Create(child, &CreateOptions{Props: map[string]any{"mountpoint": tdir(t), "quota": uint64(64 << 20)}}); err != nil {
		t.Fatal(err)
	}
	cli(t, "zfs", "set", "repl:note=hello", src)
	snap(t, c, src+"@a")
	snap(t, c, child+"@a")
	// A clone inside the tree: its stream starts from its origin, which has
	// to be sent first.
	cli(t, "zfs", "clone", "-o", "mountpoint=none", child+"@a", src+"/kid_clone")
	snap(t, c, src+"@b")
	snap(t, c, child+"@b")
	snap(t, c, src+"/kid_clone@b")
	return src
}

// The header of streams made by zfs send is decoded, its checksum verified,
// and its nvlist packs back to the very same bytes: the XDR codec and the
// fletcher4 code agree with libzfs.
func TestPackageHeaderOfRealStreams(t *testing.T) {
	c, pool := testClient(t)
	src := replTree(t, c, pool, "pkg_src")
	cli(t, "zfs", "hold", "keep", src+"@a", src+"/kid@a")
	t.Cleanup(func() { // a held snapshot cannot be destroyed with its dataset
		exec.Command("zfs", "release", "keep", src+"@a", src+"/kid@a").Run()
	})

	for name, args := range map[string][]string{
		"-R":    {"-R", src + "@b"},
		"-R -i": {"-R", "-i", src + "@a", src + "@b"},
		"-p":    {"-p", src + "@b"},
		"-I":    {"-I", "@a", src + "@b"},
		"-h":    {"-h", src + "@b"},
		"-Rh":   {"-R", "-h", src + "@b"},
		"-RI":   {"-R", "-I", "@a", src + "@b"},
	} {
		stream := theirs(t, args...)
		r := bytes.NewReader(stream)
		h, end, err := readHeaderOrEnd(r)
		if err != nil || end || !h.Compound {
			t.Fatalf("%s: header %+v end=%v err=%v", name, h, end, err)
		}
		p, err := readPackage(r, h)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		t.Logf("%s: features %v payload %d bytes, fss %d", name, h.Features, h.PayloadLen, len(p.Filesystems()))
		if h.PayloadLen > 0 {
			payload := stream[ioctl.ReplayRecordSize : ioctl.ReplayRecordSize+int(h.PayloadLen)]
			l, err := nvlist.Unpack(payload)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			again, err := nvlist.PackXDR(l)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if !bytes.Equal(again, payload) {
				t.Errorf("%s: PackXDR(Unpack(header)) differs from what libzfs wrote\n%s", name, l.Format())
			}
			if p.ToSnap != "b" {
				t.Errorf("%s: tosnap %q", name, p.ToSnap)
			}
		} else if name != "-I" {
			t.Errorf("%s: no payload", name)
		}
		// The substreams follow, and the package ends with the empty record.
		var n int
		for {
			sh, end, err := readHeaderOrEnd(r)
			if err != nil {
				t.Fatalf("%s: substream %d: %v", name, n, err)
			}
			if end {
				break
			}
			if sh.Compound {
				t.Fatalf("%s: nested compound stream", name)
			}
			if _, err := skipStream(r, sh, sh.ToName); err != nil {
				t.Fatalf("%s: skipping %s: %v", name, sh.ToName, err)
			}
			n++
		}
		if n == 0 || r.Len() != 0 {
			t.Errorf("%s: %d substreams, %d bytes left", name, n, r.Len())
		}
	}
}

// treeState describes a dataset tree in a form that two copies of the same
// data share: every locally set or received property, the snapshots with
// their GUIDs, the holds, and the clone origins, with the root's name taken
// out.
func treeState(t *testing.T, root string) []string {
	t.Helper()
	norm := func(s string) string { return strings.ReplaceAll(s, root, "ROOT") }
	var out []string
	for _, r := range rows(cli(t, "zfs", "get", "-r", "-Hp", "-o", "name,property,value,source", "all", root)) {
		if len(r) < 4 || (r[3] != "local" && r[3] != "received") {
			continue
		}
		out = append(out, norm(strings.Join(r, "\t")))
	}
	for _, r := range rows(cli(t, "zfs", "list", "-r", "-t", "all", "-Hp", "-o", "name,guid,origin", root)) {
		if !strings.Contains(r[0], "@") && len(r) >= 2 {
			r = append([]string{r[0]}, r[2:]...) // a dataset's own GUID is made at random
		}
		out = append(out, "list\t"+norm(strings.Join(r, "\t")))
	}
	for _, f := range holdRows(root) {
		out = append(out, "hold\t"+norm(f[0])+"\t"+f[1])
	}
	sort.Strings(out)
	return out
}

func sameTrees(t *testing.T, label, a, b string) {
	t.Helper()
	sa, sb := treeState(t, a), treeState(t, b)
	if len(sa) < 4 {
		t.Fatalf("%s: only %d facts about %s: the comparison would prove nothing", label, len(sa), a)
	}
	missing, extra := diff(sa, sb)
	if len(missing) > 0 || len(extra) > 0 {
		t.Errorf("%s: %s and %s differ\nonly in the oracle's copy: %q\nonly in ours: %q", label, a, b, missing, extra)
	}
}

// Streams made by zfs send are received here as zfs receive receives them:
// the same datasets, snapshots (same GUIDs), received properties and holds.
func TestReceiveReplicationStreamsLikeZfs(t *testing.T) {
	c, pool := testClient(t)
	src := replTree(t, c, pool, "rr_src")
	cli(t, "zfs", "hold", "keep", src+"@a", src+"/kid@a")
	t.Cleanup(func() { exec.Command("zfs", "release", "-r", "keep", src+"@a", src+"/kid@a").Run() })

	baseStream := theirs(t, "-R", src+"@a")
	for name, c1 := range map[string]struct {
		args []string
		base bool // an incremental stream: the targets get @a first
	}{
		"-R":  {[]string{"-R", src + "@b"}, false},
		"-p":  {[]string{"-p", src + "@b"}, false},
		"-I":  {[]string{"-I", "@a", src + "@b"}, true},
		"-Rh": {[]string{"-R", "-h", src + "@b"}, false},
		"-RI": {[]string{"-R", "-I", "@a", src + "@b"}, true},
		"-Ri": {[]string{"-R", "-i", "@a", src + "@b"}, true},
	} {
		stream := theirs(t, c1.args...)
		ref := wds(t, pool, "rr_ref")
		mine := wds(t, pool, "rr_mine")
		t.Cleanup(func() { releaseAll(ref); releaseAll(mine) }) // held snapshots keep a dataset alive
		if c1.base {
			zreceive(t, baseStream, "-u", ref)
			if _, err := c.Receive(context.Background(), bytes.NewReader(baseStream), mine, &ReceiveOptions{NoMount: true}); err != nil {
				t.Fatalf("%s: base: %v", name, err)
			}
		}
		zreceive(t, stream, "-u", ref)
		got, err := c.Receive(context.Background(), bytes.NewReader(stream), mine, &ReceiveOptions{NoMount: true})
		if err != nil {
			t.Fatalf("%s: %v (received %d streams)", name, err, len(got))
		}
		if len(got) == 0 {
			t.Fatalf("%s: nothing was received", name)
		}
		sameTrees(t, name, ref, mine)
		if strings.Contains(name, "h") && len(holdRows(mine)) == 0 {
			t.Errorf("%s: the holds were not received", name)
		}
		t.Logf("%s: %d streams, %d facts", name, len(got), len(treeState(t, mine)))
		releaseAll(ref)
		releaseAll(mine)
		cli(t, "zfs", "destroy", "-rR", ref)
		cli(t, "zfs", "destroy", "-rR", mine)
	}
}

// holdRows lists the holds on the snapshots below root as (snapshot, tag).
func holdRows(root string) [][]string {
	snaps, err := exec.Command("zfs", "list", "-rH", "-t", "snapshot", "-o", "name", root).Output()
	if err != nil || len(bytes.TrimSpace(snaps)) == 0 {
		return nil
	}
	out, err := exec.Command("zfs", append([]string{"holds", "-H"}, strings.Fields(string(snaps))...)...).Output()
	if err != nil {
		return nil
	}
	var rows [][]string
	for _, l := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if f := strings.Split(l, "\t"); len(f) >= 2 {
			rows = append(rows, f[:2])
		}
	}
	return rows
}

// releaseAll drops every hold below root.
func releaseAll(root string) {
	for _, f := range holdRows(root) {
		exec.Command("zfs", "release", f[1], f[0]).Run()
	}
}

// canon renders an nvlist independent of the order of its pairs.
func canon(l *nvlist.List) string {
	names := l.Names()
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		v, _ := l.Get(n)
		switch x := v.(type) {
		case *nvlist.List:
			fmt.Fprintf(&b, "%s={%s};", n, canon(x))
		default:
			fmt.Fprintf(&b, "%s=%v;", n, x)
		}
	}
	return b.String()
}

// shape describes a replication stream: its header and the sequence of the
// streams in it.
type shape struct {
	header  string
	subs    []string
	payload int
}

func shapeOf(t *testing.T, stream []byte) shape {
	t.Helper()
	r := bytes.NewReader(stream)
	h, end, err := readHeaderOrEnd(r)
	if err != nil || end || !h.Compound {
		t.Fatalf("header %+v end=%v err=%v", h, end, err)
	}
	p, err := readPackage(r, h)
	if err != nil {
		t.Fatal(err)
	}
	sh := shape{header: fmt.Sprintf("%s features=%v payload=%d from=%q to=%q notrec=%v raw=%v", h.ToName, h.Features, h.PayloadLen, p.FromSnap, p.ToSnap, p.NotRecursive, p.Raw), payload: int(h.PayloadLen)}
	if p.FSS != nil {
		sh.header += " fss={" + canon(p.FSS) + "}"
	}
	for {
		s, end, err := readHeaderOrEnd(r)
		if err != nil {
			t.Fatal(err)
		}
		if end {
			break
		}
		if _, err := skipStream(r, s, s.ToName); err != nil {
			t.Fatal(err)
		}
		sh.subs = append(sh.subs, fmt.Sprintf("%s from=%x to=%x clone=%v", s.ToName, s.FromGUID, s.ToGUID, s.Clone))
	}
	if r.Len() != 0 {
		t.Fatalf("%d bytes after the closing record", r.Len())
	}
	return sh
}

// Replication streams made here have the header zfs send writes, the same
// streams in the same order, and are received by zfs receive as the streams
// of zfs send are.
func TestSendReplicationLikeZfs(t *testing.T) {
	c, pool := testClient(t)
	src := replTree(t, c, pool, "rs_src")
	cli(t, "zfs", "hold", "keep", src+"@a", src+"/kid@a")
	t.Cleanup(func() { releaseAll(src) })

	baseStream := theirs(t, "-R", src+"@a")
	for name, tc := range map[string]struct {
		args []string
		opts SendOptions
		base bool
	}{
		"-R":  {[]string{"-R", src + "@b"}, SendOptions{Replicate: true}, false},
		"-p":  {[]string{"-p", src + "@b"}, SendOptions{Props: true}, false},
		"-I":  {[]string{"-I", "@a", src + "@b"}, SendOptions{From: "@a", Intermediates: true}, true},
		"-Rh": {[]string{"-R", "-h", src + "@b"}, SendOptions{Replicate: true, Holds: true}, false},
		"-RI": {[]string{"-R", "-I", "@a", src + "@b"}, SendOptions{Replicate: true, Intermediates: true, From: "@a"}, true},
		"-Ri": {[]string{"-R", "-i", "@a", src + "@b"}, SendOptions{Replicate: true, From: "@a"}, true},
		"-ph": {[]string{"-p", "-h", src + "@b"}, SendOptions{Props: true, Holds: true}, false},
	} {
		want := theirs(t, tc.args...)
		o := tc.opts
		got := ours(t, c, src+"@b", &o)
		ws, gs := shapeOf(t, want), shapeOf(t, got)
		if ws.header != gs.header {
			t.Errorf("%s: header\nzfs:  %s\nhere: %s", name, ws.header, gs.header)
		}
		if strings.Join(ws.subs, "\n") != strings.Join(gs.subs, "\n") {
			t.Errorf("%s: streams\nzfs:\n%s\nhere:\n%s", name, strings.Join(ws.subs, "\n"), strings.Join(gs.subs, "\n"))
		}
		if len(ws.subs) == 0 {
			t.Fatalf("%s: no streams: the comparison would prove nothing", name)
		}
		if len(want) != len(got) {
			t.Errorf("%s: %d bytes here, zfs sent %d", name, len(got), len(want))
		}

		// zfs receive takes ours as it takes its own.
		ref := wds(t, pool, "rs_ref")
		mine := wds(t, pool, "rs_mine")
		t.Cleanup(func() { releaseAll(ref); releaseAll(mine) })
		if tc.base {
			zreceive(t, baseStream, "-u", ref)
			zreceive(t, baseStream, "-u", mine)
		}
		zreceive(t, want, "-u", ref)
		zreceive(t, got, "-u", mine)
		sameTrees(t, name, ref, mine)
		releaseAll(ref)
		releaseAll(mine)
		cli(t, "zfs", "destroy", "-rR", ref)
		cli(t, "zfs", "destroy", "-rR", mine)
	}
}

// An incremental replication stream changes the target tree to follow the
// source: new datasets arrive, what the sender destroyed is destroyed (with
// Force), renamed snapshots and datasets are renamed, promoted clones are
// promoted. The result is what zfs receive makes of the same stream.
func TestReceiveReconcilesLikeZfs(t *testing.T) {
	c, pool := testClient(t)

	for name, tc := range map[string]struct {
		mutate func(t *testing.T, src string)
		force  bool
	}{
		"new child":                {func(t *testing.T, src string) { cli(t, "zfs", "create", "-o", "mountpoint=none", src+"/fresh") }, false},
		"destroyed snapshot":       {func(t *testing.T, src string) { cli(t, "zfs", "destroy", "-r", src+"@x") }, true},
		"destroyed snapshot, kept": {func(t *testing.T, src string) { cli(t, "zfs", "destroy", "-r", src+"@x") }, false},
		"destroyed dataset":        {func(t *testing.T, src string) { cli(t, "zfs", "destroy", "-r", src+"/gone") }, true},
		"destroyed dataset, kept":  {func(t *testing.T, src string) { cli(t, "zfs", "destroy", "-r", src+"/gone") }, false},
		"renamed snapshot":         {func(t *testing.T, src string) { cli(t, "zfs", "rename", "-r", src+"@x", "@y") }, false},
		"renamed dataset":          {func(t *testing.T, src string) { cli(t, "zfs", "rename", src+"/gone", src+"/moved") }, false},
		"promoted clone":           {func(t *testing.T, src string) { cli(t, "zfs", "promote", src+"/cl") }, false},
	} {
		src, _ := source(t, c, pool, "rc_src", map[string]any{"compression": "zle"}, map[string]string{"a": "one"})
		for _, kid := range []string{"kid", "gone"} {
			if err := c.Create(src+"/"+kid, &CreateOptions{Props: map[string]any{"mountpoint": "none"}}); err != nil {
				t.Fatal(err)
			}
		}
		cli(t, "zfs", "snapshot", "-r", src+"@x")
		cli(t, "zfs", "snapshot", src+"/kid@o") // the clone's origin, so that @x can go
		cli(t, "zfs", "clone", "-o", "mountpoint=none", src+"/kid@o", src+"/cl")
		cli(t, "zfs", "snapshot", "-r", src+"@a")

		ref, mine := wds(t, pool, "rc_ref"), wds(t, pool, "rc_mine")
		base := theirs(t, "-R", src+"@a")
		zreceive(t, base, "-u", ref)
		zreceive(t, base, "-u", mine)
		before := treeState(t, ref)

		tc.mutate(t, src)
		cli(t, "zfs", "snapshot", "-r", src+"@b")
		stream := theirs(t, "-R", "-i", "@a", src+"@b")

		args := []string{"-u"}
		if tc.force {
			args = append(args, "-F")
		}
		zreceive(t, stream, append(args, ref)...)
		got, err := c.Receive(context.Background(), bytes.NewReader(stream), mine, &ReceiveOptions{NoMount: true, Force: tc.force})
		if err != nil {
			t.Errorf("%s: %v (%d streams)", name, err, len(got))
		}
		sameTrees(t, name, ref, mine)
		if after := treeState(t, ref); strings.Join(before, "\n") == strings.Join(after, "\n") {
			t.Errorf("%s: the stream changed nothing: the comparison would prove nothing", name)
		}
		cli(t, "zfs", "destroy", "-rR", ref)
		cli(t, "zfs", "destroy", "-rR", mine)
		cli(t, "zfs", "destroy", "-rR", src)
	}
}

// cryptFacts lists encryptionroot, keyformat and keylocation of a tree.
func cryptFacts(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	for _, r := range rows(cli(t, "zfs", "get", "-r", "-Hp", "-o", "name,property,value", "encryptionroot,keyformat,keylocation,encryption", root)) {
		out = append(out, strings.ReplaceAll(strings.Join(r, "\t"), root, "ROOT"))
	}
	sort.Strings(out)
	return out
}

// A raw replication stream of an encrypted tree: the child that shares its
// parent's key and the child with a key of its own arrive with the same
// encryption roots as zfs receive makes of them.
func TestReceiveRawReplicationLikeZfs(t *testing.T) {
	c, pool := testClient(t)
	src := wds(t, pool, "rw_src")
	if err := c.Create(src, &CreateOptions{Key: PassphraseKey("correct horse battery"), Props: map[string]any{"mountpoint": "none"}}); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(src+"/shared", &CreateOptions{Props: map[string]any{"mountpoint": "none"}}); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(src+"/own", &CreateOptions{Key: PassphraseKey("another passphrase!"), Props: map[string]any{"mountpoint": "none"}}); err != nil {
		t.Fatal(err)
	}
	cli(t, "zfs", "snapshot", "-r", src+"@a")
	if roots := cryptFacts(t, src); len(roots) < 12 {
		t.Fatalf("source facts: %v", roots)
	}

	stream := theirs(t, "-R", "-w", src+"@a")
	ref, mine := wds(t, pool, "rw_ref"), wds(t, pool, "rw_mine")
	zreceive(t, stream, "-u", ref)
	got, err := c.Receive(context.Background(), bytes.NewReader(stream), mine, &ReceiveOptions{NoMount: true})
	if err != nil {
		t.Fatalf("receive: %v (%d streams)", err, len(got))
	}
	sameTrees(t, "raw -R", ref, mine)
	a, b := cryptFacts(t, ref), cryptFacts(t, mine)
	if missing, extra := diff(a, b); len(missing) > 0 || len(extra) > 0 {
		t.Errorf("encryption facts differ\nonly in zfs receive's copy: %q\nonly in ours: %q", missing, extra)
	}
}

// The estimate of a replication stream is the one zfs send -nP prints.
func TestSendReplicationSizeMatchesZfs(t *testing.T) {
	c, pool := testClient(t)
	src := replTree(t, c, pool, "rz_src")
	for name, tc := range map[string]struct {
		args []string
		opts SendOptions
	}{
		"-R":  {[]string{"-R", src + "@b"}, SendOptions{Replicate: true}},
		"-RI": {[]string{"-R", "-I", "@a", src + "@b"}, SendOptions{Replicate: true, Intermediates: true, From: "@a"}},
		"-I":  {[]string{"-I", "@a", src + "@b"}, SendOptions{Intermediates: true, From: "@a"}},
	} {
		out := cli(t, "zfs", append([]string{"send", "-n", "-v", "-P"}, tc.args...)...)
		var want uint64
		for _, l := range lines(out) {
			if f := strings.Fields(l); len(f) == 2 && f[0] == "size" {
				want, _ = strconv.ParseUint(f[1], 10, 64)
			}
		}
		o := tc.opts
		got, err := c.SendSize(src+"@b", &o)
		if err != nil || want == 0 || got != want {
			t.Errorf("%s: SendSize = %d, %v; zfs send -nP says %d", name, got, err, want)
		}
	}
}

// plainTree makes pool/<name> with a child, default mountpoints, a file in each
// and snapshots @a and @b of both. It returns the filesystem.
func plainTree(t *testing.T, pool, name string) string {
	t.Helper()
	src := wds(t, pool, name)
	cli(t, "zfs", "create", "-o", "mountpoint="+tdir(t), src)
	cli(t, "zfs", "create", src+"/kid")
	for _, d := range []string{src, src + "/kid"} {
		mp := cli(t, "zfs", "get", "-H", "-o", "value", "mountpoint", d)
		if err := os.WriteFile(filepath.Join(mp, "f"), []byte("data of "+d), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cli(t, "zfs", "snapshot", "-r", src+"@a")
	cli(t, "zfs", "snapshot", "-r", src+"@b")
	return src
}

// mounted lists "name mountpoint" of the mounted filesystems below root.
func mountedBelow(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	for _, r := range rows(cli(t, "zfs", "list", "-r", "-H", "-t", "filesystem", "-o", "name,mounted,mountpoint", root)) {
		if len(r) == 3 && r[1] == "yes" {
			out = append(out, strings.ReplaceAll(r[0]+" "+r[2], root, "ROOT"))
		}
	}
	return sorted(out)
}

// The real zfs(8) processes are connected to this library with pipes: no
// buffer in between, a reader or writer that is a file and one that is not.
func TestReplicationBetweenZfsProcessesAndThisLibrary(t *testing.T) {
	c, pool := testClient(t)
	src := plainTree(t, pool, "px_src")

	// zfs send -R | Receive, the reader being the pipe's *os.File.
	t.Run("zfs send to Receive", func(t *testing.T) {
		for _, wrap := range []string{"file", "reader"} {
			ref, mine := wds(t, pool, "px_ref"), wds(t, pool, "px_mine")
			send := exec.Command("zfs", "send", "-R", src+"@b")
			out, err := send.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			send.Stderr = &stderr
			if err := send.Start(); err != nil {
				t.Fatal(err)
			}
			var in io.Reader = out
			if wrap == "reader" {
				in = io.MultiReader(out) // not an *os.File: copied through a pipe
			}
			refDir, mineDir := tdir(t), tdir(t)
			got, rerr := c.Receive(context.Background(), in, mine, &ReceiveOptions{Props: map[string]any{"mountpoint": mineDir}})
			if err := send.Wait(); err != nil {
				t.Fatalf("%s: zfs send: %v\n%s", wrap, err, stderr.String())
			}
			if rerr != nil || len(got) != 4 {
				t.Fatalf("%s: Receive: %d streams, %v", wrap, len(got), rerr)
			}
			// The CLI does the same with the same stream.
			zreceive(t, theirs(t, "-R", src+"@b"), "-o", "mountpoint="+refDir, ref)
			noMountpoints := func(root string) string {
				var keep []string
				for _, l := range treeState(t, root) {
					if !strings.Contains(l, "\tmountpoint\t") {
						keep = append(keep, l)
					}
				}
				return strings.Join(keep, "\n")
			}
			if a, b := noMountpoints(ref), noMountpoints(mine); a != b {
				t.Errorf("%s: trees differ\nzfs receive:\n%s\nhere:\n%s", wrap, a, b)
			}
			// Mounted like zfs receive mounts, with the data in place.
			if a, b := mountedBelow(t, ref), mountedBelow(t, mine); strings.ReplaceAll(strings.Join(a, "\n"), refDir, "DIR") != strings.ReplaceAll(strings.Join(b, "\n"), mineDir, "DIR") || len(b) != 2 {
				t.Errorf("%s: mounted\nzfs receive: %q\nhere:        %q", wrap, a, b)
			}
			for _, d := range []string{"", "/kid"} {
				mp := cli(t, "zfs", "get", "-H", "-o", "value", "mountpoint", mine+d)
				if body, err := os.ReadFile(filepath.Join(mp, "f")); err != nil || !strings.HasPrefix(string(body), "data of ") {
					t.Errorf("%s: %s: %q, %v", wrap, mp, body, err)
				}
			}
			cli(t, "zfs", "destroy", "-rR", ref)
			cli(t, "zfs", "destroy", "-rR", mine)
		}
	})

	// Send | zfs receive, the writer being the process's stdin.
	t.Run("Send to zfs receive", func(t *testing.T) {
		for _, wrap := range []string{"file", "writer"} {
			dst := wds(t, pool, "px_dst")
			recv := exec.Command("zfs", "receive", "-u", dst)
			var stderr bytes.Buffer
			recv.Stderr = &stderr
			var w io.Writer
			var closeW func() error
			if wrap == "file" {
				pr, pw, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				recv.Stdin, w, closeW = pr, pw, pw.Close
				defer pr.Close()
			} else {
				in, err := recv.StdinPipe()
				if err != nil {
					t.Fatal(err)
				}
				w, closeW = struct{ io.Writer }{in}, in.Close // not an *os.File
			}
			if err := recv.Start(); err != nil {
				t.Fatal(err)
			}
			serr := c.Send(context.Background(), src+"@b", w, &SendOptions{Replicate: true})
			closeW()
			if err := recv.Wait(); err != nil || serr != nil {
				t.Fatalf("%s: Send: %v; zfs receive: %v\n%s", wrap, serr, err, stderr.String())
			}
			for _, s := range []string{"@a", "@b", "/kid@a", "/kid@b"} {
				if guid(t, dst+s) != guid(t, src+s) {
					t.Errorf("%s: %s differs", wrap, s)
				}
			}
			cli(t, "zfs", "destroy", "-rR", dst)
		}
	})

	// The layouts of -d and -e, and -n, against the CLI.
	t.Run("layouts and dry run", func(t *testing.T) {
		stream := theirs(t, "-R", src+"@b")
		for name, tc := range map[string]struct {
			flag   string
			layout Layout
		}{"-d": {"-d", LayoutDiscardPool}, "-e": {"-e", LayoutLastElement}} {
			ref, mine := wds(t, pool, "px_lref"), wds(t, pool, "px_lmine")
			for _, p := range []string{ref, mine} {
				cli(t, "zfs", "create", "-o", "mountpoint=none", p)
			}
			zreceive(t, stream, "-u", tc.flag, ref)
			got, err := c.Receive(context.Background(), bytes.NewReader(stream), mine, &ReceiveOptions{Layout: tc.layout, NoMount: true})
			if err != nil || len(got) != 4 {
				t.Fatalf("%s: %d streams, %v", name, len(got), err)
			}
			a := sorted(lines(cli(t, "zfs", "list", "-r", "-H", "-t", "all", "-o", "name", ref)))
			b := sorted(lines(cli(t, "zfs", "list", "-r", "-H", "-t", "all", "-o", "name", mine)))
			na, nb := strings.ReplaceAll(strings.Join(a, "\n"), ref, "ROOT"), strings.ReplaceAll(strings.Join(b, "\n"), mine, "ROOT")
			if na != nb || len(b) < 5 {
				t.Errorf("%s: layouts differ\nzfs receive:\n%s\nhere:\n%s", name, na, nb)
			}
			cli(t, "zfs", "destroy", "-rR", ref)
			cli(t, "zfs", "destroy", "-rR", mine)
		}

		// -n: nothing happens, the whole stream is read, the answer agrees with the CLI.
		dry := wds(t, pool, "px_dry")
		got, err := c.Receive(context.Background(), bytes.NewReader(stream), dry, &ReceiveOptions{DryRun: true})
		if err != nil || len(got) != 4 {
			t.Fatalf("dry run: %d streams, %v", len(got), err)
		}
		if ok, _ := c.Exists(dry); ok {
			t.Error("a dry run created the target")
		}
		// A truncated stream fails the same way for both and leaves nothing half made but what libzfs leaves.
		cut := stream[:len(stream)/2]
		trunc := wds(t, pool, "px_cut")
		if _, err := c.Receive(context.Background(), bytes.NewReader(cut), trunc, &ReceiveOptions{NoMount: true}); err == nil {
			t.Error("a truncated replication stream was received without an error")
		}
		cmd := exec.Command("zfs", "receive", "-u", wds(t, pool, "px_cutref"))
		cmd.Stdin = bytes.NewReader(cut)
		if err := cmd.Run(); err == nil {
			t.Error("zfs receive accepted the truncated stream")
		}
	})
}

// bigTree is plainTree with 6 MiB of random data in the child, so that cutting
// a stream in half lands inside a stream of a snapshot.
func bigTree(t *testing.T, pool, name string) string {
	t.Helper()
	src := wds(t, pool, name)
	cli(t, "zfs", "create", "-o", "mountpoint="+tdir(t), src)
	cli(t, "zfs", "create", src+"/kid")
	data := make([]byte, 6<<20)
	rand.Read(data)
	mp := cli(t, "zfs", "get", "-H", "-o", "value", "mountpoint", src+"/kid")
	if err := os.WriteFile(filepath.Join(mp, "big"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	cli(t, "zfs", "snapshot", "-r", src+"@a")
	cli(t, "zfs", "snapshot", "-r", src+"@b")
	return src
}

func namesBelow(t *testing.T, root string) string {
	t.Helper()
	out, err := exec.Command("zfs", "list", "-r", "-H", "-t", "all", "-o", "name", root).Output()
	if err != nil {
		return "(absent)"
	}
	return strings.ReplaceAll(string(out), root, "ROOT")
}

// A sender that dies halfway: the receiver fails as zfs receive does and
// leaves what zfs receive leaves.
func TestReplicationSenderDiesHalfway(t *testing.T) {
	c, pool := testClient(t)
	src := bigTree(t, pool, "pd_src")
	full := theirs(t, "-R", src+"@b")
	for _, frac := range []int{2, 3} { // half and a third in
		cut := len(full) / frac
		// A real process produces the cut stream: head ends early and zfs send dies of SIGPIPE.
		sh := exec.Command("sh", "-c", fmt.Sprintf("zfs send -R %s@b | head -c %d", src, cut))
		out, err := sh.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := sh.Start(); err != nil {
			t.Fatal(err)
		}
		mine := wds(t, pool, "pd_mine")
		_, rerr := c.Receive(context.Background(), io.MultiReader(out), mine, &ReceiveOptions{NoMount: true})
		sh.Wait()
		if rerr == nil {
			t.Fatalf("1/%d: a stream cut off was received without an error", frac)
		}
		if !errors.Is(rerr, ioctl.ErrStreamTruncated) && !errors.Is(rerr, ErrNotAStream) && !errors.Is(rerr, ErrBadPackage) {
			t.Logf("1/%d: error is %v", frac, rerr)
		}
		ref := wds(t, pool, "pd_ref")
		cmd := exec.Command("zfs", "receive", "-u", ref)
		cmd.Stdin = bytes.NewReader(full[:cut])
		if cmd.Run() == nil {
			t.Fatalf("1/%d: zfs receive accepted the cut stream", frac)
		}
		if a, b := namesBelow(t, ref), namesBelow(t, mine); a != b {
			t.Errorf("1/%d: what is left differs\nzfs receive:\n%s\nhere:\n%s", frac, a, b)
		}
		t.Logf("1/%d: error %v; left: %q", frac, rerr, strings.Fields(namesBelow(t, mine)))
		exec.Command("zfs", "destroy", "-rR", ref).Run()
		exec.Command("zfs", "destroy", "-rR", mine).Run()
	}
}

// Cancelling stops a receive that waits for a sender that went silent, and a
// send into a writer nobody reads; the holds taken for the send are released.
func TestReplicationCancel(t *testing.T) {
	c, pool := testClient(t)
	src := bigTree(t, pool, "pc_src")
	full := theirs(t, "-R", src+"@b")

	t.Run("receive", func(t *testing.T) {
		pr, pw := io.Pipe()
		go func() { pw.Write(full[:len(full)/2]) }() // then silence: the pipe stays open
		defer pw.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		mine := wds(t, pool, "pc_mine")
		done := make(chan error, 1)
		go func() {
			_, err := c.Receive(ctx, pr, mine, &ReceiveOptions{NoMount: true})
			done <- err
		}()
		select {
		case err := <-done:
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("error = %v, want the context's", err)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("Receive did not return after its context ended")
		}
	})

	t.Run("send", func(t *testing.T) {
		pr, pw := io.Pipe() // nobody reads
		defer pr.Close()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- c.Send(ctx, src+"@b", pw, &SendOptions{Replicate: true}) }()
		time.Sleep(500 * time.Millisecond)
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("error = %v, want context.Canceled", err)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("Send did not return after its context was cancelled")
		}
		if h := holdRows(src); len(h) != 0 {
			t.Errorf("holds left behind by the cancelled send: %v", h)
		}
		// And the tree is not damaged: it can be sent again.
		if got := ours(t, c, src+"@b", &SendOptions{Replicate: true}); len(got) != len(full) {
			t.Errorf("a later send is %d bytes, zfs send made %d", len(got), len(full))
		}
	})
}

// A target that was written to since the base snapshot refuses an incremental
// replication stream, and takes it with Force by rolling back: the same for
// zfs receive and for this library, with the stream coming from a process.
func TestReplicationForceOnADivergedTarget(t *testing.T) {
	c, pool := testClient(t)
	src := plainTree(t, pool, "pf_src")
	cli(t, "zfs", "destroy", "-r", src+"@b")
	base := theirs(t, "-R", src+"@a")
	cli(t, "zfs", "snapshot", "-r", src+"@b")

	for _, force := range []bool{false, true} {
		ref, mine := wds(t, pool, "pf_ref"), wds(t, pool, "pf_mine")
		for _, d := range []string{ref, mine} {
			zreceive(t, base, "-u", d)
			// Written to since the base snapshot: the target diverged.
			dir := tdir(t)
			cli(t, "zfs", "set", "mountpoint="+dir, d)
			exec.Command("zfs", "mount", d).Run() // setting the mountpoint may have mounted it already
			if err := os.WriteFile(filepath.Join(dir, "local"), []byte("not in the stream"), 0o644); err != nil {
				t.Fatal(err)
			}
			cli(t, "zfs", "unmount", d)
			if cli(t, "zfs", "get", "-H", "-o", "value", "mounted", d) != "no" {
				t.Fatalf("%s is still mounted", d)
			}
		}
		args := []string{"-u"}
		if force {
			args = append(args, "-F")
		}
		cmd := exec.Command("zfs", append([]string{"receive"}, append(args, ref)...)...)
		send := exec.Command("zfs", "send", "-R", "-i", "@a", src+"@b")
		pipe, _ := send.StdoutPipe()
		cmd.Stdin = pipe
		send.Start()
		refErr := cmd.Run()
		send.Wait()

		send2 := exec.Command("zfs", "send", "-R", "-i", "@a", src+"@b")
		out, _ := send2.StdoutPipe()
		send2.Start()
		_, mineErr := c.Receive(context.Background(), out, mine, &ReceiveOptions{NoMount: true, Force: force})
		send2.Wait()

		if (refErr == nil) != (mineErr == nil) {
			t.Errorf("force=%v: zfs receive: %v; here: %v", force, refErr, mineErr)
		}
		if a, b := namesBelow(t, ref), namesBelow(t, mine); a != b {
			t.Errorf("force=%v: trees differ\nzfs receive:\n%s\nhere:\n%s", force, a, b)
		}
		if force && mineErr != nil {
			t.Errorf("force: the receive failed: %v", mineErr)
		}
		if !force && mineErr == nil {
			t.Error("without force: a diverged target accepted the stream")
		}
		releaseAll(ref)
		cli(t, "zfs", "destroy", "-rR", ref)
		cli(t, "zfs", "destroy", "-rR", mine)
	}
}

// tryBoth feeds the same bytes to zfs receive -u and to Receive, each into a
// fresh target, and returns both errors and what each left behind.
func tryBoth(t *testing.T, c *Client, pool string, in []byte) (refErr, mineErr error, ref, mine string) {
	t.Helper()
	rd, md := wds(t, pool, "tb_ref"), wds(t, pool, "tb_mine")
	cmd := exec.Command("zfs", "receive", "-u", rd)
	cmd.Stdin = bytes.NewReader(in)
	refErr = cmd.Run()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := c.Receive(ctx, bytes.NewReader(in), md, &ReceiveOptions{NoMount: true})
		done <- err
	}()
	select {
	case mineErr = <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("Receive did not return for %d bytes of input", len(in))
	}
	ref, mine = namesBelow(t, rd), namesBelow(t, md)
	releaseAll(rd)
	releaseAll(md)
	exec.Command("zfs", "destroy", "-rR", rd).Run()
	exec.Command("zfs", "destroy", "-rR", md).Run()
	return
}

// part is a piece of a replication stream.
type part struct {
	ds, snap string
	b        []byte
}

// splitStream cuts a replication stream into its header, the streams of the
// snapshots and the closing record.
func splitStream(t *testing.T, stream []byte) (head []byte, subs []part, tail []byte) {
	t.Helper()
	r := bytes.NewReader(stream)
	h, _, err := readHeaderOrEnd(r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readPackage(r, h); err != nil {
		t.Fatal(err)
	}
	pos := func() int { return len(stream) - r.Len() }
	head = stream[:pos()]
	for {
		start := pos()
		sh, end, err := readHeaderOrEnd(r)
		if err != nil {
			t.Fatal(err)
		}
		if end {
			return head, subs, stream[start:]
		}
		if _, err := skipStream(r, sh, sh.ToName); err != nil {
			t.Fatal(err)
		}
		subs = append(subs, part{sh.Dataset(), sh.Snapshot(), stream[start:pos()]})
	}
}

// A replication stream cut off or damaged inside its header fails for both
// zfs receive and Receive and creates nothing; one whose streams come in a
// different order or lack one gets the same verdict from both and leaves the
// same datasets.
func TestReplicationHeaderCutDamagedAndReordered(t *testing.T) {
	c, pool := testClient(t)
	src := plainTree(t, pool, "ph_src")
	full := theirs(t, "-R", src+"@b")
	head, subs, tail := splitStream(t, full)
	pl := int(binary.NativeEndian.Uint32(full[4:]))
	hdrEnd := len(head)
	if len(subs) != 4 || hdrEnd != 312+pl+312 {
		t.Fatalf("stream layout: %d substreams, header %d bytes, payload %d", len(subs), hdrEnd, pl)
	}

	t.Run("cut", func(t *testing.T) {
		for _, off := range []int{0, 1, 100, 311, 312, 313, 312 + pl/2, 312 + pl - 1, 312 + pl, 312 + pl + 1,
			312 + pl + 311, hdrEnd, hdrEnd + 1, hdrEnd + 100, hdrEnd + 311, hdrEnd + 312} {
			refErr, mineErr, ref, mine := tryBoth(t, c, pool, full[:off])
			if off < hdrEnd && (refErr == nil || mineErr == nil) {
				t.Errorf("cut at %d (inside the header): zfs receive: %v; here: %v", off, refErr, mineErr)
			}
			if (refErr == nil) != (mineErr == nil) || ref != mine {
				t.Errorf("cut at %d: zfs receive: %v, left %q; here: %v, left %q", off, refErr, ref, mineErr, mine)
			}
		}
	})

	t.Run("damaged", func(t *testing.T) {
		for name, off := range map[string]int{"payload": 312 + 3, "payload end": 312 + pl - 2, "checksum": 312 + pl + 8 + 2} {
			bad := append([]byte(nil), full...)
			bad[off] ^= 0x55
			refErr, mineErr, ref, mine := tryBoth(t, c, pool, bad)
			if refErr == nil || mineErr == nil {
				t.Errorf("%s: zfs receive: %v; here: %v", name, refErr, mineErr)
			}
			if !errors.Is(mineErr, ErrBadPackage) && !errors.Is(mineErr, ErrNotAStream) {
				t.Errorf("%s: error %v is not a damaged-header error", name, mineErr)
			}
			if ref != mine {
				t.Errorf("%s: left\nzfs receive: %q\nhere:        %q", name, ref, mine)
			}
		}
	})

	t.Run("reordered", func(t *testing.T) {
		join := func(order ...int) []byte {
			out := append([]byte(nil), head...)
			for _, i := range order {
				out = append(out, subs[i].b...)
			}
			return append(out, tail...)
		}
		idx := map[string]int{}
		for i, p := range subs {
			idx[p.ds[strings.LastIndexByte(p.ds, '/')+1:]+"@"+p.snap] = i
		}
		kid := "kid"
		srcA, srcB := idx[src[strings.LastIndexByte(src, '/')+1:]+"@a"], idx[src[strings.LastIndexByte(src, '/')+1:]+"@b"]
		kidA, kidB := idx[kid+"@a"], idx[kid+"@b"]
		const knownDifference = "first of the child"
		for name, in := range map[string][]byte{
			"child before parent":   join(kidA, kidB, srcA, srcB),
			"snapshots swapped":     join(srcB, srcA, kidA, kidB),
			"newest dropped":        join(srcA, kidA, kidB),
			"first of the child":    join(srcA, srcB, kidB),
			"a stream twice":        join(srcA, srcA, srcB, kidA, kidB),
			"only the header":       join(),
			"no closing record":     append(join(srcA, srcB, kidA, kidB)[:len(full)-312], 0),
			"streams without close": join(srcA, srcB, kidA, kidB)[:len(full)-312],
		} {
			refErr, mineErr, ref, mine := tryBoth(t, c, pool, in)
			if (refErr == nil) != (mineErr == nil) || ref != mine {
				if name == knownDifference {
					// zfs receive answers success and leaves the dataset out
					// when the stream of a child lacks its base snapshot; this
					// library reports the missing dataset as an error. The
					// libzfs source gives no rule for the success, so the
					// difference is documented in the README, not copied.
					t.Logf("known difference, %s: zfs receive: %v, left %q\n  here: %v, left %q", name, refErr, ref, mineErr, mine)
					continue
				}
				t.Errorf("%s: zfs receive: %v, left %q\n  here: %v, left %q", name, refErr, ref, mineErr, mine)
			} else {
				t.Logf("%s: both %s; left %d lines", name, map[bool]string{true: "accept", false: "refuse"}[refErr == nil], strings.Count(mine, "\n"))
			}
		}
	})
}

// tokensBelow lists the datasets under root that hold a receive_resume_token.
func tokensBelow(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, r := range rows(cli(t, "zfs", "get", "-r", "-H", "-o", "name,value", "receive_resume_token", root)) {
		if len(r) == 2 && r[1] != "-" {
			out[strings.ReplaceAll(r[0], root, "ROOT")] = r[1]
		}
	}
	return out
}

// zfs refuses to resume or to redact a replication stream, so the way to
// finish an interrupted -R is: resume the one stream the token names, then
// send the rest as an incremental -R. The library does the same as the CLI at each step.
func TestReplicationResumesLikeZfs(t *testing.T) {
	c, pool := testClient(t)
	src := bigTree(t, pool, "pr_src")
	full := theirs(t, "-R", src+"@b")

	var cut int
	var ref string
	for _, frac := range [][2]int{{1, 2}, {3, 5}, {2, 5}, {3, 4}, {1, 3}} {
		cut = len(full) * frac[0] / frac[1]
		ref = wds(t, pool, "pr_ref")
		cmd := exec.Command("zfs", "receive", "-s", "-u", ref)
		cmd.Stdin = bytes.NewReader(full[:cut])
		if cmd.Run() == nil {
			t.Fatalf("zfs receive accepted a stream cut at %d", cut)
		}
		if len(tokensBelow(t, ref)) > 0 {
			break
		}
		cli(t, "zfs", "destroy", "-rR", ref)
		ref = ""
	}
	if ref == "" {
		t.Skip("no cut of the stream left a resume token with this zfs")
	}
	defer func() { releaseAll(ref); exec.Command("zfs", "destroy", "-rR", ref).Run() }()

	mine := wds(t, pool, "pr_mine")
	_, err := c.Receive(context.Background(), bytes.NewReader(full[:cut]), mine, &ReceiveOptions{NoMount: true, Resumable: true})
	if err == nil {
		t.Fatal("a cut stream was received without an error")
	}
	defer func() { releaseAll(mine); exec.Command("zfs", "destroy", "-rR", mine).Run() }()

	refTok, myTok := tokensBelow(t, ref), tokensBelow(t, mine)
	if len(refTok) != len(myTok) {
		t.Fatalf("tokens differ: zfs receive %v, here %v", refTok, myTok)
	}
	for name, tok := range refTok {
		if _, ok := myTok[name]; !ok {
			t.Fatalf("zfs receive left a token on %s, we did not: %v", name, myTok)
		}
		a, err1 := ParseResumeToken(tok)
		b, err2 := ParseResumeToken(myTok[name])
		if err1 != nil || err2 != nil || a.ToName != b.ToName || a.Object != b.Object || a.Offset != b.Offset {
			t.Errorf("%s: token %+v / %+v (%v %v)", name, a, b, err1, err2)
		}
	}

	// The CLI's token resumes in both worlds; each side resumes its own.
	for name, tok := range refTok {
		ds := strings.ReplaceAll(name, "ROOT", ref)
		send := exec.Command("zfs", "send", "-t", tok)
		recv := exec.Command("zfs", "receive", "-s", "-u", ds)
		pipe, _ := send.StdoutPipe()
		recv.Stdin = pipe
		send.Start()
		if err := recv.Run(); err != nil {
			t.Fatalf("zfs: resuming %s: %v", ds, err)
		}
		send.Wait()
	}
	for name, tok := range myTok {
		ds := strings.ReplaceAll(name, "ROOT", mine)
		pr, pw := io.Pipe()
		go func() { pw.CloseWithError(c.ResumeSend(context.Background(), pw, tok, nil)) }()
		if _, err := c.Receive(context.Background(), pr, ds, &ReceiveOptions{NoMount: true, Resumable: true}); err != nil {
			t.Fatalf("here: resuming %s: %v", ds, err)
		}
	}
	if a, b := tokensBelow(t, ref), tokensBelow(t, mine); len(a) != 0 || len(b) != 0 {
		t.Errorf("tokens left after the resume: %v / %v", a, b)
	}

	// zfs receive will not take a full stream onto a dataset that has
	// snapshots, even with -F; the rest is sent as the CLI's users do, as an
	// incremental -R from the newest snapshot every dataset has. A dataset
	// that did not arrive yet comes in full.
	rest := theirs(t, "-R", "-I", "@a", src+"@b")
	zfsRecv := exec.Command("zfs", "receive", "-s", "-u", "-F", ref)
	zfsRecv.Stdin = bytes.NewReader(rest)
	if out, err := zfsRecv.CombinedOutput(); err != nil {
		t.Fatalf("zfs receive of the rest: %v\n%s", err, out)
	}
	restOurs := ours(t, c, src+"@b", &SendOptions{Replicate: true, Intermediates: true, From: "@a"})
	if _, err := c.Receive(context.Background(), bytes.NewReader(restOurs), mine, &ReceiveOptions{NoMount: true, Resumable: true, Force: true}); err != nil {
		t.Fatalf("Receive of the rest: %v", err)
	}
	sameTrees(t, "after resume and the rest", ref, mine)
	for _, d := range []string{ref, mine} {
		if got, want := namesBelow(t, d), namesBelow(t, src); got != want {
			t.Errorf("%s does not equal the source\n%s\nsource:\n%s", d, got, want)
		}
	}
}

// send -b lists the properties as they were received, not as they are now;
// the header and the streams are those of zfs send -b, and a dataset that was
// never received lists none.
func TestSendBackupLikeZfs(t *testing.T) {
	c, pool := testClient(t)
	src := replTree(t, c, pool, "bk_src")
	mid := wds(t, pool, "bk_mid")
	t.Cleanup(func() { releaseAll(mid) })
	zreceive(t, theirs(t, "-R", "-p", src+"@b"), "-u", mid)
	// Set since it was received: -b must not see these.
	cli(t, "zfs", "set", "compression=lz4", mid)
	cli(t, "zfs", "set", "atime=off", mid+"/kid")
	cli(t, "zfs", "set", "repl:local=mine", mid)
	cli(t, "zfs", "set", "repl:note=changed", mid) // received as "hello"

	for name, tc := range map[string]struct {
		root string
		args []string
		opts SendOptions
	}{
		"-b on a received tree":    {mid, []string{"-b", mid + "@b"}, SendOptions{Backup: true}},
		"-Rb on a received tree":   {mid, []string{"-R", "-b", mid + "@b"}, SendOptions{Replicate: true, Backup: true}},
		"-Rbh":                     {mid, []string{"-R", "-b", "-h", mid + "@b"}, SendOptions{Replicate: true, Backup: true, Holds: true}},
		"-Rb incremental":          {mid, []string{"-R", "-b", "-i", "@a", mid + "@b"}, SendOptions{Replicate: true, Backup: true, From: "@a"}},
		"-Rb on a never received":  {src, []string{"-R", "-b", src + "@b"}, SendOptions{Replicate: true, Backup: true}},
		"-b on the received child": {mid + "/kid", []string{"-b", mid + "/kid@b"}, SendOptions{Backup: true}},
	} {
		snapName := tc.args[len(tc.args)-1]
		want := theirs(t, tc.args...)
		o := tc.opts
		got := ours(t, c, snapName, &o)
		ws, gs := shapeOf(t, want), shapeOf(t, got)
		if ws.header != gs.header {
			t.Errorf("%s: header\nzfs:  %s\nhere: %s", name, ws.header, gs.header)
		}
		if strings.Join(ws.subs, "\n") != strings.Join(gs.subs, "\n") {
			t.Errorf("%s: streams\nzfs:\n%s\nhere:\n%s", name, strings.Join(ws.subs, "\n"), strings.Join(gs.subs, "\n"))
		}
		if len(want) != len(got) {
			t.Errorf("%s: %d bytes here, zfs sent %d", name, len(got), len(want))
		}
		if strings.Contains(ws.header, "repl:local") {
			t.Errorf("%s: zfs send -b listed a local property: the test would prove nothing", name)
		}
		if tc.root == mid && !strings.Contains(ws.header, "repl:note=hello") && !strings.Contains(ws.header, "compression=") {
			t.Errorf("%s: the oracle's header lists no received property: %s", name, ws.header)
		}
	}

	// -b is not -p: after a local change the headers differ, in both worlds.
	p := ours(t, c, mid+"@b", &SendOptions{Replicate: true, Props: true})
	b := ours(t, c, mid+"@b", &SendOptions{Replicate: true, Backup: true})
	if shapeOf(t, p).header == shapeOf(t, b).header {
		t.Error("-b and -p wrote the same header after a local change")
	}

	// And zfs receive makes the same tree from ours as from its own.
	want := theirs(t, "-R", "-b", mid+"@b")
	got := ours(t, c, mid+"@b", &SendOptions{Replicate: true, Backup: true})
	ref, mine := wds(t, pool, "bk_ref"), wds(t, pool, "bk_mine")
	t.Cleanup(func() { releaseAll(ref); releaseAll(mine) })
	zreceive(t, want, "-u", ref)
	zreceive(t, got, "-u", mine)
	sameTrees(t, "-Rb received", ref, mine)
}
