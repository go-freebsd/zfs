//go:build freebsd

package primitives

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/go-freebsd/zfs/pkg/ioctl"
)

// receive -x, -u and -n, with zfs receive as the oracle.

// props returns "name value source" rows of zfs get for the properties.
func props(t *testing.T, ds string, names ...string) string {
	t.Helper()
	return cli(t, "zfs", "get", "-H", "-o", "property,value,source", strings.Join(names, ","), ds)
}

func TestReceiveExcludeMatchesZfsReceiveX(t *testing.T) {
	c, pool := testClient(t)
	src, dir := source(t, c, pool, "x_src",
		map[string]any{"compression": "zle", "quota": "1G", "atime": false, "acme:x": "one"},
		map[string]string{"f": "first"})
	snap(t, c, src+"@a")
	write(t, dir, map[string]string{"f": "second"})
	snap(t, c, src+"@b")
	write(t, dir, map[string]string{"f": "third"})
	snap(t, c, src+"@c")

	// Three targets that hold received values, made by zfs receive -p.
	base := theirs(t, "-p", src+"@a")
	ds := map[string]string{}
	for _, n := range []string{"ours", "twin", "control"} {
		ds[n] = wds(t, pool, "x_"+n)
		zreceive(t, base, "-u", ds[n])
	}
	list := []string{"compression", "quota", "atime", "acme:x"}
	if got := props(t, ds["ours"], list...); !strings.Contains(got, "received") {
		t.Fatalf("the targets hold no received values, the test would prove nothing:\n%s", got)
	}

	inc := theirs(t, "-i", src+"@a", src+"@b")
	if _, err := c.Receive(context.Background(), bytes.NewReader(inc), ds["ours"],
		&ReceiveOptions{NoMount: true, Exclude: []string{"compression", "quota", "acme:x"}}); err != nil {
		t.Fatalf("Receive: %v", err)
	}
	zreceive(t, inc, "-u", "-x", "compression", "-x", "quota", "-x", "acme:x", ds["twin"])
	zreceive(t, inc, "-u", ds["control"])

	ours, twin, control := props(t, ds["ours"], list...), props(t, ds["twin"], list...), props(t, ds["control"], list...)
	if ours != twin {
		t.Errorf("ours:\n%s\nzfs receive -x:\n%s", ours, twin)
	}
	if ours == control {
		t.Errorf("excluding changed nothing:\n%s", ours)
	}
	if !strings.Contains(control, "received") {
		t.Errorf("control:\n%s", control)
	}

	// A property set locally on the target keeps its value.
	for _, d := range []string{ds["ours"], ds["twin"]} {
		cli(t, "zfs", "set", "compression=gzip", d)
	}
	inc2 := theirs(t, "-i", src+"@b", src+"@c")
	if _, err := c.Receive(context.Background(), bytes.NewReader(inc2), ds["ours"],
		&ReceiveOptions{NoMount: true, Exclude: []string{"compression"}}); err != nil {
		t.Fatal(err)
	}
	zreceive(t, inc2, "-u", "-x", "compression", ds["twin"])
	if a, b := props(t, ds["ours"], "compression"), props(t, ds["twin"], "compression"); a != b || !strings.Contains(a, "gzip\tlocal") {
		t.Errorf("local value: ours %q, zfs %q", a, b)
	}

	// Refused before the kernel is asked.
	if _, err := c.Receive(context.Background(), bytes.NewReader(theirs(t, src+"@a")), wds(t, pool, "x_bad"),
		&ReceiveOptions{Exclude: []string{"nosuchprop"}}); err == nil {
		t.Error("an unknown property was accepted")
	}
}

func TestReceiveMountsLikeZfsReceive(t *testing.T) {
	c, pool := testClient(t)
	src, dir := source(t, c, pool, "u_src", nil, map[string]string{"f": "first"})
	snap(t, c, src+"@a")
	write(t, dir, map[string]string{"f": "second", "g": "new"})
	snap(t, c, src+"@b")
	full := theirs(t, src+"@a")
	inc := theirs(t, "-i", src+"@a", src+"@b")

	mp := func(t *testing.T) map[string]any { return map[string]any{"mountpoint": tdir(t)} }
	for _, tc := range []struct {
		name    string
		o       ReceiveOptions
		cliArgs []string
		mounted string
	}{
		{"default", ReceiveOptions{}, nil, "yes"},
		{"-u", ReceiveOptions{NoMount: true}, []string{"-u"}, "no"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := wds(t, pool, "u_ours_"+strings.TrimPrefix(tc.name, "-")), wds(t, pool, "u_twin_"+strings.TrimPrefix(tc.name, "-"))
			tc.o.Props = mp(t)
			got, err := c.Receive(context.Background(), bytes.NewReader(full), a, &tc.o)
			if err != nil {
				t.Fatal(err)
			}
			zreceive(t, full, append(append([]string{"-o", "mountpoint=" + tdir(t)}, tc.cliArgs...), b)...)
			if x, y := zmounted(t, a), zmounted(t, b); x != y || x != tc.mounted {
				t.Errorf("mounted: ours %s, zfs receive %s, want %s", x, y, tc.mounted)
			}
			if (got[0].Mounted != "") != (tc.mounted == "yes") {
				t.Errorf("Received.Mounted = %q", got[0].Mounted)
			}
			if tc.mounted == "yes" {
				if a1, b1 := mountFlags(t, a), mountFlags(t, b); a1 != b1 {
					t.Errorf("flags: ours %q, zfs %q", a1, b1)
				}
				// an incremental stream into the mounted filesystem is online
				if _, err := c.Receive(context.Background(), bytes.NewReader(inc), a, &tc.o); err != nil {
					t.Fatal(err)
				}
				if zmounted(t, a) != "yes" {
					t.Error("unmounted by an incremental receive")
				}
				if where, _, _ := c.IsMounted(a); where == "" {
					t.Error("IsMounted says no")
				} else if body, _ := os.ReadFile(filepath.Join(where, "g")); string(body) != "new" {
					t.Errorf("the increment is not visible: %q", body)
				}
			}
		})
	}

	t.Run("a full stream over a mounted filesystem", func(t *testing.T) {
		a, b := wds(t, pool, "u_over_ours"), wds(t, pool, "u_over_twin")
		da, db := tdir(t), tdir(t)
		for _, p := range []struct {
			ds, dir string
		}{{a, da}, {b, db}} {
			if err := c.Create(p.ds, &CreateOptions{Props: map[string]any{"mountpoint": p.dir}}); err != nil {
				t.Fatal(err)
			}
			write(t, p.dir, map[string]string{"old": "old content"})
		}
		if _, err := c.Receive(context.Background(), bytes.NewReader(full), a, &ReceiveOptions{Force: true}); err != nil {
			t.Fatalf("ours: %v", err)
		}
		zreceive(t, full, "-F", b)
		for _, d := range []string{a, b} {
			if zmounted(t, d) != "yes" {
				t.Errorf("%s is not mounted", d)
			}
		}
		for _, dir := range []string{da, db} {
			if _, err := os.Stat(filepath.Join(dir, "old")); err == nil {
				t.Errorf("%s still has the replaced content", dir)
			}
			if body, _ := os.ReadFile(filepath.Join(dir, "f")); string(body) != "first" {
				t.Errorf("%s/f = %q", dir, body)
			}
		}
	})

	t.Run("a failed receive leaves the mount", func(t *testing.T) {
		a := wds(t, pool, "u_fail")
		da := tdir(t)
		c.Create(a, &CreateOptions{Props: map[string]any{"mountpoint": da}})
		write(t, da, map[string]string{"keep": "me"})
		bad := full[:len(full)/2]
		if _, err := c.Receive(context.Background(), bytes.NewReader(bad), a, &ReceiveOptions{Force: true}); err == nil {
			t.Fatal("a truncated stream was received")
		}
		if zmounted(t, a) != "yes" {
			t.Error("not mounted after a failed receive")
		}
		if body, _ := os.ReadFile(filepath.Join(da, "keep")); string(body) != "me" {
			t.Errorf("content lost: %q", body)
		}
	})
}

func TestDryRunMatchesZfsReceiveN(t *testing.T) {
	c, pool := testClient(t)
	big := bytes.Repeat([]byte("compressible "), 40000)
	src, dir := source(t, c, pool, "n_src", map[string]any{"recordsize": "1M", "compression": "lz4"},
		map[string]string{"data": string(big), "small": "x"})
	snap(t, c, src+"@a")
	write(t, dir, map[string]string{"data": string(big) + "more", "other": "y"})
	snap(t, c, src+"@b")
	write(t, dir, map[string]string{"third": "z"})
	snap(t, c, src+"@c")

	enc := wds(t, pool, "n_enc")
	edir := tdir(t)
	if err := c.Create(enc, &CreateOptions{Key: PassphraseKey("correct horse battery"), Props: map[string]any{"mountpoint": edir}}); err != nil {
		t.Fatal(err)
	}
	write(t, edir, map[string]string{"s": "secret"})
	snap(t, c, enc+"@a")

	streams := map[string][]byte{
		"full":        ours(t, c, src+"@a", nil),
		"incremental": ours(t, c, src+"@b", &SendOptions{From: "@a"}),
		"large":       ours(t, c, src+"@a", &SendOptions{LargeBlocks: true}),
		"embedded":    ours(t, c, src+"@a", &SendOptions{EmbedData: true}),
		"compressed":  ours(t, c, src+"@a", &SendOptions{LargeBlocks: true, Compress: true}),
		"all":         ours(t, c, src+"@b", &SendOptions{LargeBlocks: true, EmbedData: true, Compress: true, From: "@a"}),
		"raw":         ours(t, c, enc+"@a", &SendOptions{Raw: true}),
		"zfs send":    theirs(t, src+"@a"),
		"zfs send -c": theirs(t, "-Lc", src+"@a"),
		"two streams": append(ours(t, c, src+"@a", nil), ours(t, c, src+"@b", &SendOptions{From: "@a"})...),
	}
	before := cli(t, "zfs", "list", "-t", "all", "-H", "-o", "name")
	for name, stream := range streams {
		t.Run(name, func(t *testing.T) {
			dst := wds(t, pool, "n_dst")
			r := bytes.NewReader(stream)
			got, err := c.Receive(context.Background(), r, dst, &ReceiveOptions{DryRun: true})
			if err != nil {
				t.Fatal(err)
			}
			var total uint64
			for _, g := range got {
				total += g.Bytes
				if !g.DryRun {
					t.Error("DryRun not set")
				}
			}
			if total != uint64(len(stream)) || r.Len() != 0 {
				t.Errorf("read %d of %d bytes, %d left", total, len(stream), r.Len())
			}
			if ok, _ := c.Exists(dst); ok {
				t.Error("a dry run created the dataset")
			}
			// zfs receive -n agrees
			if name != "two streams" {
				cmd := exec.Command("zfs", "receive", "-n", dst)
				cmd.Stdin = bytes.NewReader(stream)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Errorf("zfs receive -n refused it: %v\n%s", err, out)
				}
			}
		})
	}
	if after := cli(t, "zfs", "list", "-t", "all", "-H", "-o", "name"); after != before {
		t.Errorf("the pool changed:\nbefore:\n%s\nafter:\n%s", before, after)
	}

	// The verdicts on the target agree with zfs receive -n, or with the real receive
	// where libzfs leaves it to the kernel.
	full, incr := streams["full"], streams["incremental"]
	t.Run("verdicts", func(t *testing.T) {
		missing := wds(t, pool, "n_missing")
		existing := wds(t, pool, "n_existing")
		c.Create(existing, &CreateOptions{NoMount: true})
		withSnaps, wdir := source(t, c, pool, "n_snaps", nil, map[string]string{"q": "q"})
		_ = wdir
		snap(t, c, withSnaps+"@only")
		parentless := missing + "/a/b"

		for _, tc := range []struct {
			name   string
			stream []byte
			target string
			o      ReceiveOptions
		}{
			{"fresh target", full, missing, ReceiveOptions{}},
			{"incremental onto nothing", incr, missing, ReceiveOptions{}},
			{"full onto existing", full, existing, ReceiveOptions{}},
			{"full onto existing, -F", full, existing, ReceiveOptions{Force: true}},
			{"full onto one with snapshots, -F", full, withSnaps, ReceiveOptions{Force: true}},
			{"no parent", full, parentless, ReceiveOptions{}},
		} {
			dry := tc.o
			dry.DryRun = true
			_, ourErr := c.Receive(context.Background(), bytes.NewReader(tc.stream), tc.target, &dry)
			args := []string{"receive", "-n"}
			if tc.o.Force {
				args = append(args, "-F")
			}
			cmd := exec.Command("zfs", append(args, tc.target)...)
			cmd.Stdin = bytes.NewReader(tc.stream)
			_, cliErr := cmd.CombinedOutput()
			if (ourErr == nil) != (cliErr == nil) {
				t.Errorf("%s: ours %v, zfs receive -n %v", tc.name, ourErr, cliErr)
			}
		}

		// libzfs does not check the base of an incremental stream; the kernel does.
		// The dry run says what the real receive says.
		dst := wds(t, pool, "n_inc")
		zreceive(t, full, "-u", dst)
		cli(t, "zfs", "snapshot", dst+"@later") // the base is no longer the latest snapshot
		_, dryErr := c.Receive(context.Background(), bytes.NewReader(incr), dst, &ReceiveOptions{DryRun: true})
		_, realErr := c.Receive(context.Background(), bytes.NewReader(incr), dst, &ReceiveOptions{NoMount: true})
		if dryErr == nil || realErr == nil || !errors.Is(dryErr, syscall.ENODEV) || !errors.Is(realErr, syscall.ENODEV) {
			t.Errorf("not the latest snapshot: dry %v, real %v", dryErr, realErr)
		}
		_, dryErr = c.Receive(context.Background(), bytes.NewReader(incr), dst, &ReceiveOptions{DryRun: true, Force: true})
		_, realErr = c.Receive(context.Background(), bytes.NewReader(incr), dst, &ReceiveOptions{NoMount: true, Force: true})
		if dryErr != nil || realErr != nil {
			t.Errorf("with Force: dry %v, real %v", dryErr, realErr)
		}
		// a base that does not exist at all
		other := wds(t, pool, "n_other")
		zreceive(t, theirs(t, enc+"@a"), "-u", other)
		_, dryErr = c.Receive(context.Background(), bytes.NewReader(incr), other, &ReceiveOptions{DryRun: true, Force: true})
		_, realErr = c.Receive(context.Background(), bytes.NewReader(incr), other, &ReceiveOptions{NoMount: true, Force: true})
		if dryErr == nil || realErr == nil {
			t.Errorf("unknown base: dry %v, real %v", dryErr, realErr)
		}
	})

	t.Run("truncated", func(t *testing.T) {
		stream := streams["full"]
		dst := wds(t, pool, "n_trunc")
		for _, cut := range []int{len(stream) - 1, len(stream) / 2, ioctl.ReplayRecordSize + 10} {
			_, err := c.Receive(context.Background(), bytes.NewReader(stream[:cut]), dst, &ReceiveOptions{DryRun: true})
			if !errors.Is(err, ioctl.ErrStreamTruncated) {
				t.Errorf("cut at %d: %v", cut, err)
			}
			cmd := exec.Command("zfs", "receive", "-n", dst)
			cmd.Stdin = bytes.NewReader(stream[:cut])
			if _, cliErr := cmd.CombinedOutput(); cliErr == nil {
				t.Errorf("cut at %d: zfs receive -n accepted it", cut)
			}
		}
	})
}
