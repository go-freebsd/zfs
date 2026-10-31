package primitives

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/go-freebsd/nvlist/pkg/nvlist"
	"github.com/go-freebsd/zfs/pkg/ioctl"
)

// A replication stream (zfs send -R, -p, -I, -h) is made by libzfs, not by the
// kernel: a BEGIN record of type DMU_COMPOUNDSTREAM whose payload is an XDR
// packed nvlist describing the datasets and snapshots, an END record with the
// fletcher4 checksum of both, then one ordinary stream per snapshot, and a
// last, empty END record.

const (
	// maxPackageNvlist bounds the nvlist of a replication header, as libzfs
	// bounds it (libzfs_max_nvlist).
	maxPackageNvlist = 128 << 20

	endRecordChecksumOff = 8 // drr_end.drr_checksum in dmu_replay_record_t
)

// ErrBadPackage is returned for a replication stream whose header is damaged:
// a payload that is not an nvlist, or a checksum that does not match.
var ErrBadPackage = errors.New("zfs: replication stream header is damaged")

// Package is the decoded header of a replication stream.
type Package struct {
	// Header is the compound BEGIN record.
	Header *StreamHeader
	// FromSnap and ToSnap are the short snapshot names the stream was made
	// between; FromSnap is empty for a full stream.
	FromSnap, ToSnap string
	// NotRecursive is set when the stream holds one dataset (-I, -p, -h
	// without -R).
	NotRecursive bool
	// Raw is set for a raw (-w) stream.
	Raw bool
	// FSS describes the datasets by GUID, as libzfs wrote it; nil when the
	// header carries no payload (-I alone).
	FSS *nvlist.List

	bySnap map[uint64]pkgSnap
}

type pkgSnap struct {
	fs   *nvlist.List
	snap string
}

// Recursive reports whether the stream covers a dataset tree.
func (p *Package) Recursive() bool { return !p.NotRecursive }

// find returns the dataset record and snapshot name that hold a snapshot
// GUID (fsavl_find).
func (p *Package) find(guid uint64) (fs *nvlist.List, snap string, ok bool) {
	s, ok := p.bySnap[guid]
	return s.fs, s.snap, ok
}

// Filesystems returns the entries of FSS in the order the stream lists them.
func (p *Package) Filesystems() []*nvlist.List {
	if p.FSS == nil {
		return nil
	}
	var out []*nvlist.List
	for _, pr := range p.FSS.Pairs() {
		if l, ok := pr.Value.(*nvlist.List); ok {
			out = append(out, l)
		}
	}
	return out
}

func (p *Package) index() error {
	p.bySnap = map[uint64]pkgSnap{}
	for _, fs := range p.Filesystems() {
		snaps, ok := fs.List("snaps")
		if !ok {
			return fmt.Errorf("%w: a dataset without a snaps list", ErrBadPackage)
		}
		for _, sp := range snaps.Pairs() {
			g, ok := sp.Value.(uint64)
			if !ok {
				return fmt.Errorf("%w: snapshot %q has no GUID", ErrBadPackage, sp.Name)
			}
			if _, dup := p.bySnap[g]; !dup { // the first one wins, as in libzfs
				p.bySnap[g] = pkgSnap{fs, sp.Name}
			}
		}
	}
	return nil
}

func truncatedErr(name string) error {
	return &ioctl.Error{Ioc: ioctl.IOCRecvNew, Name: name, Errno: ioctl.ErrStreamTruncated}
}

// readPackage reads what follows the compound BEGIN record h: the payload and
// the END record, and checks the checksum.
func readPackage(r io.Reader, h *StreamHeader) (*Package, error) {
	if h.PayloadLen > maxPackageNvlist {
		return nil, fmt.Errorf("%w: the nvlist is too large (%d bytes)", ErrBadPackage, h.PayloadLen)
	}
	if h.PayloadLen%4 != 0 {
		return nil, fmt.Errorf("%w: payload of %d bytes", ErrBadPackage, h.PayloadLen)
	}
	var zc fletcher4
	zc.write(h.order, h.raw[:])
	payload := make([]byte, h.PayloadLen)
	if _, err := io.ReadFull(r, payload); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil, truncatedErr(h.ToName)
		}
		return nil, err
	}
	zc.write(h.order, payload)

	var end [ioctl.ReplayRecordSize]byte
	if _, err := io.ReadFull(r, end[:]); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil, truncatedErr(h.ToName)
		}
		return nil, err
	}
	if h.order.Uint32(end[0:]) != drrEnd {
		return nil, fmt.Errorf("%w: no END record after the header", ErrBadPackage)
	}
	if readCksum(h.order, end[endRecordChecksumOff:]) != zc {
		return nil, fmt.Errorf("%w: incorrect header checksum", ErrBadPackage)
	}

	p := &Package{Header: h}
	if h.PayloadLen == 0 {
		p.bySnap = map[uint64]pkgSnap{}
		return p, nil
	}
	l, err := nvlist.Unpack(payload)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadPackage, err)
	}
	p.FromSnap, _ = l.String("fromsnap")
	p.ToSnap, _ = l.String("tosnap")
	p.NotRecursive = l.Has("not_recursive")
	p.Raw = l.Has("raw")
	fss, ok := l.List("fss")
	if !ok {
		return nil, fmt.Errorf("%w: no fss list", ErrBadPackage)
	}
	p.FSS = fss
	if err := p.index(); err != nil {
		return nil, err
	}
	return p, nil
}

// readHeaderOrEnd reads the next record of a replication stream: a stream's
// BEGIN record, or the empty END record that closes the package (end is then
// true). Either byte order is understood.
func readHeaderOrEnd(r io.Reader) (h *StreamHeader, end bool, err error) {
	var rec [ioctl.ReplayRecordSize]byte
	if _, err := io.ReadFull(r, rec[:]); err != nil {
		if err == io.ErrUnexpectedEOF {
			err = fmt.Errorf("%w: truncated header", ErrNotAStream)
		}
		return nil, false, err
	}
	t := binary.NativeEndian.Uint32(rec[:])
	if t == drrEnd || t == swap32(drrEnd) {
		return nil, true, nil
	}
	h, err = ParseStreamHeader(rec[:])
	return h, false, err
}

func swap32(v uint32) uint32 { return v>>24 | v>>8&0xff00 | v<<8&0xff0000 | v<<24 }

// buildBegin makes the compound BEGIN record of a replication stream, in the
// given byte order.
func buildBegin(order binary.ByteOrder, toName string, features StreamFeature, payloadLen int) ([ioctl.ReplayRecordSize]byte, error) {
	var rec [ioctl.ReplayRecordSize]byte
	if len(toName) >= 256 {
		return rec, fmt.Errorf("zfs: send: name %q is too long", toName)
	}
	order.PutUint32(rec[0:], drrBegin)
	order.PutUint32(rec[4:], uint32(payloadLen))
	order.PutUint64(rec[8:], streamMagic)
	order.PutUint64(rec[16:], hdrCompound|uint64(features)<<2)
	copy(rec[56:], toName)
	return rec, nil
}

// writePackageHeader writes the compound BEGIN record, its payload and the
// END record that carries their checksum (dump_record and
// send_conclusion_record).
func writePackageHeader(w io.Writer, order binary.ByteOrder, toName string, features StreamFeature, payload []byte) error {
	if len(payload)%4 != 0 {
		return fmt.Errorf("zfs: send: header payload of %d bytes", len(payload))
	}
	rec, err := buildBegin(order, toName, features, len(payload))
	if err != nil {
		return err
	}
	var zc fletcher4
	zc.write(order, rec[:])
	zc.write(order, payload)
	if _, err := w.Write(rec[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return writeEnd(w, order, &zc)
}

// writeEnd writes an END record, with the checksum zc or, if nil, an empty one
// (the last record of a replication stream).
func writeEnd(w io.Writer, order binary.ByteOrder, zc *fletcher4) error {
	var rec [ioctl.ReplayRecordSize]byte
	order.PutUint32(rec[0:], drrEnd)
	if zc != nil {
		zc.put(order, rec[endRecordChecksumOff:])
	}
	_, err := w.Write(rec[:])
	return err
}

// guidKey is the name of a dataset in the fss list: "0x<guid in hex>".
func guidKey(guid uint64) string { return "0x" + strconv.FormatUint(guid, 16) }

// parseGUIDKey is the inverse of guidKey.
func parseGUIDKey(s string) (uint64, bool) {
	s, ok := strings.CutPrefix(s, "0x")
	if !ok {
		return 0, false
	}
	g, err := strconv.ParseUint(s, 16, 64)
	return g, err == nil
}
