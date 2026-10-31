package primitives

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// zfs send refuses these combinations (cmd/zfs/zfs_main.c); so do we, before
// the kernel is asked anything.
func TestIncompatibleSendOptions(t *testing.T) {
	cases := map[string]*SendOptions{
		"saved with -R":         {Saved: true, Replicate: true},
		"saved with -p":         {Saved: true, Props: true},
		"saved with -h":         {Saved: true, Holds: true},
		"saved with -I":         {Saved: true, From: "a", Intermediates: true},
		"resume with -R":        {ResumeObject: 1, Replicate: true},
		"resume with -p":        {ResumeOffset: 1, Props: true},
		"resume with -h":        {ResumeObject: 1, Holds: true},
		"redact with -R":        {RedactBookmark: "#b", Replicate: true},
		"redact with -I":        {RedactBookmark: "#b", From: "a", Intermediates: true},
		"redact with raw":       {RedactBookmark: "#b", Raw: true},
		"redact with -p":        {RedactBookmark: "#b", Props: true},
		"redact with -R and -w": {RedactBookmark: "#b", Replicate: true, Raw: true},
	}
	for name, o := range cases {
		k := newFake(t)
		c := newClient(k)
		before := len(k.calls)
		var buf bytes.Buffer
		if err := c.Send(context.Background(), "tank/a@s1", &buf, o); !errors.Is(err, ErrIncompatibleOptions) {
			t.Errorf("%s: Send: %v", name, err)
		}
		if _, err := c.SendSize("tank/a@s1", o); !errors.Is(err, ErrIncompatibleOptions) {
			t.Errorf("%s: SendSize: %v", name, err)
		}
		if len(k.calls) != before || buf.Len() != 0 {
			t.Errorf("%s: the kernel was asked: %v", name, k.calls[before:])
		}
	}
	// ResumeSend takes its shape from the token.
	for name, o := range map[string]*SendOptions{
		"-R": {Replicate: true}, "-p": {Props: true}, "-h": {Holds: true},
		"-I": {Intermediates: true}, "-i": {From: "x"}, "redact": {RedactBookmark: "#b"},
	} {
		k := newFake(t)
		err := newClient(k).ResumeSend(context.Background(), &bytes.Buffer{}, "1-x-y", o)
		if !errors.Is(err, ErrIncompatibleOptions) || len(k.calls) != 0 {
			t.Errorf("resume with %s: %v %v", name, err, k.calls)
		}
	}
}
