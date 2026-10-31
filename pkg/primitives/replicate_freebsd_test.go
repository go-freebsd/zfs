//go:build freebsd

package primitives

import (
	"bytes"
	"testing"

	"github.com/go-freebsd/nvlist/pkg/nvlist"
	"github.com/go-freebsd/zfs/pkg/ioctl"
)

// tree makes src with a child, two snapshots on each and a clone, and returns
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
