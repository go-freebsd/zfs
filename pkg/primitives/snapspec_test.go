package primitives

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"syscall"
	"testing"

	"github.com/go-freebsd/zfs/pkg/ioctl"
)

func TestSnapSpecExpansion(t *testing.T) {
	names := []string{"s1", "s2", "s3", "s4", "s5"}
	for spec, want := range map[string]string{
		"s2":        "s2",
		"s2%s4":     "s2 s3 s4",
		"%s3":       "s1 s2 s3",
		"s3%":       "s3 s4 s5",
		"%":         "s1 s2 s3 s4 s5",
		"s1,s4%s5":  "s1 s4 s5",
		"s1,s1%s2":  "s1 s2",
		"s5%s5":     "s5",
		"s9":        "",
		"s2%s9":     "",
		"s9%s3":     "",
		"s4%s2":     "", // backwards: nothing, as libzfs
		"s9,s2":     "s2",
		"s2%s3,s9%": "s2 s3",
	} {
		got, _ := expandSnapSpec(names, spec)
		var g []string
		for n := range got {
			g = append(g, n)
		}
		sort.Strings(g)
		if strings.Join(g, " ") != want {
			t.Errorf("%q: got %q, want %q", spec, g, want)
		}
	}
	if _, ok := expandSnapSpec(names, "s2%s4"); !ok {
		t.Error("a good range is not ok")
	}
	if _, ok := expandSnapSpec(names, "s2,s9"); ok {
		t.Error("a missing name is ok")
	}
}

func destroyed(t *testing.T, k *fakeKernel) (names []string, deferred bool) {
	t.Helper()
	w := lastWrite(t, k)
	if w.ioc != ioctl.IOCDestroySnaps {
		t.Fatalf("last write is %v", w.ioc)
	}
	snaps, _ := w.in.List("snaps")
	names = snaps.Names()
	sort.Strings(names)
	return names, w.in.Has("defer")
}

func TestDestroySnapshotSpec(t *testing.T) {
	k := newFake(t)
	c := newClient(k)
	// tank/a has s1 (15, has a clone), s2 (20), s3 (30); the child has three.
	k.snap("tank/a/child", "c1", 16)
	k.snap("tank/a/child", "c2", 17)
	k.snap("tank/a/child", "s3", 18)

	got, err := c.DestroySnapshotSpec("tank/a", "s2%s3", false, false, false)
	if names, def := destroyed(t, k); err != nil || fmt.Sprint(got) != "[tank/a@s2 tank/a@s3]" || fmt.Sprint(names) != "[tank/a@s2 tank/a@s3]" || def {
		t.Fatalf("%v %v %v %v", got, err, names, def)
	}

	// The clone of s1 stops a plain destroy; nothing is sent.
	before := len(k.writes)
	_, err = c.DestroySnapshotSpec("tank/a", "%s2", false, false, false)
	if !errors.Is(err, syscall.EBUSY) || !strings.Contains(err.Error(), "tank/clone") || len(k.writes) != before {
		t.Errorf("clone: %v, %d writes", err, len(k.writes)-before)
	}
	// Deferred marks it.
	if _, err := c.DestroySnapshotSpec("tank/a", "%s2", false, true, false); err != nil {
		t.Fatal(err)
	}
	if names, def := destroyed(t, k); fmt.Sprint(names) != "[tank/a@s1 tank/a@s2]" || !def {
		t.Errorf("deferred: %v %v", names, def)
	}

	// Recursive applies the spec to the child as well: s3 exists in both.
	got, err = c.DestroySnapshotSpec("tank/a", "s3", true, false, false)
	if err != nil || fmt.Sprint(got) != "[tank/a@s3 tank/a/child@s3]" {
		t.Errorf("recursive: %v %v", got, err)
	}
	// Names that are missing somewhere are skipped; missing everywhere is ENOENT.
	got, err = c.DestroySnapshotSpec("tank/a", "c1,s9", true, false, false)
	if err != nil || fmt.Sprint(got) != "[tank/a/child@c1]" {
		t.Errorf("partial: %v %v", got, err)
	}
	before = len(k.writes)
	if _, err = c.DestroySnapshotSpec("tank/a", "s9", true, false, false); !errors.Is(err, syscall.ENOENT) || len(k.writes) != before {
		t.Errorf("nothing found: %v", err)
	}
	for _, bad := range [][2]string{{"tank/a@s1", "s1"}, {"tank/a", ""}, {"", "s1"}, {"tank/vol#b", "s1"}} {
		if _, err := c.DestroySnapshotSpec(bad[0], bad[1], false, false, false); err == nil {
			t.Errorf("%v: no error", bad)
		}
	}
}

func TestPlanDestroySnapshotsReportsTheSpaceOfEachRange(t *testing.T) {
	k := newFake(t)
	c := newClient(k)
	k.snap("tank/a/child", "c1", 16)
	k.snap("tank/a/child", "c2", 17)
	k.snap("tank/a/child", "c3", 18)
	k.snap("tank/a/child", "c4", 19)

	// c1, c3 and c4 on the child are two ranges ({c1}, {c3 c4}); s2 on tank/a is one.
	p, err := c.PlanDestroySnapshots("tank/a", "s2,c1,c3%c4", true, false, false)
	if err == nil && len(p.Snapshots) != 4 {
		t.Fatalf("%v", p.Snapshots)
	}
	// The fake answers 4096 for every range.
	if err != nil {
		// c1,c3%c4 are not in tank/a: they are skipped there, found in the child.
		t.Fatal(err)
	}
	if p.Reclaim != 3*4096 {
		t.Errorf("reclaim %d, want three ranges of 4096", p.Reclaim)
	}
	if countCalls(k, "ZFS_IOC_SPACE_SNAPS tank/a/child@c4") != 1 || countCalls(k, "ZFS_IOC_SPACE_SNAPS tank/a@s2") != 1 {
		t.Errorf("calls %v", k.calls)
	}
	// Planning destroys nothing.
	for _, w := range k.writes {
		if w.ioc == ioctl.IOCDestroySnaps {
			t.Error("the plan destroyed something")
		}
	}
}

// zfs destroy -R fs@snap: what depends on the snapshots goes first.
func TestDestroySnapshotSpecWithClones(t *testing.T) {
	k := newFake(t)
	c := newClient(k)
	plan, err := c.PlanDestroySnapshots("tank/a", "s1,s2", false, false, true)
	if err != nil || fmt.Sprint(plan.Order) != "[tank/clone tank/a@s1 tank/a@s2]" || fmt.Sprint(plan.Datasets) != "[tank/clone]" || len(plan.Snapshots) != 2 {
		t.Fatalf("plan %+v, %v", plan, err)
	}
	if len(k.writes) != 0 {
		t.Fatalf("planning wrote %v", writtenNames(k))
	}
	got, err := c.DestroySnapshotSpec("tank/a", "s1,s2", false, false, true)
	if err != nil || fmt.Sprint(got) != "[tank/a@s1 tank/a@s2]" {
		t.Fatalf("%v %v", got, err)
	}
	if w := writtenNames(k); fmt.Sprint(w) != "[DESTROY tank/clone DESTROY_SNAPS tank]" {
		t.Errorf("writes %v", w)
	}
	if _, def := destroyed(t, k); def {
		t.Error("deferred without being asked")
	}

	// Deferred snapshots, but the clones are destroyed at once.
	k.writes = nil
	if _, err := c.DestroySnapshotSpec("tank/a", "s1", false, true, true); err != nil {
		t.Fatal(err)
	}
	if w := writtenNames(k); fmt.Sprint(w) != "[DESTROY tank/clone DESTROY_SNAPS tank]" {
		t.Errorf("deferred writes %v", w)
	}
	if _, def := destroyed(t, k); !def {
		t.Error("the snapshots were not deferred")
	}

	// A snapshot without clones needs no dependents; Destroy routes "ds@snap" with Clones here.
	k.writes = nil
	if err := c.Destroy("tank/a@s3", &DestroyOptions{Clones: true}); err != nil {
		t.Fatal(err)
	}
	if w := writtenNames(k); fmt.Sprint(w) != "[DESTROY_SNAPS tank]" {
		t.Errorf("writes %v", w)
	}
}
