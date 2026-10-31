package primitives

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"syscall"
	"testing"

	"github.com/go-freebsd/zfs/pkg/ioctl"
)

func TestPoolPropsAndReadOnly(t *testing.T) {
	k := newFake(t)
	c := newClient(k)
	props, err := c.PoolProps("tank")
	if err != nil || props["size"] != uint64(1<<30) || props["readonly"] != uint64(0) {
		t.Fatalf("%v %v", props, err)
	}
	if ro, err := c.PoolReadOnly("tank"); err != nil || ro {
		t.Errorf("writable pool: %v %v", ro, err)
	}
	k.readOnlyPools = map[string]bool{"tank": true}
	if ro, err := c.PoolReadOnly("tank"); err != nil || !ro {
		t.Errorf("read-only pool: %v %v", ro, err)
	}
	if _, err := c.PoolReadOnly("nope"); !errors.Is(err, syscall.ENOENT) {
		t.Errorf("unknown pool: %v", err)
	}
}

func TestMountOfAReadOnlyPoolIsReadOnly(t *testing.T) {
	c, k, m := mountedClient(t)
	k.readOnlyPools = map[string]bool{"tank": true}
	if err := c.Mount("tank/a", nil); err != nil {
		t.Fatal(err)
	}
	// The dataset says rw, the pool decides: ro, once, no rw.
	if got := m.opts["tank/a"]; slices.Index(got, "rw") >= 0 || countOf(got, "ro") != 1 {
		t.Errorf("options = %v", got)
	}
	// A writable pool is untouched.
	k.readOnlyPools = nil
	if err := c.Mount("tank/z", nil); err != nil {
		t.Fatal(err)
	}
	if slices.Index(m.opts["tank/z"], "ro") >= 0 || slices.Index(m.opts["tank/z"], "rw") < 0 {
		t.Errorf("options = %v", m.opts["tank/z"])
	}
	// A failure to read the pool does not stop the mount (libzfs ignores it too).
	k.pools = nil
	if err := c.Mount("tank/vol", nil); err == nil {
		t.Log("a volume is not mountable, as before")
	}
}

func countOf(s []string, v string) int {
	n := 0
	for _, x := range s {
		if x == v {
			n++
		}
	}
	return n
}

func TestReadOnlyOptions(t *testing.T) {
	got := readOnlyOptions([]string{"rw", "noatime", "ro", "rw"})
	if !slices.Equal(got, []string{"noatime", "ro"}) {
		t.Errorf("got %v", got)
	}
}

// A send of a tree from a read-only pool takes no holds (it could not), and
// one from a writable pool does.
func TestReplicationSendTakesNoHoldsOnAReadOnlyPool(t *testing.T) {
	for _, ro := range []bool{false, true} {
		k := newFake(t)
		k.readOnlyPools = map[string]bool{"tank": ro}
		c := newClient(k)
		k.sendData = map[string][]byte{"tank/a@s1": []byte("[1]"), "tank/a@s1>tank/a@s2": []byte("[1>2]")}
		var buf bytes.Buffer
		err := c.Send(context.Background(), "tank/a@s2", &buf, &SendOptions{From: "@s1", Intermediates: true})
		if err != nil {
			t.Fatalf("read-only=%v: %v", ro, err)
		}
		holds := writesOf(k, ioctl.IOCHold)
		if ro && len(holds) != 0 {
			t.Errorf("holds on a read-only pool: %+v", holds)
		}
		if !ro && len(holds) != 1 {
			t.Errorf("writable pool: %d hold calls", len(holds))
		}
	}
}

func TestReadOnlyPoolErrorIsEROFS(t *testing.T) {
	k := newFake(t)
	c := newClient(k)
	k.writeErr = map[ioctl.IOC]error{ioctl.IOCCreate: &ioctl.Error{Ioc: ioctl.IOCCreate, Name: "tank/n", Errno: syscall.EROFS}}
	err := c.Create("tank/n", nil)
	if !errors.Is(err, syscall.EROFS) {
		t.Errorf("create: %v", err)
	}
}
