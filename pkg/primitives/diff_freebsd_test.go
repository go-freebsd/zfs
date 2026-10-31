package primitives

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// diffLine formats an entry the way zfs diff -Ft -H prints it.
func diffLine(e DiffEntry, mnt string) string {
	ts := fmt.Sprintf("%10d.%09d", e.Ctime.Unix(), e.Ctime.Nanosecond())
	head := fmt.Sprintf("%s\t%c\t%c\t%s", ts, e.Change, e.Type, mnt+e.Path)
	switch {
	case e.Change == DiffRenamed:
		return head + "\t" + mnt + e.NewPath
	case e.LinkDelta != 0:
		return head + fmt.Sprintf("\t(%+d)", e.LinkDelta)
	}
	return head
}

func mineDiff(t *testing.T, c *Client, from, to, mnt string) []string {
	t.Helper()
	var out []string
	for e, err := range c.Diff(context.Background(), from, to) {
		if err != nil {
			t.Fatalf("Diff(%s, %s): %v", from, to, err)
		}
		out = append(out, diffLine(e, mnt))
	}
	sort.Strings(out)
	return out
}

func theirDiff(t *testing.T, args ...string) []string {
	t.Helper()
	out := lines(cli(t, "zfs", append([]string{"diff", "-F", "-t", "-H"}, args...)...))
	sort.Strings(out)
	return out
}

func sameDiff(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(want) < 3 {
		t.Fatalf("%s: zfs diff reports only %d changes: the comparison would prove nothing", label, len(want))
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s: differs\nzfs diff:\n%s\nhere:\n%s", label, strings.Join(want, "\n"), strings.Join(got, "\n"))
	}
}

// What zfs diff reports for creation, modification, deletion, renames, hard
// links, symbolic links, type changes and re-created names, in the very
// words of the CLI, between two snapshots, against the live filesystem and
// against the snapshot of a clone.
func TestDiffLikeZfs(t *testing.T) {
	c, pool := testClient(t)
	src, dir := source(t, c, pool, "df_src", nil, map[string]string{
		"keep": "same", "mod": "before", "del": "bye", "ren": "r", "victim": "v", "winner": "w",
		"hl": "h", "mode": "m", "type": "t", "redo": "old",
	})
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	p := func(n string) string { return filepath.Join(dir, n) }
	must(os.Mkdir(p("dir"), 0o755))
	must(os.WriteFile(p("dir/inner"), []byte("i"), 0o644))
	must(os.Mkdir(p("mv"), 0o755))
	must(os.Mkdir(p("gone"), 0o755))
	must(os.Symlink("keep", p("sym")))
	snap(t, c, src+"@a")

	must(os.WriteFile(p("mod"), []byte("after, and longer"), 0o644))
	must(os.Remove(p("del")))
	must(os.Rename(p("ren"), p("ren2")))
	must(os.Rename(p("winner"), p("victim"))) // rename over an existing name
	must(os.Link(p("hl"), p("hl_new")))       // a hard link appears
	must(os.Chmod(p("mode"), 0o600))          // metadata only
	must(os.Rename(p("mv"), p("dir/mv")))     // a directory moves
	must(os.Rename(p("dir"), p("dir2")))      // and a directory with content is renamed
	must(os.Remove(p("type")))                // a file becomes a directory
	must(os.Mkdir(p("type"), 0o755))
	must(os.Remove(p("redo"))) // removed and created again
	must(os.WriteFile(p("redo"), []byte("new"), 0o644))
	must(os.Remove(p("sym")))
	must(os.Symlink("mod", p("sym")))
	must(os.WriteFile(p("new"), []byte("n"), 0o644))
	must(syscall.Mkfifo(p("fifo"), 0o644))
	must(os.Remove(p("gone")))
	snap(t, c, src+"@b")

	mnt := cli(t, "zfs", "get", "-H", "-o", "value", "mountpoint", src)
	sameDiff(t, "snapshot to snapshot", mineDiff(t, c, src+"@a", src+"@b", mnt), theirDiff(t, src+"@a", src+"@b"))
	sameDiff(t, "@name shorthand", mineDiff(t, c, src+"@a", "@b", mnt), theirDiff(t, src+"@a", "@b"))

	// The live filesystem: more changes after @b.
	must(os.WriteFile(p("late"), []byte("l"), 0o644))
	must(os.Remove(p("keep")))
	sameDiff(t, "against the live filesystem", mineDiff(t, c, src+"@b", "", mnt), theirDiff(t, src+"@b"))
	if tmp := cli(t, "zfs", "list", "-H", "-t", "snapshot", "-o", "name", "-r", src); strings.Contains(tmp, "zfs-diff") {
		t.Errorf("the temporary snapshot is still there:\n%s", tmp)
	}

	// A clone of @a, changed and snapshotted: diff of @a against it.
	clone := wds(t, pool, "df_clone")
	cdir := tdir(t)
	must(c.Clone(clone, src+"@a", &CreateOptions{Props: map[string]any{"mountpoint": cdir}}))
	ensureMounted(t, clone)
	must(os.WriteFile(filepath.Join(cdir, "only_in_clone"), []byte("c"), 0o644))
	must(os.Remove(filepath.Join(cdir, "del")))
	snap(t, c, clone+"@c")
	cmnt := cli(t, "zfs", "get", "-H", "-o", "value", "mountpoint", clone)
	want := theirDiff(t, src+"@a", clone+"@c")
	// The CLI names the paths below the mountpoint of the dataset of from.
	for i := range want {
		want[i] = strings.ReplaceAll(want[i], mnt, cmnt)
	}
	got := mineDiff(t, c, src+"@a", clone+"@c", mnt)
	for i := range got {
		got[i] = strings.ReplaceAll(got[i], mnt, cmnt)
	}
	sameDiff(t, "against a clone", got, want)

	// An unrelated or earlier snapshot is refused by the kernel.
	for _, bad := range [][2]string{{src + "@b", src + "@a"}} {
		var err error
		for _, e := range c.Diff(context.Background(), bad[0], bad[1]) {
			err = e
		}
		if err == nil {
			t.Errorf("diff %s -> %s: want an error", bad[0], bad[1])
		}
	}
}

// snapTree makes pool/<name> with a child, five snapshots s1..s5 of both
// (data written in between, so that destroying frees something), a clone of
// the parent's s2 and a hold on the child's s4.
func snapTree(t *testing.T, c *Client, pool, name string) string {
	t.Helper()
	src, dir := source(t, c, pool, name, nil, nil)
	cli(t, "zfs", "create", "-o", "mountpoint="+tdir(t), src+"/kid")
	kid := cli(t, "zfs", "get", "-H", "-o", "value", "mountpoint", src+"/kid")
	for i := 1; i <= 5; i++ {
		data := make([]byte, 128<<10)
		rand.Read(data)
		for _, d := range []string{dir, kid} {
			if err := os.WriteFile(filepath.Join(d, fmt.Sprintf("f%d", i)), data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		cli(t, "zfs", "snapshot", "-r", fmt.Sprintf("%s@s%d", src, i))
	}
	cli(t, "zfs", "clone", "-o", "mountpoint=none", src+"@s2", wds(t, pool, name+"_clone"))
	cli(t, "zfs", "hold", "keep", src+"/kid@s4")
	t.Cleanup(func() { releaseAll(src) })
	return src
}

func snapsBelow(t *testing.T, root string) string {
	t.Helper()
	out, err := exec.Command("zfs", "list", "-H", "-r", "-t", "snapshot", "-o", "name", "-s", "createtxg", root).Output()
	if err != nil {
		return "(none)"
	}
	return strings.ReplaceAll(string(out), root, "ROOT")
}

// zfs destroy dataset@spec and DestroySnapshotSpec destroy the same
// snapshots for every form of the specification, with and without -r and -d,
// refuse the same ones, and the dry run names the same snapshots and the
// same space as zfs destroy -nvp.
func TestDestroySnapshotSpecLikeZfs(t *testing.T) {
	c, pool := testClient(t)
	type tc struct {
		spec      string
		recursive bool
		deferred  bool
	}
	cases := []tc{
		{"s5", false, false}, {"s3%s5", false, false}, {"%s1", false, false}, {"s5%", false, false},
		{"s3,s5", false, false}, {"s1%s1,s5", false, false}, {"s9", false, false}, {"s4%s9", false, false},
		{"s3%s4", true, false}, {"s4", true, false}, {"%", true, true}, {"s2", false, false}, {"s2", false, true},
		{"%s3", true, false}, {"%s3", true, true}, {"s4%s2", false, false},
	}
	for i, k := range cases {
		label := fmt.Sprintf("%s r=%v d=%v", k.spec, k.recursive, k.deferred)
		ref := snapTree(t, c, pool, fmt.Sprintf("sd_ref%d", i))
		mine := snapTree(t, c, pool, fmt.Sprintf("sd_mine%d", i))

		args := []string{"destroy"}
		if k.recursive {
			args = append(args, "-r")
		}
		if k.deferred {
			args = append(args, "-d")
		}
		// The dry run first.
		dry := exec.Command("zfs", append(append([]string{}, args...), "-n", "-p", ref+"@"+k.spec)...)
		dryOut, dryErr := dry.CombinedOutput()
		// The dry run asks about the very tree the CLI is asked about: the sizes
		// depend on the data, which differs between two random trees.
		plan, planErr := c.PlanDestroySnapshots(ref, k.spec, k.recursive, k.deferred)
		if (dryErr == nil) != (planErr == nil) {
			t.Errorf("%s: zfs destroy -np: %v %q; plan: %v", label, dryErr, dryOut, planErr)
		}
		if dryErr == nil && planErr == nil {
			var want []string
			var reclaim uint64
			for _, l := range lines(string(dryOut)) {
				f := strings.Fields(l)
				switch {
				case len(f) == 2 && f[0] == "destroy":
					want = append(want, strings.ReplaceAll(f[1], ref, "ROOT"))
				case len(f) == 2 && f[0] == "reclaim":
					reclaim, _ = strconv.ParseUint(f[1], 10, 64)
				}
			}
			var got []string
			for _, s := range plan.Snapshots {
				got = append(got, strings.ReplaceAll(s, ref, "ROOT"))
			}
			sort.Strings(want)
			sort.Strings(got)
			if strings.Join(want, ",") != strings.Join(got, ",") || len(want) == 0 {
				t.Errorf("%s: plan\nzfs: %v\nhere: %v", label, want, got)
			}
			if reclaim != plan.Reclaim {
				t.Errorf("%s: reclaim %d here, zfs says %d", label, plan.Reclaim, reclaim)
			}
		}

		refErr := exec.Command("zfs", append(append([]string{}, args...), ref+"@"+k.spec)...).Run()
		_, mineErr := c.DestroySnapshotSpec(mine, k.spec, k.recursive, k.deferred)
		if (refErr == nil) != (mineErr == nil) {
			t.Errorf("%s: zfs destroy: %v; here: %v", label, refErr, mineErr)
		}
		if a, b := snapsBelow(t, ref), snapsBelow(t, mine); a != b {
			t.Errorf("%s: what is left differs\nzfs destroy:\n%s\nhere:\n%s", label, a, b)
		}
		t.Logf("%s: zfs destroy %v, here %v", label, refErr, mineErr)
		for _, r := range []string{ref, mine} {
			releaseAll(r)
			exec.Command("zfs", "destroy", "-rR", r).Run()
		}
	}
}

// snapshot -o: user properties are set on the new snapshots, anything else is
// refused, by the CLI and here alike (the kernel takes only user properties).
func TestSnapshotPropertiesLikeZfs(t *testing.T) {
	c, pool := testClient(t)
	ref, mine := wds(t, pool, "so_ref"), wds(t, pool, "so_mine")
	for _, d := range []string{ref, mine} {
		cli(t, "zfs", "create", "-o", "mountpoint=none", d)
	}
	if err := exec.Command("zfs", "snapshot", "-o", "repl:note=hi", "-o", "repl:n2=two", ref+"@a").Run(); err != nil {
		t.Fatal(err)
	}
	if err := c.Snapshot([]string{mine + "@a"}, map[string]any{"repl:note": "hi", "repl:n2": "two"}); err != nil {
		t.Fatal(err)
	}
	get := func(d string) string {
		l := lines(strings.ReplaceAll(cli(t, "zfs", "get", "-H", "-o", "property,value,source", "-s", "local", "all", d+"@a"), d, "ROOT"))
		sort.Strings(l)
		return strings.Join(l, "\n")
	}
	if a, b := get(ref), get(mine); a != b || !strings.Contains(a, "repl:note") {
		t.Errorf("properties differ\nzfs:\n%s\nhere:\n%s", a, b)
	}
	for _, bad := range []string{"compression", "readonly", "used"} {
		refErr := exec.Command("zfs", "snapshot", "-o", bad+"=on", ref+"@b").Run()
		mineErr := c.Snapshot([]string{mine + "@b"}, map[string]any{bad: "on"})
		if (refErr == nil) != (mineErr == nil) || refErr == nil {
			t.Errorf("-o %s=on: zfs snapshot: %v; here: %v", bad, refErr, mineErr)
		}
	}
	if a, b := snapsBelow(t, ref), snapsBelow(t, mine); a != b {
		t.Errorf("snapshots differ: %q / %q", a, b)
	}
}
