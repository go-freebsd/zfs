package primitives

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-freebsd/nvlist/pkg/nvlist"
	"github.com/go-freebsd/zfs/pkg/ioctl"
)

// SendOptions are the settings of a send. They correspond to the flags of
// zfs send for a single snapshot stream.
type SendOptions struct {
	// From makes the stream incremental: the full name of an earlier
	// snapshot ("pool/fs@a") or bookmark ("pool/fs#b") of the same dataset,
	// or just "@a" / "#b". Empty is a full stream.
	From string

	// Intermediates (zfs send -I) also sends every snapshot between From
	// and the snapshot, each as its own stream, one after the other. From
	// must be a snapshot. Without it only the difference between the two is
	// sent (zfs send -i).
	Intermediates bool

	LargeBlocks bool // -L: blocks larger than 128 KiB
	EmbedData   bool // -e: embedded data blocks
	Compress    bool // -c: send blocks as they are compressed on disk
	Raw         bool // -w: raw stream; encrypted data stays encrypted
	Saved       bool // -S: send the saved state of a partially received dataset; the "snapshot" is the dataset

	// ResumeObject and ResumeOffset restart an interrupted send where it
	// stopped (zfs send -t); ResumeBytes is what was already received and
	// only matters for SendSize. ResumeSend sets all three from a token.
	ResumeObject, ResumeOffset, ResumeBytes uint64

	// RedactBookmark sends a redacted stream, using the redaction bookmark
	// made by Redact (zfs send --redact): its name, with or without the
	// dataset and #.
	RedactBookmark string

	// Progress, if set, is called about every ProgressInterval (100 ms if
	// zero) with the bytes the kernel has written so far. It runs on its own
	// goroutine and stops before Send returns.
	Progress         func(SendProgress)
	ProgressInterval time.Duration
}

// SendProgress is the state of a running send, from ZFS_IOC_SEND_PROGRESS.
type SendProgress struct {
	Bytes  uint64
	Blocks uint64
}

func (o *SendOptions) flags(in *nvlist.List) {
	if o == nil {
		return
	}
	for _, f := range []struct {
		on   bool
		name string
	}{{o.LargeBlocks, "largeblockok"}, {o.EmbedData, "embedok"}, {o.Compress, "compressok"}, {o.Raw, "rawok"}, {o.Saved, "savedok"}} {
		if f.on {
			in.Add(f.name, nvlist.Flag{})
		}
	}
}

// expand turns "@a" and "#b" into full names.
func expand(dataset, name string) string {
	if strings.HasPrefix(name, "@") || strings.HasPrefix(name, "#") {
		return dataset + name
	}
	return name
}

// shortBook is the bookmark name without dataset and #, which is what the
// kernel takes for redaction bookmarks.
func shortBook(name string) string {
	if i := strings.LastIndexByte(name, '#'); i >= 0 {
		return name[i+1:]
	}
	return name
}

func datasetOf(name string) string {
	if i := strings.IndexAny(name, "@#"); i >= 0 {
		return name[:i]
	}
	return name
}

func (o *SendOptions) sendArgs(snapshot string, fd int) (*nvlist.List, error) {
	in := nvlist.New()
	in.Add("fd", int32(fd))
	if o == nil {
		return in, nil
	}
	if o.From != "" {
		if !strings.ContainsAny(o.From, "@#") {
			return nil, fmt.Errorf("zfs: send: from %q is neither a snapshot nor a bookmark", o.From)
		}
		in.Add("fromsnap", expand(datasetOf(snapshot), o.From))
	}
	o.flags(in)
	if o.ResumeObject != 0 || o.ResumeOffset != 0 {
		in.Add("resume_object", o.ResumeObject)
		in.Add("resume_offset", o.ResumeOffset)
	}
	if o.RedactBookmark != "" {
		in.Add("redactbook", shortBook(o.RedactBookmark))
	}
	return in, nil
}

// SendFD writes the stream of one snapshot to a file descriptor, which must
// be open for writing and blocking (a pipe, a socket or a file). It returns
// when the whole stream is written. For Send with an io.Writer see Send.
//
// The call is not repeated if it is interrupted: a half written stream
// cannot be restarted.
func (c *Client) SendFD(snapshot string, fd int, o *SendOptions) error {
	in, err := o.sendArgs(snapshot, fd)
	if err != nil {
		return err
	}
	_, err = c.c.CallOnce(ioctl.IOCSendNew, snapshot, in)
	return err
}

// SendSize estimates the size of the stream in bytes without writing it
// (zfs send -n). The flags of o change the answer: a compressed stream is
// smaller.
func (c *Client) SendSize(snapshot string, o *SendOptions) (uint64, error) {
	in := nvlist.New()
	if o != nil {
		if o.From != "" {
			in.Add("from", expand(datasetOf(snapshot), o.From))
		}
		o.flags(in)
		in.Delete("savedok") // not a key of the space query
		if o.ResumeObject != 0 || o.ResumeOffset != 0 {
			in.Add("resume_object", o.ResumeObject)
			in.Add("resume_offset", o.ResumeOffset)
			in.Add("bytes", o.ResumeBytes)
		}
		if o.RedactBookmark != "" {
			in.Add("redactbook", shortBook(o.RedactBookmark))
		}
	}
	out, err := c.c.Call(ioctl.IOCSendSpace, snapshot, in)
	if err != nil {
		return 0, err
	}
	if out == nil {
		return 0, errors.New("zfs: send size: the kernel returned no result")
	}
	n, ok := out.Uint64("space")
	if !ok {
		return 0, errors.New("zfs: send size: no space value in the result")
	}
	return n, nil
}

// SendProgress asks how far a running send of snapshot to the descriptor fd
// of this process has come. It fails with ENOENT if there is none.
func (c *Client) SendProgress(snapshot string, fd int) (SendProgress, error) {
	cmd := ioctl.NewCmd()
	if err := cmd.SetName(snapshot); err != nil {
		return SendProgress{}, err
	}
	cmd.Cookie = uint64(fd)
	if err := c.c.Ioctl(ioctl.IOCSendProgress, cmd); err != nil {
		return SendProgress{}, err
	}
	return SendProgress{Bytes: cmd.Cookie, Blocks: cmd.ObjsetType}, nil
}

// Redact creates the redaction bookmark of a redacted send: the blocks
// that differ between snapshot and each of the redaction snapshots are left
// out of a stream sent with SendOptions.RedactBookmark.
func (c *Client) Redact(snapshot, bookmark string, redactionSnapshots []string) error {
	snaps := nvlist.New()
	for _, s := range redactionSnapshots {
		snaps.Add(s, nvlist.Flag{})
	}
	in := nvlist.New()
	in.Add("bookname", shortBook(bookmark))
	in.Add("snapnv", snaps)
	_, err := c.c.Call(ioctl.IOCRedact, snapshot, in)
	return err
}

// Send writes the stream of a snapshot to w. If w is an *os.File its
// descriptor is handed to the kernel directly (and made blocking). Any other
// writer is fed through a pipe by a goroutine; an error from w ends the send.
//
// Cancelling ctx stops a send to a pipe; a send to an *os.File cannot be
// interrupted. With o.Intermediates the intermediate snapshots follow as
// further streams.
func (c *Client) Send(ctx context.Context, snapshot string, w io.Writer, o *SendOptions) error {
	if o == nil {
		o = &SendOptions{}
	}
	if !o.Intermediates {
		return c.sendOne(ctx, snapshot, w, o)
	}
	steps, err := c.intermediateSteps(snapshot, o)
	if err != nil {
		return err
	}
	for _, s := range steps {
		step := *o
		step.From, step.Intermediates = s.from, false
		if err := c.sendOne(ctx, s.to, w, &step); err != nil {
			return fmt.Errorf("zfs: send %s: %w", s.to, err)
		}
	}
	return nil
}

type sendStep struct{ from, to string }

// intermediateSteps lists the incremental steps from o.From to snapshot.
func (c *Client) intermediateSteps(snapshot string, o *SendOptions) ([]sendStep, error) {
	ds := datasetOf(snapshot)
	from := expand(ds, o.From)
	if !strings.Contains(from, "@") {
		return nil, errors.New("zfs: send: intermediates need From to be a snapshot")
	}
	var names []string
	err := c.Walk(ds, ListOptions{Types: Snapshot}, func(d *Dataset) error {
		names = append(names, d.Name)
		return nil
	})
	if err != nil {
		return nil, err
	}
	fi, ti := indexOf(names, from), indexOf(names, snapshot)
	switch {
	case fi < 0:
		return nil, fmt.Errorf("zfs: send: %s does not exist", from)
	case ti < 0:
		return nil, fmt.Errorf("zfs: send: %s does not exist", snapshot)
	case fi >= ti:
		return nil, fmt.Errorf("zfs: send: %s is not older than %s", from, snapshot)
	}
	var steps []sendStep
	for i := fi + 1; i <= ti; i++ {
		steps = append(steps, sendStep{from: names[i-1], to: names[i]})
	}
	return steps, nil
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

func (c *Client) sendOne(ctx context.Context, snapshot string, w io.Writer, o *SendOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if f, ok := w.(*os.File); ok {
		fd := int(f.Fd()) // makes the descriptor blocking
		stop := c.watchProgress(snapshot, fd, o)
		err := c.SendFD(snapshot, fd, o)
		stop()
		return err
	}

	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	copied := make(chan error, 1)
	go func() {
		_, err := io.Copy(w, pr)
		if err != nil {
			pr.Close() // the kernel's next write fails with EPIPE
		}
		copied <- err
	}()
	var once sync.Once
	closeReader := func() { once.Do(func() { pr.Close() }) }
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			closeReader()
		case <-done:
		}
	}()

	fd := int(pw.Fd()) // blocking, as the kernel needs it
	stop := c.watchProgress(snapshot, fd, o)
	sendErr := c.SendFD(snapshot, fd, o)
	stop()
	pw.Close() // end of stream for the copier
	var copyErr error
	select {
	case copyErr = <-copied:
	case <-ctx.Done():
		// The writer is stuck or slow and the caller gave up. The copier
		// goroutine ends when w returns.
		closeReader()
		close(done)
		return ctx.Err()
	}
	close(done)
	closeReader()

	switch {
	case sendErr == nil && copyErr == nil:
		return ctx.Err()
	case copyErr != nil && !errors.Is(copyErr, os.ErrClosed):
		return fmt.Errorf("zfs: send: writing the stream: %w", copyErr)
	case ctx.Err() != nil:
		return ctx.Err()
	case sendErr != nil:
		return sendErr
	}
	return copyErr
}

// watchProgress polls SEND_PROGRESS while a send runs. The returned function
// stops it and waits for it.
func (c *Client) watchProgress(snapshot string, fd int, o *SendOptions) (stop func()) {
	if o == nil || o.Progress == nil {
		return func() {}
	}
	interval := o.ProgressInterval
	if interval <= 0 {
		interval = 100 * time.Millisecond
	}
	quit, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-quit:
				return
			case <-t.C:
				if p, err := c.SendProgress(snapshot, fd); err == nil {
					o.Progress(p)
				}
			}
		}
	}()
	return func() { close(quit); <-done }
}

// ResumeSend continues an interrupted send from the receive_resume_token
// of the target dataset (zfs send -t). The flags the original send used come
// from the token; o may add others and set Progress. The snapshot is found
// by its GUID, like zfs does, so it may have been renamed.
func (c *Client) ResumeSend(ctx context.Context, w io.Writer, token string, o *SendOptions) error {
	t, err := ParseResumeToken(token)
	if err != nil {
		return err
	}
	opts := SendOptions{}
	if o != nil {
		opts = *o
	}
	opts.LargeBlocks = opts.LargeBlocks || t.LargeBlocks
	opts.EmbedData = opts.EmbedData || t.EmbedData
	opts.Compress = opts.Compress || t.Compress
	opts.Raw = opts.Raw || t.Raw
	opts.Saved = opts.Saved || t.Saved
	opts.ResumeObject, opts.ResumeOffset, opts.ResumeBytes = t.Object, t.Offset, t.Bytes
	opts.Intermediates = false

	name := t.ToName
	if !opts.Saved {
		if name, err = c.findByGUID(t.ToName, t.ToGUID, false); err != nil {
			return fmt.Errorf("zfs: cannot resume send: %s used in the initial send: %w", t.ToName, err)
		}
	}
	if t.FromGUID != 0 {
		from, err := c.findByGUID(t.ToName, t.FromGUID, true)
		if err != nil {
			return fmt.Errorf("zfs: cannot resume send: incremental source %#x no longer exists: %w", t.FromGUID, err)
		}
		opts.From = from
	}
	return c.sendOne(ctx, name, w, &opts)
}

// ErrGUIDNotFound is returned when no snapshot with a GUID exists.
var ErrGUIDNotFound = errors.New("no snapshot with that GUID")

// findByGUID looks for a snapshot (or bookmark) with the GUID, first in the
// dataset of name, then in each parent in turn, like libzfs's guid_to_name.
func (c *Client) findByGUID(name string, guid uint64, bookmarks bool) (string, error) {
	types := Snapshot
	if bookmarks {
		types |= Bookmark
	}
	base := datasetOf(name)
	for ds := base; ds != ""; {
		var found string
		err := c.Walk(ds, ListOptions{Types: types, Depth: Unlimited, Simple: false}, func(d *Dataset) error {
			if d.GUID == guid && found == "" {
				found = d.Name
			}
			return nil
		})
		if err == nil && found != "" {
			return found, nil
		}
		if err != nil && !isNotExist(err) {
			return "", err
		}
		i := strings.LastIndexByte(ds, '/')
		if i < 0 {
			break
		}
		ds = ds[:i]
	}
	return "", ErrGUIDNotFound
}
