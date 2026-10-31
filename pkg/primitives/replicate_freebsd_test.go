//go:build freebsd

package primitives

import (
	"bytes"
	"context"
	"os/exec"
	"sort"
	"strings"
	"testing"

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
	snap(t, c, src+"@b")
	snap(t, c, child+"@b")
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
		out = append(out, "list\t"+norm(strings.Join(r, "\t")))
	}
	if h, err := exec.Command("zfs", "holds", "-rH", root).Output(); err == nil {
		for _, l := range lines(strings.TrimRight(string(h), "\n")) {
			f := strings.Split(l, "\t")
			if len(f) >= 2 {
				out = append(out, "hold\t"+norm(f[0])+"\t"+f[1])
			}
		}
	}
	sort.Strings(out)
	return out
}

func sameTrees(t *testing.T, label, a, b string) {
	t.Helper()
	sa, sb := treeState(t, a), treeState(t, b)
	if len(sa) < 8 {
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
		t.Logf("%s: %d streams, %d facts", name, len(got), len(treeState(t, mine)))
		releaseAll(ref)
		releaseAll(mine)
		cli(t, "zfs", "destroy", "-rR", ref)
		cli(t, "zfs", "destroy", "-rR", mine)
	}
}

// releaseAll drops every hold below root.
func releaseAll(root string) {
	out, err := exec.Command("zfs", "holds", "-rH", root).Output()
	if err != nil {
		return
	}
	for _, l := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if f := strings.Split(l, "\t"); len(f) >= 2 {
			exec.Command("zfs", "release", f[1], f[0]).Run()
		}
	}
}
