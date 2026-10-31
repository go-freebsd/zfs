package primitives

import (
	"reflect"
	"strings"
	"testing"
)

func snaps(names ...string) []snapInfo {
	var out []snapInfo
	for i, n := range names {
		out = append(out, snapInfo{name: n, guid: uint64(100 + i), txg: uint64(10 + i)})
	}
	return out
}

func steps(t *testing.T, r *replSend) []string {
	t.Helper()
	s, err := r.plan()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, x := range s {
		out = append(out, x.from+">"+x.to)
	}
	return out
}

func TestPlanSingleAndIncrementalAndIntermediates(t *testing.T) {
	e := &fsEntry{name: "tank/a", snaps: snaps("s1", "s2", "s3", "s4")}
	mk := func(from string, doall bool) *replSend {
		return &replSend{top: "tank/a", from: from, to: "s4", doall: doall, entries: []*fsEntry{e}}
	}
	// -p: a full stream of the last snapshot only.
	if got := steps(t, mk("", false)); !reflect.DeepEqual(got, []string{">tank/a@s4"}) {
		t.Errorf("full: %v", got)
	}
	// -i: the difference between two snapshots.
	if got := steps(t, mk("s2", false)); !reflect.DeepEqual(got, []string{"tank/a@s2>tank/a@s4"}) {
		t.Errorf("incremental: %v", got)
	}
	// -I: every step between them.
	if got := steps(t, mk("s2", true)); !reflect.DeepEqual(got, []string{"tank/a@s2>tank/a@s3", "tank/a@s3>tank/a@s4"}) {
		t.Errorf("intermediates: %v", got)
	}
	// A base that does not exist is reported, not guessed.
	r := mk("nope", false)
	if _, err := r.plan(); err == nil {
		t.Error("a missing base was accepted")
	}
	// A base that is not older than the snapshot sends nothing and warns.
	r = &replSend{top: "tank/a", from: "s4", to: "s2", doall: true, entries: []*fsEntry{e}}
	if got := steps(t, r); len(got) != 0 || len(r.warnings) == 0 {
		t.Errorf("base after the snapshot: %v, warnings %v", got, r.warnings)
	}
}

func TestPlanReplicationOrderAndClones(t *testing.T) {
	// The children are listed before the parent and the clone before its
	// origin's dataset: the plan still sends parents and origins first.
	parent := &fsEntry{name: "tank/p", guid: 1, snaps: snaps("a", "b")}
	kid := &fsEntry{name: "tank/p/k", guid: 2, snaps: []snapInfo{{"a", 201, 20}, {"b", 202, 21}}, parentFrom: 101}
	clone := &fsEntry{name: "tank/p/c", guid: 3, origin: "tank/p/k@a", originGUID: 201, parentFrom: 101,
		snaps: []snapInfo{{"b", 302, 30}}}
	r := &replSend{top: "tank/p", to: "b", recursive: true, doall: true, bySnap: map[uint64]*fsEntry{}, entries: []*fsEntry{clone, kid, parent}}
	for _, e := range r.entries {
		for _, s := range e.snaps {
			r.bySnap[s.guid] = e
		}
	}
	got := steps(t, r)
	want := []string{
		">tank/p@a", "tank/p@a>tank/p@b", // the parent, every snapshot (doall)
		">tank/p/k@a", "tank/p/k@a>tank/p/k@b", // then the origin's dataset
		"tank/p/k@a>tank/p/c@b", // then the clone, from its origin
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("order:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestPlanReplicationIncrementalLeavesOutIntermediates(t *testing.T) {
	e := &fsEntry{name: "tank/p", snaps: snaps("a", "b", "c", "d"), guid: 1}
	origin := &fsEntry{name: "tank/p/k", snaps: []snapInfo{{"x", 500, 50}, {"d", 501, 51}}, guid: 2}
	// tank/p@b is the origin of a clone elsewhere in the tree: it is sent
	// although it lies between the base and the snapshot.
	clone := &fsEntry{name: "tank/p/c", snaps: []snapInfo{{"d", 601, 60}}, origin: "tank/p@b", originGUID: 101, guid: 3}
	r := &replSend{top: "tank/p", from: "a", to: "d", recursive: true, bySnap: map[uint64]*fsEntry{},
		entries: []*fsEntry{e, origin, clone}}
	for _, x := range r.entries {
		for _, s := range x.snaps {
			r.bySnap[s.guid] = x
		}
	}
	got := steps(t, r)
	if want := []string{"tank/p@a>tank/p@b", "tank/p@b>tank/p@d"}; !reflect.DeepEqual(got[:2], want) {
		t.Errorf("tank/p: %v", got)
	}
	for _, g := range got {
		if strings.Contains(g, "@c") {
			t.Errorf("an intermediate snapshot was sent: %v", got)
		}
	}
	// The dataset that lacks the base starts full.
	var kid string
	for _, g := range got {
		if strings.HasSuffix(g, "tank/p/k@d") {
			kid = g
		}
	}
	if kid != ">tank/p/k@d" {
		t.Errorf("a dataset without the base snapshot: %q in %v", kid, got)
	}
}

func TestPlanSkipsDatasetsWithoutTheSnapshotWithAWarning(t *testing.T) {
	e := &fsEntry{name: "tank/p", snaps: snaps("a")}
	r := &replSend{top: "tank/p", to: "zzz", entries: []*fsEntry{e}}
	if got := steps(t, r); len(got) != 0 || len(r.warnings) != 1 {
		t.Errorf("%v %v", got, r.warnings)
	}
}

func TestSendPropsKeepsWhatLibzfsKeeps(t *testing.T) {
	d := &Dataset{Name: "tank/a", Type: Filesystem, Props: map[string]Property{
		"compression":    {Value: uint64(14), Source: "tank/a"},      // local
		"atime":          {Value: uint64(0), Source: "$recvd"},       // received
		"recordsize":     {Value: uint64(131072), Source: "tank"},    // inherited
		"used":           {Value: uint64(5), Source: ""},             // computed
		"mountpoint":     {Value: "/x", Source: "tank/a"},            // a string
		"repl:note":      {Value: "hi", Source: "tank/a"},            // a user property
		"repl:inherited": {Value: "no", Source: "tank"},              // inherited user property
		"quota":          {Value: uint64(1 << 20), Source: "tank/a"}, // a space limit
	}}
	got := sendProps(d)
	names := got.Names()
	want := map[string]bool{"compression": true, "atime": true, "mountpoint": true, "repl:note": true, "quota": true}
	if len(names) != len(want) {
		t.Fatalf("props = %v", names)
	}
	for _, n := range names {
		if !want[n] {
			t.Errorf("unexpected %q", n)
		}
	}
	if v, _ := got.String("mountpoint"); v != "/x" {
		t.Error("a string property is not sent as a string")
	}
	if v, _ := got.Uint64("compression"); v != 14 {
		t.Error("a number is not sent as a number")
	}
	// A snapshot does not carry the space limits.
	d.Type = Snapshot
	d.Name = "tank/a@s"
	d.Props["quota"] = Property{Value: uint64(1), Source: "tank/a@s"}
	if sendProps(d).Has("quota") {
		t.Error("quota sent for a snapshot")
	}
}

func TestReceiveNameInAPackage(t *testing.T) {
	h := &StreamHeader{ToName: "pool/a/b/c@s"}
	for _, c := range []struct {
		layout Layout
		sendfs string
		want   string
	}{
		{LayoutExact, "pool/a", "tank/bk/b/c@s"},
		{LayoutDiscardPool, "pool/a", "tank/bk/a/b/c@s"},
		{LayoutLastElement, "pool/a", "tank/bk/a/b/c@s"},
	} {
		got, err := receiveNameFrom("tank/bk", h, c.layout, c.sendfs)
		if err != nil || got != c.want {
			t.Errorf("layout %d: %q, %v; want %q", c.layout, got, err, c.want)
		}
	}
	// The pool itself is the top.
	h = &StreamHeader{ToName: "pool/kid@s"}
	if got, _ := receiveNameFrom("tank/bk", h, LayoutLastElement, "pool"); got != "tank/bk/pool/kid@s" {
		t.Errorf("last element of a pool: %q", got)
	}
}
