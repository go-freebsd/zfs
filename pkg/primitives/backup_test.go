package primitives

import (
	"context"
	"errors"
	"testing"
)

// zfs send -b lists the received values of a dataset and of its snapshots,
// not the values in effect (send_iterate_prop with received_only).
func TestSendBackupListsReceivedProperties(t *testing.T) {
	k := newFake(t)
	c := newClient(k)
	// compression was received as off and set to lz4 locally since; atime was
	// only set locally; the kernel's received list knows only the former.
	d := &Dataset{Name: "tank/a", Type: Filesystem, Props: map[string]Property{
		"compression": {Value: uint64(15), Source: "tank/a"},
		"atime":       {Value: uint64(0), Source: "tank/a"},
		"repl:note":   {Value: "mine", Source: "tank/a"},
	}}
	k.recvdProps = map[string]map[string]any{"tank/a": {"compression": uint64(2), "repl:note": "theirs"}}

	plain := (&replSend{c: c})
	got, err := plain.propsOf(d)
	if err != nil || len(got.Names()) != 3 {
		t.Fatalf("without Backup: %v %v", got.Names(), err)
	}
	if v, _ := got.Uint64("compression"); v != 15 {
		t.Errorf("without Backup compression = %d", v)
	}

	r := &replSend{c: c, backup: true}
	got, err = r.propsOf(d)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := got.Uint64("compression"); v != 2 {
		t.Errorf("compression = %d, want the received value 2", v)
	}
	if v, _ := got.String("repl:note"); v != "theirs" {
		t.Errorf("repl:note = %q", v)
	}
	if got.Has("atime") || got.Len() != 2 {
		t.Errorf("a locally set property was sent: %v", got.Names())
	}

	// Never received: nothing to list.
	k.recvdProps = nil
	if got, err := r.propsOf(d); err != nil || got.Len() != 0 {
		t.Errorf("never received: %v %v", got.Names(), err)
	}
	// The lookup is what is asked for, once per dataset.
	if n := countCalls(k, "ZFS_IOC_OBJSET_RECVD_PROPS tank/a"); n != 2 {
		t.Errorf("%d lookups", n)
	}
}

func countCalls(k *fakeKernel, prefix string) int {
	n := 0
	for _, c := range k.calls {
		if c == prefix {
			n++
		}
	}
	return n
}

func TestSendBackupIsAPackageAndNotResumable(t *testing.T) {
	k := newFake(t)
	c := newClient(k)
	// A header is written, so it is no plain send; and the CLI refuses it
	// together with a token, a saved send and a redacted one.
	for name, o := range map[string]*SendOptions{
		"saved":  {Saved: true, Backup: true},
		"resume": {ResumeObject: 1, Backup: true},
	} {
		if err := c.Send(context.Background(), "tank/a@s1", &discard{}, o); !errors.Is(err, ErrIncompatibleOptions) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := c.ResumeSend(context.Background(), &discard{}, "1-x-y", &SendOptions{Backup: true}); !errors.Is(err, ErrIncompatibleOptions) {
		t.Errorf("ResumeSend: %v", err)
	}
}

type discard struct{}

func (*discard) Write(p []byte) (int, error) { return len(p), nil }
