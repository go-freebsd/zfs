package primitives

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"

	"github.com/go-freebsd/nvlist/pkg/nvlist"
)

// Replication streams (zfs send -R, -p, -I, -h) are assembled in userland: a
// header that describes the datasets, then one kernel stream per snapshot.
// This file is the port of libzfs's send side of that: gather_nvlist and
// send_iterate_* describe the datasets, dump_filesystems and dump_snapshot
// choose which snapshots go out in which order.

// fsEntry is one dataset of a replication stream.
type fsEntry struct {
	name   string
	guid   uint64
	origin string // the snapshot a clone was made from
	nv     *nvlist.List
	snaps  []snapInfo // every snapshot of the dataset, oldest first
	originGUID,
	parentFrom uint64
}

type snapInfo struct {
	name string // short name
	guid uint64
	txg  uint64
}

// replSend is the state of one replication send (send_data_t and
// send_dump_data_t of libzfs).
type replSend struct {
	c        *Client
	o        *SendOptions
	top      string // the dataset of the snapshot
	from, to string // short names
	pool     string

	recursive, doall, props, holds, backup, raw, skipMissing bool

	fss     *nvlist.List
	entries []*fsEntry
	bySnap  map[uint64]*fsEntry // fsavl: snapshot GUID -> dataset

	// state of the walk
	parentFrom   uint64
	toTxg        uint64
	seenFrom     bool
	seenTo       bool
	cloneOrigins map[uint64]bool

	warnings []string
}

func (r *replSend) warn(format string, a ...any) {
	r.warnings = append(r.warnings, fmt.Sprintf(format, a...))
}

func shortName(snapshot string) string {
	_, s, _ := strings.Cut(snapshot, "@")
	return s
}

// sendStep is one kernel send of a replication stream.
type sendStep struct {
	to, from string // full snapshot names; from is empty for a full stream
}

// wantsHeaderNvlist is gather_props of send_prelim_records.
func (r *replSend) wantsHeaderNvlist() bool { return r.recursive || r.props || r.holds || r.backup }

func (c *Client) newReplSend(snapshot string, o *SendOptions) (*replSend, error) {
	top, to, ok := strings.Cut(snapshot, "@")
	if !ok || to == "" || strings.ContainsAny(snapshot, "#%") {
		return nil, fmt.Errorf("zfs: send: %q is not a snapshot name", snapshot)
	}
	r := &replSend{
		c: c, o: o, top: top, to: to,
		pool:        strings.SplitN(top, "/", 2)[0],
		recursive:   o.Replicate,
		doall:       o.Intermediates || (o.Replicate && o.From == ""), // zfs send -R sends every snapshot
		props:       o.Props,
		holds:       o.Holds,
		backup:      o.Backup,
		raw:         o.Raw,
		skipMissing: o.SkipMissing,
		bySnap:      map[uint64]*fsEntry{},
	}
	if o.From != "" {
		from := expand(top, o.From)
		ds, short, ok := strings.Cut(from, "@")
		if !ok || strings.Contains(from, "#") {
			return nil, errors.New("zfs: send: a replication stream needs a snapshot to start from, not a bookmark")
		}
		if ds != top {
			return nil, fmt.Errorf("zfs: send: %s is not a snapshot of %s", from, top)
		}
		r.from = short
	} else if o.Intermediates {
		return nil, errors.New("zfs: send: intermediates need From to be a snapshot")
	}
	if err := o.check(); err != nil {
		return nil, err
	}
	if o.RedactBookmark != "" {
		return nil, fmt.Errorf("%w: a stream with Props or Holds cannot be redacted here", ErrIncompatibleOptions)
	}
	return r, nil
}

// gather describes the dataset tree (gather_nvlist).
func (r *replSend) gather() error {
	d, err := r.c.get(r.top, false, nil)
	if err != nil {
		return err
	}
	if d.Type != Filesystem && d.Type != Volume {
		return fmt.Errorf("zfs: send: %s is not a filesystem or volume", r.top)
	}
	r.fss = nvlist.New()
	return r.iterateFS(d)
}

var errMissingSnapshot = errors.New("a snapshot is missing")

// snapshotsOf lists the snapshots of a dataset, oldest first.
func (c *Client) snapshotsOf(dataset string) ([]*Dataset, error) {
	var out []*Dataset
	err := c.Walk(dataset, ListOptions{Types: Snapshot}, func(d *Dataset) error {
		out = append(out, d)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreationTxg < out[j].CreationTxg })
	return out, nil
}

func (r *replSend) iterateFS(d *Dataset) error {
	savedParent, savedTo := r.parentFrom, r.toTxg
	defer func() { r.parentFrom, r.toTxg = savedParent, savedTo }()

	snaps, err := r.c.snapshotsOf(d.Name)
	if err != nil {
		return err
	}
	var toTxg uint64
	for _, s := range snaps {
		if shortName(s.Name) == r.to {
			toTxg = s.CreationTxg
		}
	}
	if toTxg != 0 {
		r.toTxg = toTxg
	}
	if toTxg == 0 && r.to != "" {
		switch {
		case r.toTxg != 0 && d.CreationTxg > r.toTxg:
			return nil // created after the snapshot: it is not part of the stream
		case r.skipMissing:
			r.warn("skipping dataset %s and its children: snapshot %s does not exist", d.Name, r.to)
			return nil
		}
		return fmt.Errorf("zfs: send: cannot send %s@%s%s: snapshot %s@%s does not exist: %w",
			r.top, r.to, map[bool]string{true: " recursively"}[r.recursive], d.Name, r.to, errMissingSnapshot)
	}

	e := &fsEntry{name: d.Name, guid: d.GUID, origin: d.Origin}
	for _, s := range snaps {
		e.snaps = append(e.snaps, snapInfo{shortName(s.Name), s.GUID, s.CreationTxg})
	}
	nvfs := nvlist.New()
	nvfs.Add("name", d.Name)
	nvfs.Add("parentfromsnap", r.parentFrom)
	e.parentFrom = r.parentFrom
	if d.Origin != "" {
		o, err := r.c.get(d.Origin, true, nil)
		if err != nil {
			return err
		}
		nvfs.Add("origin", o.GUID)
		e.originGUID = o.GUID
	}
	if r.props || r.recursive || r.backup {
		p, err := r.propsOf(d)
		if err != nil {
			return err
		}
		nvfs.Add("props", p)
	}
	if enc, _ := d.Enum("encryption"); enc != "" && enc != "off" {
		if root, _ := d.String("encryptionroot"); root == d.Name {
			nvfs.Add("is_encroot", nvlist.Flag{})
		}
		if !r.raw {
			return fmt.Errorf("zfs: send: cannot send %s@%s: encrypted dataset %s may not be sent with properties without the raw flag", r.top, r.to, d.Name)
		}
	}

	r.parentFrom = 0
	snapGUIDs, snapProps := nvlist.New(), nvlist.New()
	snapHolds := nvlist.New()
	iter := func(s *Dataset) error {
		name := shortName(s.Name)
		isFrom := r.from != "" && r.from == name
		isTo := r.to == name
		isToNoFrom := isTo && r.from == ""
		if r.toTxg != 0 && s.CreationTxg > r.toTxg {
			return nil
		}
		snapGUIDs.Add(name, s.GUID)
		if isFrom || (r.parentFrom == 0 && isTo) {
			r.parentFrom = s.GUID
		}
		if !r.recursive {
			if r.doall && r.from == "" && !r.seenFrom {
				r.seenFrom = true
			}
			if !r.seenFrom && isFrom {
				r.seenFrom = true
				return nil
			}
			if (r.seenTo || !r.seenFrom) && !isToNoFrom {
				return nil
			}
			if isTo {
				r.seenTo = true
			}
		}
		sp, err := r.propsOf(s)
		if err != nil {
			return err
		}
		snapProps.Add(name, sp)
		if r.holds {
			tags := nvlist.New()
			if hs, err := r.c.Holds(s.Name); err == nil {
				for _, h := range hs {
					tags.Add(h.Tag, uint64(h.Created.Unix()))
				}
			}
			snapHolds.Add(name, tags)
		}
		return nil
	}
	if r.doall || r.recursive || r.to == "" {
		for _, s := range snaps {
			if err := iter(s); err != nil {
				return err
			}
		}
	} else {
		if r.from != "" {
			r.seenFrom = true
		}
		for _, s := range snaps {
			if shortName(s.Name) == r.to {
				if err := iter(s); err != nil {
					return err
				}
			}
		}
	}
	nvfs.Add("snaps", snapGUIDs)
	nvfs.Add("snapprops", snapProps)
	if r.holds {
		nvfs.Add("snapholds", snapHolds)
	}
	e.nv = nvfs
	r.fss.Add(guidKey(d.GUID), nvfs)
	r.entries = append(r.entries, e)
	for _, s := range snapGUIDs.Pairs() {
		if g, ok := s.Value.(uint64); ok {
			if _, dup := r.bySnap[g]; !dup {
				r.bySnap[g] = e
			}
		}
	}

	if r.recursive {
		var kids []*Dataset
		err := r.c.Walk(d.Name, ListOptions{Types: Filesystem | Volume, Depth: 1}, func(k *Dataset) error {
			if k.Name != d.Name {
				kids = append(kids, k)
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, k := range kids {
			if err := r.iterateFS(k); err != nil {
				return err
			}
		}
	}
	return nil
}

var spaceLimits = map[string]bool{"quota": true, "reservation": true, "refquota": true, "refreservation": true}

// propsOf is the properties a header lists for d: the ones in effect, or with
// Backup (send_iterate_prop with received_only) the received ones.
func (r *replSend) propsOf(d *Dataset) (*nvlist.List, error) {
	if !r.backup {
		return sendProps(d), nil
	}
	recvd, err := r.c.receivedProps(d.Name)
	if err != nil {
		return nil, err
	}
	cp := *d
	cp.Props = recvd
	return sendProps(&cp), nil
}

// sendProps collects the properties of a dataset or snapshot that go into the
// header (send_iterate_prop): the ones set on it or received, which are not
// computed by ZFS.
func sendProps(d *Dataset) *nvlist.List {
	out := nvlist.New()
	names := make([]string, 0, len(d.Props))
	for n := range d.Props {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		p := d.Props[name]
		info, ok := LookupProp(name)
		user := ok && info.Kind == KindUser
		if !user && (!ok || info.ReadOnly) {
			continue
		}
		if spaceLimits[name] && d.Type == Snapshot {
			continue
		}
		if p.Source != d.Name && p.Source != "$recvd" {
			continue
		}
		if user || info.Kind == KindString {
			if v, ok := p.Value.(string); ok {
				out.Add(name, v)
			}
		} else if v, ok := p.Value.(uint64); ok {
			out.Add(name, v)
		}
	}
	return out
}

// plan decides which snapshots are sent, in which order (dump_filesystems).
func (r *replSend) plan() ([]sendStep, error) {
	if !r.recursive {
		if len(r.entries) == 0 {
			return nil, errors.New("zfs: send: nothing to send")
		}
		return r.planFS(r.entries[0])
	}
	r.cloneOrigins = map[uint64]bool{}
	for _, e := range r.entries {
		if e.originGUID != 0 {
			r.cloneOrigins[e.originGUID] = true
		}
	}
	var steps []sendStep
	sent := map[*fsEntry]bool{}
	for {
		again, progress := false, false
		for _, e := range r.entries {
			if sent[e] {
				continue
			}
			if e.parentFrom != 0 {
				if p := r.bySnap[e.parentFrom]; p != nil && !sent[p] {
					again = true
					continue
				}
			}
			if e.originGUID != 0 {
				if o := r.bySnap[e.originGUID]; o != nil && !sent[o] {
					again = true
					continue
				}
			}
			s, err := r.planFS(e)
			if err != nil {
				return steps, err
			}
			steps = append(steps, s...)
			sent[e] = true
			progress = true
		}
		if !again {
			break
		}
		if !progress {
			return steps, errors.New("zfs: send: the datasets depend on each other in a cycle")
		}
	}
	return steps, nil
}

func (e *fsEntry) hasSnap(name string) (snapInfo, bool) {
	for _, s := range e.snaps {
		if s.name == name {
			return s, true
		}
	}
	return snapInfo{}, false
}

// planFS is dump_filesystem and dump_snapshot for one dataset.
func (r *replSend) planFS(e *fsEntry) ([]sendStep, error) {
	if _, ok := e.hasSnap(r.to); !ok {
		r.warn("could not send %s@%s: does not exist", e.name, r.to)
		return nil, nil
	}
	missingFrom := false
	if r.recursive && r.from != "" {
		_, ok := e.hasSnap(r.from)
		missingFrom = !ok
	}
	seenFrom, seenTo := r.from == "" || missingFrom, false
	prev := ""
	var steps []sendStep

	process := func(s snapInfo) {
		isFrom := r.from != "" && r.from == s.name
		if !seenFrom && isFrom {
			seenFrom, prev = true, s.name
			return
		}
		if seenTo || !seenFrom {
			return
		}
		isTo := s.name == r.to
		if isTo {
			seenTo = true
		}
		if !r.doall && !isFrom && !isTo {
			// Intermediate snapshots are left out of a replication stream,
			// except the origins of clones that go with it.
			if !r.recursive || !r.cloneOrigins[s.guid] {
				return
			}
		}
		step := sendStep{to: e.name + "@" + s.name}
		switch {
		case prev != "":
			step.from = e.name + "@" + prev
		case r.recursive && e.origin != "":
			step.from = e.origin // a clone starts from its origin
		}
		steps = append(steps, step)
		prev = s.name
	}
	if r.doall || r.recursive {
		for _, s := range e.snaps {
			process(s)
		}
	} else {
		if !seenFrom {
			s, ok := e.hasSnap(r.from)
			if !ok {
				return nil, fmt.Errorf("zfs: send: cannot open %s@%s: %w", e.name, r.from, syscall.ENOENT)
			}
			process(s)
		}
		s, _ := e.hasSnap(r.to)
		process(s)
	}
	switch {
	case !seenFrom:
		r.warn("could not send %s@%s: incremental source (%s@%s) does not exist", e.name, r.to, e.name, r.from)
	case !seenTo && r.from != "":
		r.warn("could not send %s@%s: incremental source (%s@%s) is not earlier than it", e.name, r.to, e.name, r.from)
	case !seenTo:
		r.warn("could not send %s@%s: does not exist", e.name, r.to)
	}
	return steps, nil
}

// features are the feature flags of the compound header. libzfs asks for
// SA_SPILL only when the handle of the snapshot is a filesystem, which it never
// is, so only the holds flag is ever set.
func (r *replSend) features() (StreamFeature, error) {
	var f StreamFeature
	if r.holds {
		f |= FeatureHolds
	}
	return f, nil
}

var holdSeq atomic.Uint64

// ErrSendIncomplete is returned, after the stream was written, when some of
// the datasets or snapshots could not be sent (the warnings of zfs send). The
// stream is complete for the rest.
var ErrSendIncomplete = errors.New("zfs: send: some snapshots could not be sent")

func (c *Client) sendPackage(ctx context.Context, snapshot string, w io.Writer, o *SendOptions) error {
	r, err := c.newReplSend(snapshot, o)
	if err != nil {
		return err
	}
	if err := r.gather(); err != nil {
		return err
	}
	steps, err := r.plan()
	if err != nil {
		return err
	}
	if len(steps) == 0 {
		return fmt.Errorf("zfs: send: %s: nothing to send: %w", snapshot, syscall.ENOENT)
	}
	feat, err := r.features()
	if err != nil {
		return err
	}
	var payload []byte
	if r.wantsHeaderNvlist() {
		hdr := nvlist.New()
		if r.from != "" {
			hdr.Add("fromsnap", r.from)
		}
		hdr.Add("tosnap", r.to)
		if !r.recursive {
			hdr.Add("not_recursive", nvlist.Flag{})
		}
		if r.raw {
			hdr.Add("raw", nvlist.Flag{})
		}
		hdr.Add("fss", r.fss)
		if payload, err = nvlist.PackXDR(hdr); err != nil {
			return err
		}
		if len(payload) > maxPackageNvlist {
			return errors.New("zfs: send: the size of the list of snapshots and properties is too large to be received; select fewer snapshots")
		}
	}

	// Holds keep the snapshots from going away while the stream is written.
	// A read-only pool cannot take holds, and its snapshots cannot go away.
	readOnly, _ := c.PoolReadOnly(r.pool)
	if (r.doall || r.recursive) && !readOnly {
		tag := fmt.Sprintf(".send-%d-%d", os.Getpid(), holdSeq.Add(1))
		seen := map[string]bool{}
		var held []string
		for _, s := range steps {
			for _, n := range []string{s.from, s.to} {
				if n != "" && strings.Contains(n, "@") && !seen[n] {
					seen[n] = true
					held = append(held, n)
				}
			}
		}
		if err := c.Hold(tag, held...); err == nil {
			defer c.Release(tag, held...)
		} else if !errors.Is(err, syscall.EROFS) {
			return err
		}
	}

	var runErr error
	err = c.sink(ctx, w, func(out *os.File) error {
		order := binary.NativeEndian
		if err := writePackageHeader(out, order, r.top+"@"+r.to, feat, payload); err != nil {
			return err
		}
		fd := int(out.Fd())
		for _, s := range steps {
			if err := ctx.Err(); err != nil {
				runErr = err
				break
			}
			step := *o
			step.From, step.Intermediates, step.Replicate, step.Props, step.Holds, step.Backup = s.from, false, false, false, false, false
			stop := c.watchProgress(s.to, fd, &step)
			err := c.SendFD(s.to, fd, &step)
			stop()
			if err != nil {
				runErr = fmt.Errorf("zfs: send %s: %w", s.to, err)
				break
			}
		}
		// The closing record is written also after a failure: what was sent
		// is a valid stream up to there.
		if err := writeEnd(out, order, nil); err != nil && runErr == nil {
			return err
		}
		return runErr
	})
	if err != nil {
		return err
	}
	if len(r.warnings) > 0 {
		return fmt.Errorf("%w: %s", ErrSendIncomplete, strings.Join(r.warnings, "; "))
	}
	return nil
}

// sendPackageSize estimates a replication stream as zfs send -n does: the
// sum of the estimates of its streams, without the header. The estimate is
// high for a clone, whose first stream is counted as a full one.
func (c *Client) sendPackageSize(snapshot string, o *SendOptions) (uint64, error) {
	r, err := c.newReplSend(snapshot, o)
	if err != nil {
		return 0, err
	}
	if err := r.gather(); err != nil {
		return 0, err
	}
	steps, err := r.plan()
	if err != nil {
		return 0, err
	}
	var total uint64
	for _, s := range steps {
		step := *o
		step.From, step.Intermediates, step.Replicate, step.Props, step.Holds, step.Backup = s.from, false, false, false, false, false
		if datasetOf(s.from) != datasetOf(s.to) {
			// The first stream of a clone goes out incremental from its origin,
			// but zfs send -n estimates it as a full stream, and so do we.
			step.From = ""
		}
		n, err := c.SendSize(s.to, &step)
		if err != nil {
			return 0, fmt.Errorf("zfs: send size %s: %w", s.to, err)
		}
		total += n
	}
	return total, nil
}
