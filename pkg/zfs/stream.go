package zfs

import (
	"context"
	"io"
	"strings"
)

// Send writes the stream of the snapshot to w. A stream can be received
// elsewhere with Dataset.Receive, or by zfs receive. If w is an *os.File
// (a file, a pipe, a socket's File) the kernel writes to it directly; any
// other writer is fed through a pipe, and an error from w ends the send.
//
// The options select the incremental source (SendFrom, SendIntermediates),
// a replication stream (Replicate, SendProps, SendHolds) and the stream
// format (LargeBlocks, EmbeddedData, Compressed, Raw, RedactedWith), and
// OnProgress reports how far it has come. Cancelling
// ctx stops a send to a writer that is not an *os.File.
func (s *Snapshot) Send(ctx context.Context, w io.Writer, opts ...Option) error {
	cfg, err := collect("send", scSend, opts)
	if err != nil {
		return wrap("send", s.name, err)
	}
	return wrap("send", s.name, s.c.b.Send(ctx, s.name, w, cfg.sendOpts()))
}

// SendSize estimates the size of the stream in bytes without writing it
// (zfs send -n). The options that change the stream change the answer.
func (s *Snapshot) SendSize(opts ...Option) (uint64, error) {
	cfg, err := collect("send size", scSend, opts)
	if err != nil {
		return 0, wrap("send size", s.name, err)
	}
	n, err := s.c.b.SendSize(s.name, cfg.sendOpts())
	return n, wrap("send size", s.name, err)
}

// Redact makes the redaction bookmark of a redacted send: blocks that
// differ between the snapshot and each of the redaction snapshots (snapshots
// of clones of this one) are left out of a stream sent with RedactedWith.
func (s *Snapshot) Redact(bookmark string, redactionSnapshots ...*Snapshot) (*Bookmark, error) {
	names := make([]string, len(redactionSnapshots))
	for i, r := range redactionSnapshots {
		names[i] = r.name
	}
	bm := s.c.Bookmark(s.Dataset().name + "#" + strings.TrimPrefix(bookmark, "#"))
	if err := s.c.b.Redact(s.name, bm.name, names); err != nil {
		return nil, wrap("redact", s.name, err)
	}
	return bm, nil
}

// SendSaved sends the saved state of a dataset that holds a partially
// received stream (zfs send --saved): the part that arrived, as a stream
// that can be received in turn.
func (d *Dataset) SendSaved(ctx context.Context, w io.Writer, opts ...Option) error {
	cfg, err := collect("send", scSend, opts)
	if err != nil {
		return wrap("send", d.name, err)
	}
	o := cfg.sendOpts()
	o.Saved = true
	return wrap("send", d.name, d.c.b.Send(ctx, d.name, w, o))
}

// ResumeSend continues an interrupted send from the token the receiving side
// reports (Dataset.ResumeToken), writing the rest of the stream to w
// (zfs send -t). The flags of the first send come from the token. The snapshot is
// found by its GUID, so it may have been renamed.
func (c *Client) ResumeSend(ctx context.Context, w io.Writer, token string, opts ...Option) error {
	cfg, err := collect("resume send", scSend, opts)
	if err != nil {
		return wrap("resume send", "", err)
	}
	return wrap("resume send", "", c.b.ResumeSend(ctx, w, token, cfg.sendOpts()))
}

// Receive reads send streams from r into the dataset. The dataset name is the
// target: if it exists the stream must be an increment on it (or Force),
// otherwise a full stream creates it. The snapshot name comes from the stream.
// With DiscardPool or LastElement the dataset is the parent below which the
// stream's own path is rebuilt.
//
// More than one stream may follow each other, as Send with SendIntermediates
// writes them; each is received in turn. The result lists what was
// received, also when a later stream failed. Replication streams (zfs send
// -R, -p, -I, -h) are received stream by stream, with the properties,
// snapshot properties and holds of their datasets.
//
// If r is an *os.File the kernel reads from it directly. Cancelling ctx ends
// the transfer from any other reader; the goroutine copying from r then ends
// when r returns.
func (d *Dataset) Receive(ctx context.Context, r io.Reader, opts ...Option) ([]Received, error) {
	return d.c.receive(ctx, "receive", d.name, r, opts)
}

// Receive reads send streams from r into this snapshot name. The stream must
// be a single one.
func (s *Snapshot) Receive(ctx context.Context, r io.Reader, opts ...Option) ([]Received, error) {
	return s.c.receive(ctx, "receive", s.name, r, opts)
}

func (c *Client) receive(ctx context.Context, op, target string, r io.Reader, opts []Option) ([]Received, error) {
	cfg, err := collect(op, scReceive, opts)
	if err != nil {
		return nil, wrap(op, target, err)
	}
	got, err := c.b.Receive(ctx, r, target, cfg.recvOpts())
	return got, wrap(op, target, err)
}

// ResumeToken returns the token of an interrupted resumable receive into
// the dataset (its receive_resume_token property). ok is false if there is
// none. Give the token to Client.ResumeSend on the sending side.
func (d *Dataset) ResumeToken() (token string, ok bool, err error) {
	token, ok, err = d.c.b.ResumeToken(d.name)
	return token, ok, wrap("resume token", d.name, err)
}

// AbortResume discards what an interrupted resumable receive left
// (zfs receive -A).
func (d *Dataset) AbortResume() error {
	return wrap("abort resume", d.name, d.c.b.AbortResume(d.name))
}
