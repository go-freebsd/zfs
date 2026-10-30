package primitives

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"

	"github.com/go-freebsd/nvlist/pkg/nvlist"
	"github.com/go-freebsd/zfs/pkg/ioctl"
)

// Layout says how the name of a received snapshot is made from the name
// the stream carries and the target given to Receive.
type Layout uint8

const (
	// LayoutExact receives into the target: a dataset name gets the stream's
	// snapshot name added ("tank/b" and "pool/fs@s" give "tank/b@s"); a
	// snapshot name is used as it is.
	LayoutExact Layout = iota
	// LayoutDiscardPool (zfs receive -d) puts the stream's dataset path
	// below the target, without the pool: "pool/a/b@s" into "tank/bk" gives
	// "tank/bk/a/b@s".
	LayoutDiscardPool
	// LayoutLastElement (zfs receive -e) keeps only the last element of the
	// stream's dataset path: "pool/a/b@s" into "tank/bk" gives "tank/bk/b@s".
	LayoutLastElement
)

// ReceiveOptions are the settings of a receive; they correspond to the
// flags of zfs receive.
type ReceiveOptions struct {
	// Force (-F) rolls the target back to its latest snapshot, destroying
	// newer snapshots and changes, so that an incremental stream applies.
	Force bool
	// Resumable (-s) keeps the part already received if the transfer is
	// interrupted; the dataset then has a receive_resume_token.
	Resumable bool
	// Heal (-c) repairs corrupted blocks of an existing snapshot from a
	// stream of that snapshot.
	Heal bool
	// Origin (-o origin=) is the snapshot a new clone is based on.
	Origin string
	// Props (-o) are properties set locally on the received dataset, in the
	// values SetProps takes.
	Props map[string]any
	// Layout selects the naming; see Layout.
	Layout Layout
	// Parents creates the missing parent filesystems of the target.
	Parents bool
}

// Received is the outcome of receiving one stream.
type Received struct {
	Header *StreamHeader
	// Snapshot is the full name the stream was received into.
	Snapshot string
	// Bytes is how much of the stream the kernel read.
	Bytes uint64
	// ErrorFlags are the ZPROP error flags (ZPROP_ERR_NOCLEAR, NORESTORE).
	ErrorFlags uint64
	// PropErrors lists properties of the stream that could not be set.
	PropErrors map[string]error
}

// ReceiveError is the error of a receive that failed. Received holds what
// is known about the attempt, Err the cause (an *ioctl.Error that unwraps to
// the errno).
type ReceiveError struct {
	Received
	Err error
}

func (e *ReceiveError) Error() string {
	return fmt.Sprintf("zfs: receive %s: %v", e.Snapshot, e.Err)
}
func (e *ReceiveError) Unwrap() error { return e.Err }

// receiveName makes the snapshot name for a stream.
func receiveName(target string, h *StreamHeader, layout Layout) (string, error) {
	snap := h.Snapshot()
	if snap == "" {
		return "", fmt.Errorf("%w: the stream has no snapshot name (%q)", ErrNotAStream, h.ToName)
	}
	if strings.Contains(target, "@") {
		if layout != LayoutExact {
			return "", errors.New("zfs: receive: a snapshot target cannot be combined with a layout")
		}
		return target, nil
	}
	ds := h.Dataset()
	switch layout {
	case LayoutDiscardPool:
		if _, rest, ok := strings.Cut(ds, "/"); ok {
			return target + "/" + rest + "@" + snap, nil
		}
	case LayoutLastElement:
		return target + "/" + ds[strings.LastIndexByte(ds, '/')+1:] + "@" + snap, nil
	}
	return target + "@" + snap, nil
}

// ReceiveFD receives one stream from a file descriptor, whose header h has
// been read already: the descriptor is positioned right after the 312 byte
// header. snapshot is the full name of the snapshot to create. The call is
// not repeated if interrupted.
func (c *Client) ReceiveFD(h *StreamHeader, fd int, snapshot string, o *ReceiveOptions) (Received, error) {
	rec := Received{Header: h, Snapshot: snapshot}
	if o == nil {
		o = &ReceiveOptions{}
	}
	if h.Compound {
		return rec, ErrCompoundStream
	}
	ds, _, ok := strings.Cut(snapshot, "@")
	if !ok || strings.ContainsAny(snapshot, "%#") {
		return rec, fmt.Errorf("zfs: receive: %q is not a snapshot name", snapshot)
	}

	// The ioctl takes the filesystem that holds the new dataset: the target
	// itself if it exists, else its parent.
	if o.Parents {
		if err := c.createParents(ds); err != nil {
			return rec, err
		}
	}
	fsname := ds
	exists, err := c.Exists(ds)
	if err != nil {
		return rec, err
	}
	if !exists {
		i := strings.LastIndexByte(ds, '/')
		if i < 0 {
			return rec, &ioctl.Error{Ioc: ioctl.IOCRecvNew, Name: ds, Errno: syscall.ENOENT}
		}
		fsname = ds[:i]
	}

	in := nvlist.New()
	in.Add("snapname", snapshot)
	if len(o.Props) > 0 {
		lp := nvlist.New()
		for _, k := range sortedKeys(o.Props) {
			v, err := convertProp(k, o.Props[k], false)
			if err != nil {
				return rec, err
			}
			lp.Add(k, v)
		}
		in.Add("localprops", lp)
	}
	if o.Origin != "" {
		in.Add("origin", o.Origin)
	}
	raw := make([]nvlist.Byte, len(h.raw))
	for i, b := range h.raw {
		raw[i] = nvlist.Byte(b)
	}
	in.Add("begin_record", raw)
	in.Add("input_fd", int32(fd))
	if o.Force {
		in.Add("force", nvlist.Flag{})
	}
	if o.Resumable {
		in.Add("resumable", nvlist.Flag{})
	}
	if o.Heal {
		in.Add("heal", nvlist.Flag{})
	}

	out, err := c.c.CallOnce(ioctl.IOCRecvNew, fsname, in)
	if errors.Is(err, ioctl.ErrTruncated) {
		return rec, nil // it happened; only the statistics are lost
	}
	if err != nil {
		var ie *ioctl.Error
		if errors.As(err, &ie) && ie.Detail != nil {
			rec.fill(ie.Detail)
		}
		return rec, &ReceiveError{Received: rec, Err: err}
	}
	if out != nil {
		rec.fill(out)
	}
	return rec, nil
}

func (r *Received) fill(l *nvlist.List) {
	r.Bytes, _ = l.Uint64("read_bytes")
	r.ErrorFlags, _ = l.Uint64("error_flags")
	if errs, ok := l.List("errors"); ok && errs.Len() > 0 {
		r.PropErrors = map[string]error{}
		for _, p := range errs.Pairs() {
			if n, ok := p.Value.(int32); ok {
				r.PropErrors[p.Name] = syscall.Errno(n)
			}
		}
	}
}

// Receive reads send streams from r and receives them into target. target
// is a dataset name, or with LayoutExact a snapshot name; see Layout. More
// than one stream may follow each other (zfs send -I does that); each is
// received in turn until r ends. It returns what was received, also when a
// later stream failed.
//
// If r is an *os.File its descriptor is passed to the kernel (and made
// blocking); other readers are copied into a pipe by a goroutine. Cancelling
// ctx ends the transfer of a pipe fed receive; the goroutine copying from r
// ends when r returns. Replication streams (send -R/-p) are refused with
// ErrCompoundStream.
func (c *Client) Receive(ctx context.Context, r io.Reader, target string, o *ReceiveOptions) ([]Received, error) {
	if o == nil {
		o = &ReceiveOptions{}
	}
	src, closeSrc, err := receiveSource(ctx, r)
	if err != nil {
		return nil, err
	}
	defer closeSrc()

	var got []Received
	for {
		if err := ctx.Err(); err != nil {
			return got, err
		}
		h, err := ReadStreamHeader(src)
		if err == io.EOF {
			if len(got) == 0 {
				return nil, fmt.Errorf("%w: the stream is empty", ErrNotAStream)
			}
			return got, nil
		}
		if err != nil {
			return got, ctxOr(ctx, err)
		}
		if h.Compound {
			return got, ErrCompoundStream
		}
		name, err := receiveName(target, h, o.Layout)
		if err != nil {
			return got, err
		}
		if len(got) > 0 && strings.Contains(target, "@") {
			return got, errors.New("zfs: receive: the target is a snapshot, but the input holds several streams")
		}
		rec, err := c.ReceiveFD(h, int(src.Fd()), name, o)
		got = append(got, rec)
		if err != nil {
			return got, ctxOr(ctx, err)
		}
	}
}

// ctxOr prefers the context's error when the transfer was cancelled.
func ctxOr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("%w (%v)", ctx.Err(), err)
	}
	return err
}

// receiveSource returns a blocking file to read the stream from. For an
// *os.File that is the file itself; otherwise a pipe fed from r.
func receiveSource(ctx context.Context, r io.Reader) (*os.File, func(), error) {
	if f, ok := r.(*os.File); ok {
		f.Fd() // makes it blocking
		return f, func() {}, nil
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	pr.Fd() // the kernel reads this end: blocking
	var once sync.Once
	closeW := func() { once.Do(func() { pw.Close() }) }
	done := make(chan struct{})
	go func() {
		io.Copy(pw, r) // ends on EOF of r, or when the pipe is closed
		closeW()       // end of stream for the reader
	}()
	go func() {
		select {
		case <-ctx.Done():
			closeW()
		case <-done:
		}
	}()
	return pr, func() {
		close(done)
		pr.Close() // a copier blocked on a full pipe gets EPIPE
		closeW()
	}, nil
}

// ResumeToken returns the receive_resume_token of a dataset: the token of an
// interrupted resumable receive. ok is false if there is none.
func (c *Client) ResumeToken(dataset string) (token string, ok bool, err error) {
	d, err := c.get(dataset, false, []string{"receive_resume_token"})
	if err != nil {
		return "", false, err
	}
	t, _ := d.String("receive_resume_token")
	if t == "" || t == "-" {
		return "", false, nil
	}
	return t, true, nil
}

// ErrNoResumeState is returned by AbortResume for a dataset without an
// interrupted resumable receive.
var ErrNoResumeState = errors.New("zfs: no resumable receive state to abort")

// AbortResume discards the partially received state of an interrupted
// resumable receive (zfs receive -A). The state is a hidden "%recv" child
// when the dataset existed, or the half received dataset itself.
func (c *Client) AbortResume(dataset string) error {
	if ok, err := c.Exists(dataset + "/%recv"); err != nil {
		return err
	} else if ok {
		return c.destroyHead(dataset + "/%recv")
	}
	d, err := c.get(dataset, false, []string{"receive_resume_token"})
	if err != nil {
		return err
	}
	if tok, _ := d.String("receive_resume_token"); !d.Inconsistent || tok == "" || tok == "-" {
		return fmt.Errorf("%w: %s", ErrNoResumeState, dataset)
	}
	return c.destroyHead(dataset)
}
