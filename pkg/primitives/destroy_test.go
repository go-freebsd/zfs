package primitives

import (
	"errors"
	"io/fs"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/go-freebsd/zfs/pkg/ioctl"
)

func writtenNames(k *fakeKernel) []string {
	var out []string
	for _, w := range k.writes {
		out = append(out, w.ioc.String()[len("ZFS_IOC_"):]+" "+w.name)
	}
	return out
}

// tank/a has the child tank/a/child and the snapshots s1 (cloned to
// tank/clone), s2 and s3.
func TestDestroyNamesWhatIsInTheWay(t *testing.T) {
	k := newFake(t)
	c := newClient(k)

	err := c.Destroy("tank/a", nil)
	var de *DependentsError
	if !errors.As(err, &de) || !reflect.DeepEqual(de.Children, []string{"tank/a/child", "tank/a@s1", "tank/a@s2", "tank/a@s3"}) || len(de.Clones) != 0 {
		t.Fatalf("plain: %v", err)
	}
	if !errors.Is(err, syscall.EEXIST) || !errors.Is(err, fs.ErrExist) || errors.Is(err, syscall.EBUSY) {
		t.Errorf("plain: wrong errno class: %v", err)
	}

	err = c.Destroy("tank/a", &DestroyOptions{Recursive: true})
	if !errors.As(err, &de) || !reflect.DeepEqual(de.Clones, []string{"tank/clone"}) || len(de.Children) != 0 {
		t.Fatalf("recursive: %v", err)
	}
	if !errors.Is(err, syscall.EBUSY) || errors.Is(err, syscall.EEXIST) {
		t.Errorf("recursive: wrong errno class: %v", err)
	}
	if !strings.Contains(err.Error(), "tank/clone") || !strings.Contains(err.Error(), "Clones") {
		t.Errorf("message %q", err)
	}
	if len(k.writes) != 0 {
		t.Errorf("something was written before the refusal: %v", writtenNames(k))
	}
}

func TestDestroyWithClonesGoesInTheOrderOfLibzfs(t *testing.T) {
	k := newFake(t)
	c := newClient(k)
	plan, err := c.PlanDestroy("tank/a", &DestroyOptions{Clones: true})
	want := []string{"tank/a/child", "tank/clone", "tank/a@s1", "tank/a@s2", "tank/a@s3", "tank/a"}
	if err != nil || !reflect.DeepEqual(plan.Order, want) {
		t.Fatalf("order %v, %v", plan.Order, err)
	}
	if !reflect.DeepEqual(plan.Datasets, []string{"tank/a/child", "tank/clone", "tank/a"}) || len(plan.Snapshots) != 3 {
		t.Errorf("plan %+v", plan)
	}
	if len(k.writes) != 0 {
		t.Errorf("planning wrote %v", writtenNames(k))
	}

	if err := c.Destroy("tank/a", &DestroyOptions{Clones: true}); err != nil {
		t.Fatal(err)
	}
	// The clone goes before its origin snapshot, the snapshots in one batch
	// before their dataset.
	got := writtenNames(k)
	if !reflect.DeepEqual(got, []string{"DESTROY tank/a/child", "DESTROY tank/clone", "DESTROY_SNAPS tank", "DESTROY tank/a"}) {
		t.Errorf("writes %v", got)
	}
	snaps, _ := k.writes[2].in.List("snaps")
	if !reflect.DeepEqual(snaps.SortedNames(), []string{"tank/a@s1", "tank/a@s2", "tank/a@s3"}) {
		t.Errorf("snaps %v", snaps.Names())
	}
}

func TestDestroyDryRunChangesNothing(t *testing.T) {
	c, k, m := mountedClient(t)
	c.Mount("tank/a", nil)
	k.calls = nil
	if err := c.Destroy("tank/a", &DestroyOptions{Clones: true, DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if len(k.writes) != 0 || len(k.mountCalls()) != 0 || m.table["tank/a"] == "" {
		t.Errorf("writes %v, mount calls %v, table %v", k.writes, k.mountCalls(), m.table)
	}
	// A dry run refuses what the real thing refuses.
	if err := c.Destroy("tank/a", &DestroyOptions{DryRun: true}); !errors.Is(err, syscall.EEXIST) {
		t.Errorf("dry run of a plain destroy: %v", err)
	}
}

func TestDestroyGuardsPoolsAndDeferOnDatasets(t *testing.T) {
	k := newFake(t)
	c := newClient(k)
	if err := c.Destroy("tank", nil); err == nil || !strings.Contains(err.Error(), "does not apply to pools") {
		t.Errorf("pool: %v", err)
	}
	plan, err := c.PlanDestroy("tank", &DestroyOptions{Clones: true})
	if err != nil || slices.Contains(plan.Order, "tank") || !slices.Contains(plan.Order, "tank/a") {
		t.Errorf("pool plan: %v, %v", plan, err)
	}
	if err := c.Destroy("tank/z", &DestroyOptions{Defer: true}); !errors.Is(err, syscall.EINVAL) {
		t.Errorf("Defer on a dataset: %v", err)
	}
	if err := c.Destroy("tank/a#bm1", &DestroyOptions{Recursive: true}); err == nil {
		t.Error("Recursive on a bookmark")
	}
	if err := c.Destroy("tank/a#bm1", &DestroyOptions{DryRun: true}); err == nil {
		t.Error("DryRun on a bookmark")
	}
	if len(k.writes) != 0 {
		t.Errorf("writes %v", writtenNames(k))
	}
}

// Under Recursive an error does not stop the rest; without it the first stops.
func TestDestroyCarriesOnPastErrorsWhenRecursive(t *testing.T) {
	k := newFake(t)
	noClones(k)
	c := newClient(k)
	k.writeErr = map[ioctl.IOC]error{ioctl.IOCDestroy: &ioctl.Error{Ioc: ioctl.IOCDestroy, Errno: syscall.EBUSY}}
	err := c.Destroy("tank/a", &DestroyOptions{Recursive: true})
	var be *BatchError
	if !errors.As(err, &be) || len(be.Errors) != 2 || be.Errors["tank/a/child"] == nil || be.Errors["tank/a"] == nil {
		t.Fatalf("err = %v", err)
	}
	if got := writtenNames(k); !reflect.DeepEqual(got, []string{"DESTROY tank/a/child", "DESTROY_SNAPS tank", "DESTROY tank/a"}) {
		t.Errorf("writes %v", got)
	}

	k2 := newFake(t)
	k2.writeErr = map[ioctl.IOC]error{ioctl.IOCDestroy: &ioctl.Error{Ioc: ioctl.IOCDestroy, Errno: syscall.EBUSY}}
	if err := newClient(k2).Destroy("tank/z", nil); !errors.Is(err, syscall.EBUSY) || len(k2.writes) != 1 {
		t.Errorf("single: %v, %d writes", err, len(k2.writes))
	}
}

// libzfs removes the empty mountpoint directory of a filesystem whose
// mountpoint is the default or inherited, never one that was set on it.
func TestDestroyRemovesTheDefaultMountpointDirectory(t *testing.T) {
	c, k, _ := mountedClient(t)
	c.Mount("tank/z", nil)
	k.calls = nil
	if err := c.Destroy("tank/z", nil); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(k.mountCalls(), "rmdir /tank/z") {
		t.Errorf("calls %v", k.mountCalls())
	}

	c, k, _ = mountedClient(t)
	k.ds["tank/z"].props["mountpoint"] = Property{Value: "/tank/z", Source: "tank/z"} // local
	c.Mount("tank/z", nil)
	k.calls = nil
	if err := c.Destroy("tank/z", nil); err != nil {
		t.Fatal(err)
	}
	for _, call := range k.mountCalls() {
		if strings.HasPrefix(call, "rmdir") {
			t.Errorf("removed a directory of a local mountpoint: %v", k.mountCalls())
		}
	}
}

func TestDestroyUnmountsTheClonesOutsideTheTree(t *testing.T) {
	c, k, m := mountedClient(t)
	c.Mount("tank/clone", nil)
	k.calls = nil
	if err := c.Destroy("tank/a", &DestroyOptions{Clones: true}); err != nil {
		t.Fatal(err)
	}
	got := k.mountCalls()
	i, d := slices.Index(got, "unmount /tank/clone"), slices.Index(got, "ZFS_IOC_DESTROY tank/clone")
	if i < 0 || d < 0 || i > d {
		t.Errorf("calls %v", got)
	}
	if len(m.table) != 0 {
		t.Errorf("still mounted %v", m.table)
	}
}
