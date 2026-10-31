package primitives

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
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
	// Exclude (-x) names properties that the stream's value must not set:
	// they are inherited on the target instead. A plain stream carries no
	// properties, so this changes something only for a target that holds
	// received values already (from an earlier replication stream). The
	// rules of zfs receive apply: aliases are accepted, a property that is
	// set locally on an existing target, or does not apply to the dataset
	// type, is left alone, and encryption properties cannot be excluded
	// from a raw stream.
	Exclude []string
	// NoMount (-u) leaves the received filesystem unmounted. Without it a
	// new filesystem is mounted after it is received, and one that was
	// replaced is mounted again, as zfs receive does.
	NoMount bool
	// DryRun (-n) checks the stream and where it would go, and reads it to
	// its end, but receives nothing: no dataset is created, changed or
	// mounted. The checks are those zfs receive -n makes: the stream must be
	// well formed, a new filesystem on an existing target needs Force and
	// no snapshots, an incremental stream needs its base snapshot on the
	// target and an existing target, a missing parent needs Parents. Data
	// checksums are not verified, and failures only the kernel can know
	// (space, features) are not predicted.
	DryRun bool
	// SkipHolds (-k) does not put back the holds a replication stream
	// carries.
	SkipHolds bool

	// virtual lists datasets that earlier streams of a dry run would have
	// made.
	virtual map[string]bool
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
	// DryRun is set when nothing was received (ReceiveOptions.DryRun); Bytes
	// is then the length of the whole stream, header included.
	DryRun bool
	// Mounted is where the received filesystem was mounted, if it was.
	Mounted string
	// Skipped is set for a stream of a replication stream whose snapshot
	// existed already; it was read and ignored.
	Skipped bool
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
	return receiveNameFrom(target, h, layout, h.Dataset())
}

// receiveNameFrom is receiveName for a stream that is part of a replication
// stream: sendfs is the dataset the replication stream was made from, and the
// names of the streams below it keep their path below sendfs.
func receiveNameFrom(target string, h *StreamHeader, layout Layout, sendfs string) (string, error) {
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
	to := h.ToName
	switch layout {
	case LayoutDiscardPool:
		if i := strings.IndexAny(to, "/@"); i >= 0 && to[i] == '/' {
			return target + to[i:], nil
		}
		return target + "@" + snap, nil
	case LayoutLastElement:
		if i := strings.LastIndexByte(sendfs, '/'); i >= 0 {
			return target + to[i:], nil
		}
		return target + "/" + to, nil
	}
	return target + to[len(sendfs):], nil
}

// ReceiveFD receives one stream from a file descriptor, whose header h has
// been read already: the descriptor is positioned right after the 312 byte
// header. snapshot is the full name of the snapshot to create. The call is
// not repeated if interrupted.
func (c *Client) ReceiveFD(h *StreamHeader, fd int, snapshot string, o *ReceiveOptions) (Received, error) {
	return c.receiveFD(h, fd, snapshot, o, nil)
}

// substream carries what a replication stream adds to one of its streams.
type substream struct {
	// props are the properties the stream's dataset had on the sender; they
	// are received as "received" values.
	props *nvlist.List
	// noMount leaves mounting to the caller, which mounts the whole tree
	// when it is done.
	noMount bool
	// child is set for every dataset of the tree but the first: the
	// properties given with Props apply to the top only, and the others
	// inherit them (libzfs: "override props on child dataset are inherited").
	child bool
}

func (c *Client) receiveFD(h *StreamHeader, fd int, snapshot string, o *ReceiveOptions, sub *substream) (Received, error) {
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

	if o.DryRun {
		return rec, errors.New("zfs: receive: a dry run reads the stream itself, use Receive")
	}
	// The ioctl takes the filesystem that holds the new dataset: the target
	// itself if it exists, else its parent.
	if o.Parents {
		if _, err := c.createParents(ds); err != nil {
			return rec, err
		}
	}
	fsname := ds
	exists, err := c.Exists(ds)
	if err != nil {
		return rec, err
	}
	var target *Dataset
	if exists {
		if target, err = c.get(ds, false, nil); err != nil {
			return rec, err
		}
	} else {
		i := strings.LastIndexByte(ds, '/')
		if i < 0 {
			return rec, &ioctl.Error{Ioc: ioctl.IOCRecvNew, Name: ds, Errno: syscall.ENOENT}
		}
		fsname = ds[:i]
	}
	excluded, err := excludeProps(h, o, target)
	if err != nil {
		return rec, err
	}

	// A stream that makes a new filesystem on an existing one replaces it,
	// which cannot be done online: unmount it and what is below first.
	newfs := (h.FromGUID == 0 || h.Clone || o.Origin != "") && !h.Resuming

	in := nvlist.New()
	in.Add("snapname", snapshot)
	lp := nvlist.New()
	for _, k := range sortedKeys(o.Props) {
		v, err := convertProp(k, o.Props[k], false)
		if err != nil {
			return rec, err
		}
		if sub != nil && sub.child {
			lp.Add(k, nvlist.Flag{})
			continue
		}
		lp.Add(k, v)
	}
	if sub != nil && sub.props != nil {
		rp := sub.props
		// A property that cannot be inherited cannot be excluded either:
		// it is left out of the received values instead.
		var drop []string
		for _, k := range excluded {
			if info, ok := LookupProp(k); ok && info.Kind != KindUser && !info.Inherits && rp.Has(k) {
				drop = append(drop, k)
			}
		}
		if len(drop) > 0 {
			rp = copyList(rp, drop)
			excluded = removeAll(excluded, drop)
		}
		in.Add("props", rp)
		// The stream's own dataset is not encrypted; without this the new
		// filesystem would inherit encryption from its parent.
		if newfs && !h.Raw() {
			if _, set := o.Props["encryption"]; !set {
				lp.Add("encryption", uint64(2)) // ZIO_CRYPT_OFF
			}
		}
	}
	for _, k := range excluded {
		lp.Add(k, nvlist.Flag{}) // a boolean entry means -x
	}
	if lp.Len() > 0 {
		in.Add("localprops", lp)
	}
	origin := o.Origin
	if origin == "" && h.Clone {
		// A clone stream names its origin by GUID.
		if origin, err = c.findByGUID(snapshot, h.FromGUID, false); err != nil {
			return rec, fmt.Errorf("zfs: receive: local origin for clone %s does not exist: %w", snapshot, err)
		}
	}
	if origin != "" {
		in.Add("origin", origin)
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

	// A full stream onto an existing dataset needs Force, whatever the
	// snapshot: libzfs says so before it asks the kernel.
	if exists && newfs && !o.Force {
		return rec, &ReceiveError{Received: rec, Err: &needsForceError{ds}}
	}
	var cl *changelist
	if exists && h.Type == Filesystem && (newfs || h.Resuming && h.FromGUID == 0) {
		if cl, err = c.gather(ds, "name", false); err != nil {
			return rec, err
		}
		if err := cl.prefix(); err != nil {
			return rec, err
		}
	}

	out, err := c.c.CallOnce(ioctl.IOCRecvNew, fsname, in)
	recvErr := err
	if errors.Is(err, ioctl.ErrTruncated) {
		recvErr = nil // it happened; only the statistics are lost
	} else if err != nil {
		var ie *ioctl.Error
		if errors.As(err, &ie) && ie.Detail != nil {
			rec.fill(ie.Detail)
		}
		recvErr = &ReceiveError{Received: rec, Err: err}
	} else if out != nil {
		rec.fill(out)
	}

	var mountErr error
	if cl != nil && !o.NoMount {
		mountErr = cl.postfix()
	}
	if recvErr != nil {
		return rec, recvErr
	}
	if !exists && h.Type == Filesystem && !o.NoMount && !h.Redacted() && (sub == nil || !sub.noMount) {
		mountErr = firstThen(mountErr, c.autoMount(ds))
	}
	if where, ok, _ := c.IsMounted(ds); ok && !o.NoMount {
		rec.Mounted = where
	}
	return rec, mountErr
}

// excludeProps applies the rules of zfs_setup_cmdline_props to the -x list:
// it returns the property names to put into localprops.
func excludeProps(h *StreamHeader, o *ReceiveOptions, target *Dataset) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, name := range o.Exclude {
		info, ok := LookupProp(name)
		if !ok {
			return nil, propErr(name, ErrUnknownProp, "")
		}
		name = info.Name
		if name == "origin" || seen[name] {
			continue
		}
		seen[name] = true
		if _, clash := o.Props[name]; clash {
			return nil, fmt.Errorf("zfs: receive: property %q is both set and excluded", name)
		}
		switch name {
		case "encryption", "keyformat", "keylocation", "pbkdf2iters", "pbkdf2salt":
			if h.Raw() {
				return nil, fmt.Errorf("zfs: receive: encryption property %q cannot be excluded for raw streams", name)
			}
		}
		if info.Kind != KindUser && h.Type != 0 && info.Types&h.Type == 0 {
			continue // does not apply to this kind of dataset
		}
		if target != nil {
			if p, present := target.Props[name]; present && p.Source != "$recvd" {
				continue // set or inherited on the target: its value wins anyway
			}
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
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
// ends when r returns. Replication streams (send -R, -p, -I, -h) are
// received stream by stream.
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
	var prev *StreamHeader
	packages := 0
	for {
		if err := ctx.Err(); err != nil {
			return got, err
		}
		h, err := ReadStreamHeader(src)
		if err == io.EOF {
			if len(got) == 0 && packages == 0 {
				return nil, fmt.Errorf("%w: the stream is empty", ErrNotAStream)
			}
			return got, nil
		}
		if err != nil {
			return got, ctxOr(ctx, err)
		}
		if h.Compound {
			packages++
			recs, err := c.receivePackage(ctx, src, h, target, o, prev)
			got = append(got, recs...)
			if err != nil {
				return got, err
			}
			if o.DryRun && len(recs) > 0 {
				prev = recs[len(recs)-1].Header
			}
			continue
		}
		name, err := receiveName(target, h, o.Layout)
		if err != nil {
			return got, err
		}
		if len(got) > 0 && strings.Contains(target, "@") {
			return got, errors.New("zfs: receive: the target is a snapshot, but the input holds several streams")
		}
		var rec Received
		if o.DryRun {
			rec, err = c.receiveDry(h, prev, src, name, o)
			prev = h
		} else {
			rec, err = c.ReceiveFD(h, int(src.Fd()), name, o)
		}
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

// copyList returns a copy of l without the named pairs.
func copyList(l *nvlist.List, without []string) *nvlist.List {
	out := nvlist.New()
	for _, p := range l.Pairs() {
		if !slices.Contains(without, p.Name) {
			out.Add(p.Name, p.Value)
		}
	}
	return out
}

func removeAll(s, drop []string) []string {
	var out []string
	for _, x := range s {
		if !slices.Contains(drop, x) {
			out = append(out, x)
		}
	}
	return out
}

// needsForceError is the refusal to replace an existing dataset without Force.
// It matches EEXIST, like the kernel's error for the same thing.
type needsForceError struct{ dataset string }

func (e *needsForceError) Error() string {
	return "destination " + e.dataset + " exists: Force is needed to overwrite it"
}
func (e *needsForceError) Unwrap() error { return syscall.EEXIST }
